// Package proxy provides high-performance OpenAI multi-key proxy server
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"gpt-load/internal/affinity"
	"gpt-load/internal/channel"
	"gpt-load/internal/config"
	"gpt-load/internal/encryption"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/keypool"
	"gpt-load/internal/models"
	"gpt-load/internal/response"
	"gpt-load/internal/services"
	"gpt-load/internal/store"
	"gpt-load/internal/utils"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// ProxyServer represents the proxy server
type ProxyServer struct {
	keyProvider       *keypool.KeyProvider
	groupManager      *services.GroupManager
	subGroupManager   *services.SubGroupManager
	settingsManager   *config.SystemSettingsManager
	channelFactory    *channel.Factory
	requestLogService *services.RequestLogService
	encryptionSvc     encryption.Service
	affinityProvider  affinity.Provider
}

// NewProxyServer creates a new proxy server
func NewProxyServer(
	keyProvider *keypool.KeyProvider,
	groupManager *services.GroupManager,
	subGroupManager *services.SubGroupManager,
	settingsManager *config.SystemSettingsManager,
	channelFactory *channel.Factory,
	requestLogService *services.RequestLogService,
	encryptionSvc encryption.Service,
	affinityProvider affinity.Provider,
) (*ProxyServer, error) {
	return &ProxyServer{
		keyProvider:       keyProvider,
		groupManager:      groupManager,
		subGroupManager:   subGroupManager,
		settingsManager:   settingsManager,
		channelFactory:    channelFactory,
		requestLogService: requestLogService,
		encryptionSvc:     encryptionSvc,
		affinityProvider:  affinityProvider,
	}, nil
}

// HandleProxy is the main entry point for proxy requests, refactored based on the stable .bak logic.
func (ps *ProxyServer) HandleProxy(c *gin.Context) {
	startTime := time.Now()
	groupName := c.Param("group_name")

	originalGroup, err := ps.groupManager.GetGroupByName(groupName)
	if err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	// Select sub-group if this is an aggregate group
	subGroupName, err := ps.subGroupManager.SelectSubGroup(originalGroup)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"aggregate_group": originalGroup.Name,
			"error":           err,
		}).Error("Failed to select sub-group from aggregate")
		response.Error(c, app_errors.NewAPIError(app_errors.ErrNoKeysAvailable, "No available sub-groups"))
		return
	}

	group := originalGroup
	if subGroupName != "" {
		group, err = ps.groupManager.GetGroupByName(subGroupName)
		if err != nil {
			response.Error(c, app_errors.ParseDBError(err))
			return
		}
	}

	channelHandler, err := ps.channelFactory.GetChannel(group)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInternalServer, fmt.Sprintf("Failed to get channel for group '%s': %v", groupName, err)))
		return
	}

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logrus.Errorf("Failed to read request body: %v", err)
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, "Failed to read request body"))
		return
	}
	c.Request.Body.Close()

	finalBodyBytes, err := ps.applyParamOverrides(bodyBytes, group)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInternalServer, fmt.Sprintf("Failed to apply parameter overrides: %v", err)))
		return
	}

	isStream := channelHandler.IsStreamRequest(c, bodyBytes)

	ps.executeRequestWithRetry(c, channelHandler, originalGroup, group, finalBodyBytes, isStream, startTime, 0, "", false)
}

// executeRequestWithRetry is the core recursive function for handling requests and retries.
//
// affinityFP and affinityHit are threaded through retries so that the final
// outcome can either persist the fp->key mapping (on success) or clear a
// poisoned mapping (on final failure). They are computed once at retryCount==0
// and never recomputed during retries.
func (ps *ProxyServer) executeRequestWithRetry(
	c *gin.Context,
	channelHandler channel.ChannelProxy,
	originalGroup *models.Group,
	group *models.Group,
	bodyBytes []byte,
	isStream bool,
	startTime time.Time,
	retryCount int,
	affinityFP string,
	affinityHit bool,
) {
	cfg := group.EffectiveConfig

	var apiKey *models.APIKey
	var err error
	// attemptStatus tracks how affinity influenced *this* attempt, for logging.
	// It starts empty (no affinity engagement for retries) and is set at
	// retryCount==0 from tryAffinityKey, then possibly upgraded to "unbind"
	// when a hit-bound key fails on this attempt.
	attemptStatus := affinity.StatusNone

	// First attempt: try affinity. On retries we deliberately skip affinity so
	// a failed key isn't picked again — the parent call already passes the
	// affinityFP forward so we can still record/clear on the final outcome.
	if retryCount == 0 {
		newFP, hit, key, status := ps.tryAffinityKey(c, channelHandler, group, bodyBytes)
		affinityFP = newFP
		affinityHit = hit
		apiKey = key
		attemptStatus = status
	}

	if apiKey == nil {
		apiKey, err = ps.keyProvider.SelectKey(group.ID)
		if err != nil {
			logrus.Errorf("Failed to select a key for group %s on attempt %d: %v", group.Name, retryCount+1, err)
			response.Error(c, app_errors.NewAPIError(app_errors.ErrNoKeysAvailable, err.Error()))
			ps.logRequest(c, originalGroup, group, nil, startTime, http.StatusServiceUnavailable, err, isStream, "", channelHandler, bodyBytes, models.RequestTypeFinal, attemptStatus)
			return
		}
	}

	upstreamURL, err := channelHandler.BuildUpstreamURL(c.Request.URL, originalGroup.Name)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInternalServer, fmt.Sprintf("Failed to build upstream URL: %v", err)))
		return
	}

	var ctx context.Context
	var cancel context.CancelFunc
	if isStream {
		ctx, cancel = context.WithCancel(c.Request.Context())
	} else {
		timeout := time.Duration(cfg.RequestTimeout) * time.Second
		ctx, cancel = context.WithTimeout(c.Request.Context(), timeout)
	}
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, c.Request.Method, upstreamURL, bytes.NewReader(bodyBytes))
	if err != nil {
		logrus.Errorf("Failed to create upstream request: %v", err)
		response.Error(c, app_errors.ErrInternalServer)
		return
	}
	req.ContentLength = int64(len(bodyBytes))

	req.Header = c.Request.Header.Clone()

	// Clean up client auth key
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	req.Header.Del("X-Goog-Api-Key")

	// Apply model redirection
	finalBodyBytes, err := channelHandler.ApplyModelRedirect(req, bodyBytes, group)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, err.Error()))
		ps.logRequest(c, originalGroup, group, apiKey, startTime, http.StatusBadRequest, err, isStream, upstreamURL, channelHandler, bodyBytes, models.RequestTypeFinal, attemptStatus)
		return
	}

	// Update request body if it was modified by redirection
	if !bytes.Equal(finalBodyBytes, bodyBytes) {
		req.Body = io.NopCloser(bytes.NewReader(finalBodyBytes))
		req.ContentLength = int64(len(finalBodyBytes))
	}

	channelHandler.ModifyRequest(req, apiKey, group)

	// Apply custom header rules
	if len(group.HeaderRuleList) > 0 {
		headerCtx := utils.NewHeaderVariableContextFromGin(c, group, apiKey)
		utils.ApplyHeaderRules(req, group.HeaderRuleList, headerCtx)
	}

	var client *http.Client
	if isStream {
		client = channelHandler.GetStreamClient()
		req.Header.Set("X-Accel-Buffering", "no")
	} else {
		client = channelHandler.GetHTTPClient()
	}

	resp, err := client.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}

	// Unified error handling for retries.
	// Retry policy is fully defined by group.FailoverStatusCodeMatcher (derived from EffectiveConfig).
	shouldRetryByStatus := resp != nil && shouldFailoverOnStatusCode(resp.StatusCode, group)
	if err != nil || shouldRetryByStatus {
		if err != nil && app_errors.IsIgnorableError(err) {
			logrus.Debugf("Client-side ignorable error for key %s, aborting retries: %v", utils.MaskAPIKey(apiKey.KeyValue), err)
			ps.logRequest(c, originalGroup, group, apiKey, startTime, 499, err, isStream, upstreamURL, channelHandler, bodyBytes, models.RequestTypeFinal, attemptStatus)
			return
		}

		var statusCode int
		var errorMessage string
		var parsedError string

		if err != nil {
			statusCode = 500
			errorMessage = err.Error()
			parsedError = errorMessage
			logrus.Debugf("Request failed (attempt %d/%d) for key %s: %v", retryCount+1, cfg.MaxRetries, utils.MaskAPIKey(apiKey.KeyValue), err)
		} else {
			// Retryable upstream response (HTTP status code matched failover policy)
			statusCode = resp.StatusCode
			errorBody, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				logrus.Errorf("Failed to read error body: %v", readErr)
				errorBody = []byte("Failed to read error body")
			}

			errorBody = handleGzipCompression(resp, errorBody)
			errorMessage = string(errorBody)
			parsedError = app_errors.ParseUpstreamError(errorBody)
			logrus.Debugf("Request failed with status %d (attempt %d/%d) for key %s. Parsed Error: %s", statusCode, retryCount+1, cfg.MaxRetries, utils.MaskAPIKey(apiKey.KeyValue), parsedError)
		}

		// 使用解析后的错误信息更新密钥状态
		ps.keyProvider.UpdateStatus(apiKey, group, false, parsedError)

		// If we just failed on an affinity-bound key, drop the binding
		// immediately (don't wait for isLastAttempt). Retries pick a fresh
		// key via plain round-robin; if the retry chain ultimately succeeds,
		// the Record(SETNX) below will establish a new binding to the
		// key that actually worked. Without this, SETNX would no-op on the
		// stale mapping and future requests with the same fp would keep
		// hitting the same failing key until it gets blacklisted.
		if affinityHit {
			if err := ps.affinityProvider.Delete(group.ID, affinityFP); err != nil {
				logrus.WithError(err).Debug("affinity: delete after hit failure failed (non-fatal)")
			}
			affinityHit = false
			attemptStatus = affinity.StatusUnbind
		}

		// 判断是否为最后一次尝试
		isLastAttempt := retryCount >= cfg.MaxRetries
		requestType := models.RequestTypeRetry
		if isLastAttempt {
			requestType = models.RequestTypeFinal
		}

		ps.logRequest(c, originalGroup, group, apiKey, startTime, statusCode, errors.New(parsedError), isStream, upstreamURL, channelHandler, bodyBytes, requestType, attemptStatus)

		// 如果是最后一次尝试，直接返回错误，不再递归
		if isLastAttempt {
			var errorJSON map[string]any
			if err := json.Unmarshal([]byte(errorMessage), &errorJSON); err == nil {
				c.JSON(statusCode, errorJSON)
			} else {
				response.Error(c, app_errors.NewAPIErrorWithUpstream(statusCode, "UPSTREAM_ERROR", errorMessage))
			}
			return
		}

		ps.executeRequestWithRetry(c, channelHandler, originalGroup, group, bodyBytes, isStream, startTime, retryCount+1, affinityFP, affinityHit)
		return
	}

	// ps.keyProvider.UpdateStatus(apiKey, group, true) // 请求成功不再重置成功次数，减少IO消耗
	logrus.Debugf("Request for group %s succeeded on attempt %d with key %s", group.Name, retryCount+1, utils.MaskAPIKey(apiKey.KeyValue))

	// Affinity bookkeeping on success: SETNX so we don't churn an existing
	// mapping; a no-op when fp was already bound to a key (including this one).
	if affinityFP != "" {
		if err := ps.affinityProvider.Record(group.ID, affinityFP, apiKey.ID, ps.affinityTTL(group.ChannelType)); err != nil {
			logrus.WithError(err).Debug("affinity: record failed (non-fatal)")
		}
	}

	// Check if this is a model list request (needs special handling)
	if shouldInterceptModelList(c.Request.URL.Path, c.Request.Method) {
		ps.handleModelListResponse(c, resp, group, channelHandler)
	} else {
		for key, values := range resp.Header {
			for _, value := range values {
				c.Header(key, value)
			}
		}
		c.Status(resp.StatusCode)

		if isStream {
			ps.handleStreamingResponse(c, resp)
		} else {
			ps.handleNormalResponse(c, resp)
		}
	}

	ps.logRequest(c, originalGroup, group, apiKey, startTime, resp.StatusCode, nil, isStream, upstreamURL, channelHandler, bodyBytes, models.RequestTypeFinal, attemptStatus)
}

// tryAffinityKey attempts to resolve an affinity-bound key for the current
// request. Returns (fp, hit, key, status):
//   - fp == "": the request does not qualify for affinity (no fingerprinter,
//     disabled, model/path mismatch, empty first_user_text, etc.). The caller
//     should fall through to plain round-robin. status is affinity.StatusNone.
//   - fp != "", key != nil: a previously bound key was found and is still
//     active. hit is true, status is affinity.StatusHit.
//   - fp != "", key == nil: the request qualifies but either has no binding
//     yet (status=StatusMiss), or had a binding pointing to a now-invalid key
//     in which case the stale mapping has been deleted (status=StatusUnbind).
//     The caller should fall through to round-robin; on success the fp will
//     be recorded against the picked key.
func (ps *ProxyServer) tryAffinityKey(
	c *gin.Context,
	channelHandler channel.ChannelProxy,
	group *models.Group,
	bodyBytes []byte,
) (string, bool, *models.APIKey, string) {
	if ps.affinityProvider == nil {
		return "", false, nil, affinity.StatusNone
	}
	fp, ok := ps.affinityProvider.Fingerprinter(group.ChannelType)
	if !ok || !fp.Enabled() {
		return "", false, nil, affinity.StatusNone
	}
	model := channelHandler.ExtractModel(c, bodyBytes)
	// Use c.Param("path") rather than c.Request.URL.Path: the proxy route is
	// "/proxy/:group_name/*path", so URL.Path is "/proxy/claude/v1/messages"
	// while the fingerprinter expects the upstream path "/v1/messages".
	fingerprint, matched := fp.Compute(model, c.Param("path"), bodyBytes)
	if !matched {
		return "", false, nil, affinity.StatusNone
	}

	keyID, found := ps.affinityProvider.Lookup(group.ID, fingerprint)
	if !found {
		return fingerprint, false, nil, affinity.StatusMiss
	}

	key, err := ps.keyProvider.GetKeyByID(keyID)
	if err != nil || key == nil || key.Status != models.KeyStatusActive || key.GroupID != group.ID {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			logrus.WithError(err).Debug("affinity: GetKeyByID failed, treating as miss")
		}
		// Stale binding: clear so the next request can rebind to a fresh key.
		if delErr := ps.affinityProvider.Delete(group.ID, fingerprint); delErr != nil {
			logrus.WithError(delErr).Debug("affinity: delete stale mapping failed (non-fatal)")
		}
		return fingerprint, false, nil, affinity.StatusUnbind
	}

	logrus.Debugf("affinity hit: group=%s fp=%s key=%s", group.Name, fingerprint, utils.MaskAPIKey(key.KeyValue))
	return fingerprint, true, key, affinity.StatusHit
}

// affinityTTL returns the Fingerprinter's configured TTL for the given channel
// type, or a zero duration if no fingerprinter is registered (in which case
// callers wouldn't have a non-empty fp anyway).
func (ps *ProxyServer) affinityTTL(channelType string) time.Duration {
	if ps.affinityProvider == nil {
		return 0
	}
	fp, ok := ps.affinityProvider.Fingerprinter(channelType)
	if !ok {
		return 0
	}
	return fp.TTL()
}

func shouldFailoverOnStatusCode(statusCode int, group *models.Group) bool {
	if group == nil {
		return false
	}
	return group.FailoverStatusCodeMatcher.Match(statusCode)
}

// logRequest is a helper function to create and record a request log.
func (ps *ProxyServer) logRequest(
	c *gin.Context,
	originalGroup *models.Group,
	group *models.Group,
	apiKey *models.APIKey,
	startTime time.Time,
	statusCode int,
	finalError error,
	isStream bool,
	upstreamAddr string,
	channelHandler channel.ChannelProxy,
	bodyBytes []byte,
	requestType string,
	affinityStatus string,
) {
	if ps.requestLogService == nil {
		return
	}

	var requestBodyToLog, userAgent string

	if group.EffectiveConfig.EnableRequestBodyLogging {
		requestBodyToLog = utils.TruncateString(string(bodyBytes), 65000)
		userAgent = c.Request.UserAgent()
	}

	duration := time.Since(startTime).Milliseconds()

	logEntry := &models.RequestLog{
		GroupID:        group.ID,
		GroupName:      group.Name,
		IsSuccess:      finalError == nil && statusCode < 400,
		SourceIP:       c.ClientIP(),
		StatusCode:     statusCode,
		RequestPath:    utils.TruncateString(c.Request.URL.String(), 500),
		Duration:       duration,
		UserAgent:      userAgent,
		RequestType:    requestType,
		IsStream:       isStream,
		UpstreamAddr:   utils.TruncateString(upstreamAddr, 500),
		RequestBody:    requestBodyToLog,
		AffinityStatus: affinityStatus,
	}

	// Set parent group
	if originalGroup != nil && originalGroup.GroupType == "aggregate" && originalGroup.ID != group.ID {
		logEntry.ParentGroupID = originalGroup.ID
		logEntry.ParentGroupName = originalGroup.Name
	}

	if channelHandler != nil && bodyBytes != nil {
		logEntry.Model = channelHandler.ExtractModel(c, bodyBytes)
	}

	if apiKey != nil {
		// 加密密钥值用于日志存储
		encryptedKeyValue, err := ps.encryptionSvc.Encrypt(apiKey.KeyValue)
		if err != nil {
			logrus.WithError(err).Error("Failed to encrypt key value for logging")
			logEntry.KeyValue = "failed-to-encryption"
		} else {
			logEntry.KeyValue = encryptedKeyValue
		}
		// 添加 KeyHash 用于反查
		logEntry.KeyHash = ps.encryptionSvc.Hash(apiKey.KeyValue)
	}

	if finalError != nil {
		logEntry.ErrorMessage = finalError.Error()
	}

	if err := ps.requestLogService.Record(logEntry); err != nil {
		logrus.Errorf("Failed to record request log: %v", err)
	}
}
