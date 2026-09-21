package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"gpt-load/internal/affinity"
	"gpt-load/internal/channel"
	"gpt-load/internal/config"
	"gpt-load/internal/encryption"
	"gpt-load/internal/failover"
	"gpt-load/internal/keypool"
	"gpt-load/internal/models"
	"gpt-load/internal/services"
	"gpt-load/internal/store"
	"gpt-load/internal/utils"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

type affinityTransport func(*http.Request) (*http.Response, error)

func (f affinityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type affinityHarness struct {
	server  *ProxyServer
	group   *models.Group
	channel *channel.OpenAIResponseChannel
	store   store.Store
	client  *http.Client
}

func newAffinityHarness(t *testing.T) *affinityHarness {
	t.Helper()
	t.Setenv("OPENAI_AFFINITY_ENABLED", "true")
	t.Setenv("OPENAI_AFFINITY_TTL", "7200")
	t.Setenv("CLAUDE_AFFINITY_ENABLED", "true")
	gin.SetMode(gin.TestMode)
	s := store.NewMemoryStore()
	t.Cleanup(func() { _ = s.Close() })
	crypt, err := encryption.NewService("")
	if err != nil {
		t.Fatal(err)
	}
	sm := config.NewSystemSettingsManager()
	group := &models.Group{ID: 7, Name: "responses", ChannelType: affinity.OpenAIResponseChannelType, EffectiveConfig: utils.DefaultSystemSettings()}
	group.EffectiveConfig.MaxRetries = 3
	group.FailoverStatusCodeMatcher, err = failover.ParseStatusCodeMatcher("400-403,405-999")
	if err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= 2; id++ {
		if err := s.HSet(fmt.Sprintf("key:%d", id), map[string]any{
			"id": id, "group_id": group.ID, "status": models.KeyStatusActive, "key_string": fmt.Sprintf("key-%d", id),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.LPush("group:7:active_keys", 2, 1); err != nil {
		t.Fatal(err)
	}
	upstream, _ := url.Parse("http://upstream.invalid")
	client := &http.Client{Transport: affinityTransport(func(req *http.Request) (*http.Response, error) {
		return affinityHTTPResponse(req, 200, `{"status":"completed"}`), nil
	})}
	ch := &channel.OpenAIResponseChannel{BaseChannel: &channel.BaseChannel{
		Name: affinity.OpenAIResponseChannelType, HTTPClient: client, StreamClient: client,
		Upstreams: []channel.UpstreamInfo{{URL: upstream, Weight: 1}},
	}}
	ps := &ProxyServer{
		keyProvider: keypool.NewProvider(nil, s, sm, crypt), encryptionSvc: crypt,
		affinityProvider: affinity.NewProvider(s), requestLogService: services.NewRequestLogService(nil, s, sm),
	}
	return &affinityHarness{server: ps, group: group, channel: ch, store: s, client: client}
}

func affinityHTTPResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func (h *affinityHarness) request(t *testing.T, method, path, body string) (*httptest.ResponseRecorder, []models.RequestLog) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/proxy/responses"+path, strings.NewReader(body))
	c.Request.Header.Set("Authorization", "Bearer proxy-secret")
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("X-Test-Header", "unchanged")
	c.Params = gin.Params{{Key: "path", Value: path}, {Key: "group_name", Value: h.group.Name}}
	original := []byte(body)
	overridden, err := h.server.applyParamOverrides(original, h.group)
	if err != nil {
		t.Fatal(err)
	}
	h.server.executeRequestWithRetry(c, h.channel, h.group, h.group, overridden,
		h.channel.IsStreamRequest(c, original), time.Now(), 0, "", false, nil)
	keys, err := h.store.SPopN(services.PendingLogKeysSet, 100)
	if err != nil {
		t.Fatal(err)
	}
	logs := make([]models.RequestLog, 0, len(keys))
	for _, key := range keys {
		data, err := h.store.Get(key)
		if err != nil {
			t.Fatal(err)
		}
		var row models.RequestLog
		if err := json.Unmarshal(data, &row); err != nil {
			t.Fatal(err)
		}
		logs = append(logs, row)
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].Timestamp.Before(logs[j].Timestamp) })
	return w, logs
}

func (h *affinityHarness) fingerprint(t *testing.T, body string) string {
	t.Helper()
	f, _ := h.server.affinityProvider.Fingerprinter(h.group.ChannelType)
	fp, ok := f.Compute("model", "/v1/responses", []byte(body))
	if !ok {
		t.Fatal("test body did not match affinity")
	}
	return fp
}

func assertAffinityLog(t *testing.T, row models.RequestLog, status, key string) {
	t.Helper()
	crypt, _ := encryption.NewService("")
	if row.AffinityStatus != status || row.KeyHash != crypt.Hash(key) {
		t.Fatalf("unexpected log: status=%q key=%q, want %q %q", row.AffinityStatus, row.KeyValue, status, key)
	}
}

func TestResponsesAffinityNormalizedHistoryAndLogs(t *testing.T) {
	h := newAffinityHarness(t)
	bodies := []string{
		`{ "model":"model", "instructions":"rules", "input":"help", "tools":[{"type":"function","name":"read"},{"type":"function","name":"write"}] }`,
		`{"model":"model","input":[{"role":"developer","content":[{"type":"input_text","text":"rules"}]},{"role":"user","content":[{"type":"input_text","text":""},{"type":"input_text","text":"help"}]},{"role":"assistant","content":"reply"},{"role":"user","content":"next"}],"tools":[{"name":"write","type":"function"},{"name":"read","type":"function","parameters":{"type":"object"}}],"prompt_cache_key":"different","stream":true}`,
		`{"model":"model","instructions":"rules","input":"different user"}`,
	}
	var received [][]byte
	h.client.Transport = affinityTransport(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		received = append(received, body)
		if req.URL.Path != "/v1/responses" || req.Header.Get("X-Test-Header") != "unchanged" || req.ContentLength != int64(len(body)) {
			t.Fatal("forwarded request metadata changed")
		}
		return affinityHTTPResponse(req, 200, "data: {\"type\":\"response.completed\"}\n\n"), nil
	})
	for i, body := range bodies {
		w, logs := h.request(t, http.MethodPost, "/v1/responses", body)
		if w.Code != 200 || len(logs) != 1 {
			t.Fatalf("request %d: code=%d logs=%v", i, w.Code, logs)
		}
		status, key := affinity.StatusMiss, "key-1"
		if i == 1 {
			status = affinity.StatusHit
		} else if i == 2 {
			key = "key-2"
		}
		assertAffinityLog(t, logs[0], status, key)
		if !bytes.Equal(received[i], []byte(body)) {
			t.Fatal("fingerprinting changed forwarded bytes")
		}
		if logs[0].IsStream != (i == 1) {
			t.Fatal("streaming log flag changed")
		}
	}
}

func TestResponsesAffinityRetryRebindsWithoutRepeatingFailedKey(t *testing.T) {
	h := newAffinityHarness(t)
	body := `{"model":"model","input":"help"}`
	fp := h.fingerprint(t, body)
	if err := h.server.affinityProvider.Record(h.group.ID, fp, 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	var attempts []string
	h.client.Transport = affinityTransport(func(req *http.Request) (*http.Response, error) {
		key := req.Header.Get("Authorization")
		attempts = append(attempts, key)
		if key == "Bearer key-1" {
			// This existing error category does not update the database, so the
			// failed key remains active and first in the rotation for this test.
			return affinityHTTPResponse(req, 429, `{"error":{"message":"resource has been exhausted"}}`), nil
		}
		return affinityHTTPResponse(req, 200, `{"status":"completed"}`), nil
	})
	w, logs := h.request(t, http.MethodPost, "/v1/responses", body)
	if w.Code != 200 || len(attempts) != 2 || attempts[0] != "Bearer key-1" || attempts[1] != "Bearer key-2" || len(logs) != 2 {
		t.Fatalf("retry sequence: status=%d attempts=%v logs=%v", w.Code, attempts, logs)
	}
	assertAffinityLog(t, logs[0], affinity.StatusUnbind, "key-1")
	assertAffinityLog(t, logs[1], affinity.StatusNone, "key-2")
	if logs[0].RequestType != models.RequestTypeRetry || logs[1].RequestType != models.RequestTypeFinal {
		t.Fatal("retry/final log classification incorrect")
	}
	if id, found := h.server.affinityProvider.Lookup(h.group.ID, fp); !found || id != 2 {
		t.Fatalf("expected new binding to key 2, got %d %v", id, found)
	}
}

func TestResponsesAffinityExhaustionReturnsLastError(t *testing.T) {
	h := newAffinityHarness(t)
	h.group.EffectiveConfig.MaxRetries = 10
	body := `{"model":"model","input":"help"}`
	const failure = `{"error":{"message":"resource has been exhausted"}}`
	count := 0
	h.client.Transport = affinityTransport(func(req *http.Request) (*http.Response, error) {
		count++
		return affinityHTTPResponse(req, 429, failure), nil
	})
	w, logs := h.request(t, http.MethodPost, "/v1/responses", body)
	if count != 2 || w.Code != 429 || len(logs) != 2 || logs[1].RequestType != models.RequestTypeFinal {
		t.Fatalf("exhaustion lost final attempt: count=%d status=%d logs=%v", count, w.Code, logs)
	}
	if !strings.Contains(w.Body.String(), "resource has been exhausted") {
		t.Fatal("last upstream error was lost")
	}
	if _, found := h.server.affinityProvider.Lookup(h.group.ID, h.fingerprint(t, body)); found {
		t.Fatal("failed request created a binding")
	}
}

func TestResponsesAffinityNonRetryableErrorDoesNotBind(t *testing.T) {
	h := newAffinityHarness(t)
	body := `{"model":"model","input":"help"}`
	fp := h.fingerprint(t, body)
	h.client.Transport = affinityTransport(func(req *http.Request) (*http.Response, error) {
		return affinityHTTPResponse(req, 404, `{"error":"unknown model"}`), nil
	})
	w, logs := h.request(t, http.MethodPost, "/v1/responses", body)
	if w.Code != 404 || len(logs) != 1 || logs[0].IsSuccess {
		t.Fatalf("unexpected error response: %d %v", w.Code, logs)
	}
	if _, found := h.server.affinityProvider.Lookup(h.group.ID, fp); found {
		t.Fatal("404 created an affinity binding")
	}
	if err := h.server.affinityProvider.Record(h.group.ID, fp, 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	h.request(t, http.MethodPost, "/v1/responses", body)
	if id, found := h.server.affinityProvider.Lookup(h.group.ID, fp); !found || id != 1 {
		t.Fatal("non-retryable error removed existing binding")
	}
}

func TestResponsesAffinityCanceledRequestKeepsBinding(t *testing.T) {
	h := newAffinityHarness(t)
	body := `{"model":"model","input":"help"}`
	fp := h.fingerprint(t, body)
	if err := h.server.affinityProvider.Record(h.group.ID, fp, 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	h.client.Transport = affinityTransport(func(*http.Request) (*http.Response, error) { return nil, context.Canceled })
	_, logs := h.request(t, http.MethodPost, "/v1/responses", body)
	if len(logs) != 1 || logs[0].StatusCode != 499 {
		t.Fatalf("cancellation retried: %v", logs)
	}
	if id, found := h.server.affinityProvider.Lookup(h.group.ID, fp); !found || id != 1 {
		t.Fatal("cancellation removed binding")
	}
}

func TestResponsesAffinityOverridesAndModelRedirect(t *testing.T) {
	h := newAffinityHarness(t)
	h.group.ParamOverrides = datatypes.JSONMap{"instructions": "fixed rules"}
	h.group.ModelRedirectMap = map[string]string{"alias-a": "model", "alias-b": "model"}
	var received []map[string]any
	h.client.Transport = affinityTransport(func(req *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		received = append(received, payload)
		return affinityHTTPResponse(req, 200, `{}`), nil
	})
	for i, body := range []string{
		`{"model":"alias-a","instructions":"client a","input":"help"}`,
		`{"model":"alias-b","instructions":"client b","input":"help"}`,
	} {
		_, logs := h.request(t, http.MethodPost, "/v1/responses", body)
		want := affinity.StatusMiss
		if i == 1 {
			want = affinity.StatusHit
		}
		if len(logs) != 1 || logs[0].AffinityStatus != want || received[i]["model"] != "model" || received[i]["instructions"] != "fixed rules" {
			t.Fatalf("override/redirect mismatch: logs=%v payload=%v", logs, received[i])
		}
	}
	h.group.ModelRedirectMap["alias-b"] = "new-target"
	_, logs := h.request(t, http.MethodPost, "/v1/responses", `{"model":"alias-b","input":"help"}`)
	if len(logs) != 1 || logs[0].AffinityStatus != affinity.StatusMiss {
		t.Fatal("new redirected model reused previous fingerprint")
	}
	h.group.ModelRedirectStrict = true
	w, logs := h.request(t, http.MethodPost, "/v1/responses", `{"model":"unconfigured","input":"help"}`)
	if w.Code != 400 || len(logs) != 1 || logs[0].AffinityStatus != affinity.StatusSkip || len(received) != 3 {
		t.Fatal("strict redirect must reject before forwarding")
	}
}

func TestResponsesAffinityScopeAndStaleKeys(t *testing.T) {
	t.Run("scope", func(t *testing.T) {
		h := newAffinityHarness(t)
		for _, tc := range []struct{ method, path, body, status string }{
			{http.MethodGet, "/v1/responses", `{"model":"model","input":"help"}`, affinity.StatusSkip},
			{http.MethodPost, "/v1/chat/completions", `{"model":"model","input":"help"}`, affinity.StatusSkip},
			{http.MethodPost, "/v1/responses", `{"model":"model","input":"help","previous_response_id":"r1"}`, affinity.StatusSkip},
		} {
			_, logs := h.request(t, tc.method, tc.path, tc.body)
			if len(logs) != 1 || logs[0].AffinityStatus != tc.status {
				t.Fatalf("unexpected scope log: %v", logs)
			}
		}
		t.Setenv("OPENAI_AFFINITY_ENABLED", "false")
		h.server.affinityProvider = affinity.NewProvider(h.store)
		_, logs := h.request(t, http.MethodPost, "/v1/responses", `{"model":"model","input":"help"}`)
		if len(logs) != 1 || logs[0].AffinityStatus != affinity.StatusNone {
			t.Fatal("disabled affinity changed logging")
		}
	})
	for _, kind := range []string{"deleted", "invalid", "moved"} {
		t.Run(kind, func(t *testing.T) {
			h := newAffinityHarness(t)
			body := `{"model":"model","input":"help"}`
			fp := h.fingerprint(t, body)
			if err := h.server.affinityProvider.Record(h.group.ID, fp, 1, time.Hour); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "deleted":
				_ = h.store.Delete("key:1")
			case "invalid":
				_ = h.store.HSet("key:1", map[string]any{"status": models.KeyStatusInvalid})
			case "moved":
				_ = h.store.HSet("key:1", map[string]any{"group_id": 8})
			}
			_, logs := h.request(t, http.MethodPost, "/v1/responses", body)
			if len(logs) != 1 {
				t.Fatalf("unexpected logs: %v", logs)
			}
			assertAffinityLog(t, logs[0], affinity.StatusUnbind, "key-2")
			if id, found := h.server.affinityProvider.Lookup(h.group.ID, fp); !found || id != 2 {
				t.Fatal("stale binding was not replaced")
			}
		})
	}
}

func TestResponsesAffinityRetryBudget(t *testing.T) {
	h := newAffinityHarness(t)
	h.group.EffectiveConfig.MaxRetries = 0
	body := `{"model":"model","input":"help"}`
	fp := h.fingerprint(t, body)
	if err := h.server.affinityProvider.Record(h.group.ID, fp, 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	count := 0
	h.client.Transport = affinityTransport(func(req *http.Request) (*http.Response, error) {
		count++
		return affinityHTTPResponse(req, 429, `{"error":{"message":"resource has been exhausted"}}`), nil
	})
	w, logs := h.request(t, http.MethodPost, "/v1/responses", body)
	if count != 1 || w.Code != 429 || len(logs) != 1 || logs[0].RequestType != models.RequestTypeFinal {
		t.Fatalf("retry budget exceeded: count=%d status=%d logs=%v", count, w.Code, logs)
	}
	assertAffinityLog(t, logs[0], affinity.StatusUnbind, "key-1")
	if _, found := h.server.affinityProvider.Lookup(h.group.ID, fp); found {
		t.Fatal("failed final attempt retained binding")
	}
}

func TestResponsesAffinityHTTP200StreamFailureBoundary(t *testing.T) {
	h := newAffinityHarness(t)
	body := `{"model":"model","input":"help","stream":true}`
	const event = "event: response.failed\ndata: {\"type\":\"response.failed\"}\n\n"
	h.client.Transport = affinityTransport(func(req *http.Request) (*http.Response, error) {
		return affinityHTTPResponse(req, 200, event), nil
	})
	w, logs := h.request(t, http.MethodPost, "/v1/responses", body)
	if w.Body.String() != event || len(logs) != 1 || logs[0].AffinityStatus != affinity.StatusMiss {
		t.Fatal("SSE payload or logging was altered")
	}
	if _, found := h.server.affinityProvider.Lookup(h.group.ID, h.fingerprint(t, body)); !found {
		t.Fatal("HTTP 200 must follow the documented header-level success rule")
	}
}

func TestClaudeAffinityProxyBehaviorPreserved(t *testing.T) {
	h := newAffinityHarness(t)
	h.group.ChannelType = "anthropic"
	// The channel used here shares JSON model extraction and transport behavior
	// with Claude; fingerprint dispatch is determined by the actual group type.
	body := `{"model":"claude-sonnet","system":"rules","messages":[{"role":"user","content":"help"}],"cache_control":{"type":"ephemeral"}}`
	for i := range 2 {
		w, logs := h.request(t, http.MethodPost, "/v1/messages", body)
		if w.Code != 200 || len(logs) != 1 {
			t.Fatalf("Claude request failed: %d %v", w.Code, logs)
		}
		want := affinity.StatusMiss
		if i == 1 {
			want = affinity.StatusHit
		}
		assertAffinityLog(t, logs[0], want, "key-1")
	}
	// Responses' new HTTP-200 restriction must not change Claude bookkeeping.
	h.client.Transport = affinityTransport(func(req *http.Request) (*http.Response, error) {
		return affinityHTTPResponse(req, 404, `{}`), nil
	})
	otherBody := strings.Replace(body, `"help"`, `"other user"`, 1)
	h.request(t, http.MethodPost, "/v1/messages", otherBody)
	f, _ := h.server.affinityProvider.Fingerprinter("anthropic")
	fp, ok := f.Compute("claude-sonnet", "/v1/messages", []byte(otherBody))
	if !ok {
		t.Fatal("Claude fingerprint no longer matches")
	}
	if _, found := h.server.affinityProvider.Lookup(h.group.ID, fp); !found {
		t.Fatal("Claude HTTP bookkeeping changed")
	}
}
