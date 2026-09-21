package affinity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpt-load/internal/utils"

	"github.com/sirupsen/logrus"
)

// Both OpenAI-compatible channels can forward requests to /v1/responses.
const (
	OpenAIChannelType         = "openai"
	OpenAIResponseChannelType = "openai-response"
)

const (
	openAIResponsePath       = "/v1/responses"
	openAIResponseDefaultTTL = 7200 * time.Second
	openAIResponsePrefix     = "openai-responses-v1:"
)

// IsOpenAIResponseRoute checks the actual upstream path captured by Gin's
// /*path parameter. The group name and /proxy prefix do not belong to it.
func IsOpenAIResponseRoute(channelType, path string) bool {
	return (channelType == OpenAIChannelType || channelType == OpenAIResponseChannelType) && path == openAIResponsePath
}

// OpenAIResponseFingerprinter groups stateless requests by their initial text
// and tool identities. It does not reproduce the upstream prompt cache key.
type OpenAIResponseFingerprinter struct {
	enabled bool
	ttl     time.Duration
}

func newOpenAIResponseFingerprinter() *OpenAIResponseFingerprinter {
	ttl := openAIResponseDefaultTTL
	if value := os.Getenv("OPENAI_AFFINITY_TTL"); value != "" {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || seconds <= 0 || seconds > int64((1<<63-1)/time.Second) {
			logrus.Warn("affinity: invalid OPENAI_AFFINITY_TTL, using 7200 seconds")
		} else {
			ttl = time.Duration(seconds) * time.Second
		}
	}
	return &OpenAIResponseFingerprinter{
		enabled: utils.ParseBoolean(os.Getenv("OPENAI_AFFINITY_ENABLED"), false),
		ttl:     ttl,
	}
}

func (f *OpenAIResponseFingerprinter) Enabled() bool      { return f.enabled }
func (f *OpenAIResponseFingerprinter) TTL() time.Duration { return f.ttl }

// Compute reads the body after parameter overrides. The caller supplies the
// effective model after redirection and checks the HTTP method separately.
func (f *OpenAIResponseFingerprinter) Compute(model, path string, body []byte) (string, bool) {
	if model == "" || path != openAIResponsePath {
		return "", false
	}
	var req struct {
		Instructions       json.RawMessage `json:"instructions"`
		Input              json.RawMessage `json:"input"`
		Tools              json.RawMessage `json:"tools"`
		PreviousResponseID json.RawMessage `json:"previous_response_id"`
		Conversation       json.RawMessage `json:"conversation"`
		Prompt             json.RawMessage `json:"prompt"`
		Background         bool            `json:"background"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return skipOpenAIResponse("invalid_body")
	}
	if rawIsPresent(req.PreviousResponseID) || rawIsPresent(req.Conversation) {
		return skipOpenAIResponse("stateful_request")
	}
	if rawIsPresent(req.Prompt) || req.Background {
		return skipOpenAIResponse("unsupported_request")
	}

	var instructions string
	if rawIsPresent(req.Instructions) {
		if err := json.Unmarshal(req.Instructions, &instructions); err != nil {
			return skipOpenAIResponse("invalid_instructions")
		}
	}
	system, firstUser, ok := openAIResponseInput(instructions, req.Input)
	if !ok {
		return skipOpenAIResponse("unsupported_prefix")
	}
	if firstUser == "" {
		return skipOpenAIResponse("empty_first_user")
	}
	tools, ok := openAIResponseTools(req.Tools)
	if !ok {
		return skipOpenAIResponse("invalid_tools")
	}

	// JSON supplies unambiguous field boundaries, including for embedded control
	// characters. Only these four normalized values enter the hash.
	encoded, err := json.Marshal([4]any{model, system, tools, firstUser})
	if err != nil {
		return skipOpenAIResponse("encoding_error")
	}
	digest := sha256.Sum256(encoded)
	return openAIResponsePrefix + hex.EncodeToString(digest[:]), true
}

func skipOpenAIResponse(reason string) (string, bool) {
	logrus.WithField("reason", reason).Debug("affinity: Responses request skipped")
	return "", false
}

// openAIResponseInput reads only the initial system/developer messages and the
// first user message. Later items are checked for references, but never hashed.
func openAIResponseInput(instructions string, raw json.RawMessage) (string, string, bool) {
	input := bytes.TrimSpace(raw)
	if len(input) == 0 {
		return "", "", false
	}
	if input[0] == '"' {
		var text string
		if err := json.Unmarshal(input, &text); err != nil {
			return "", "", false
		}
		return instructions, text, true
	}
	if input[0] != '[' {
		return "", "", false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return "", "", false
	}
	systemParts := make([]string, 0, 1)
	if instructions != "" {
		systemParts = append(systemParts, instructions)
	}
	var firstUser string
	foundUser := false
	for _, rawItem := range items {
		// Inspect references even after the selected prefix. Full inline output
		// items are allowed; references to stored upstream items are not.
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(rawItem, &kind); err != nil || kind.Type == "item_reference" {
			return "", "", false
		}
		if foundUser {
			continue
		}
		if kind.Type != "" && kind.Type != "message" {
			return "", "", false
		}
		var message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(rawItem, &message); err != nil {
			return "", "", false
		}
		text, ok := openAIResponseText(message.Content)
		if !ok {
			return "", "", false
		}
		switch message.Role {
		case "system", "developer":
			if text != "" {
				systemParts = append(systemParts, text)
			}
		case "user":
			firstUser, foundUser = text, true
		default:
			return "", "", false
		}
	}
	return strings.Join(systemParts, "\n"), firstUser, foundUser
}

// openAIResponseText accepts equivalent string and input_text representations.
// Empty blocks do not introduce separators; text whitespace is preserved.
func openAIResponseText(raw json.RawMessage) (string, bool) {
	content := bytes.TrimSpace(raw)
	if len(content) == 0 {
		return "", false
	}
	if content[0] == '"' {
		var text string
		err := json.Unmarshal(content, &text)
		return text, err == nil
	}
	if content[0] != '[' {
		return "", false
	}
	var blocks []struct {
		Type string          `json:"type"`
		Text json.RawMessage `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return "", false
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		textBytes := bytes.TrimSpace(block.Text)
		if block.Type != "input_text" || len(textBytes) == 0 || textBytes[0] != '"' {
			return "", false
		}
		var text string
		if err := json.Unmarshal(textBytes, &text); err != nil {
			return "", false
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n"), true
}

func openAIResponseTools(raw json.RawMessage) ([][3]string, bool) {
	tools, err := decodeRawBlocks(raw)
	if err != nil {
		return nil, false
	}
	// Always encode an empty list as [], including absent/null tools.
	triples := make([][3]string, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			return nil, false
		}
		triples = append(triples, [3]string{
			decodeJSONString(tool["name"]),
			decodeJSONString(tool["description"]),
			decodeJSONString(tool["type"]),
		})
	}
	sort.Slice(triples, func(i, j int) bool {
		if triples[i][0] != triples[j][0] {
			return triples[i][0] < triples[j][0]
		}
		if triples[i][2] != triples[j][2] {
			return triples[i][2] < triples[j][2]
		}
		return triples[i][1] < triples[j][1]
	})
	return triples, true
}
