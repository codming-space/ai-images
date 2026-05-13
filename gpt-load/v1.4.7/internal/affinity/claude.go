package affinity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gpt-load/internal/utils"
)

const (
	// claudeChannelType matches Group.ChannelType for Anthropic Claude.
	claudeChannelType = "anthropic"
	// claudePathExact is the only path eligible for affinity (Anthropic Messages API).
	claudePathExact = "/v1/messages"
	// claudeDefaultTTLSeconds is the default TTL when CLAUDE_AFFINITY_TTL is unset.
	claudeDefaultTTLSeconds = 3600
)

var claudeModelRegex = regexp.MustCompile(`^claude-.*$`)

// ClaudeFingerprinter implements Fingerprinter for Anthropic Claude requests.
//
// Fingerprint inputs: model + normalized_system + canonical_tools + first_user_text.
// cache_control fields are stripped during normalization so that adding or
// removing a cache breakpoint does not change the fingerprint.
type ClaudeFingerprinter struct {
	enabled bool
	ttl     time.Duration
}

func newClaudeFingerprinter() *ClaudeFingerprinter {
	return &ClaudeFingerprinter{
		enabled: utils.ParseBoolean(os.Getenv("CLAUDE_AFFINITY_ENABLED"), false),
		ttl:     time.Duration(utils.ParseInteger(os.Getenv("CLAUDE_AFFINITY_TTL"), claudeDefaultTTLSeconds)) * time.Second,
	}
}

func (c *ClaudeFingerprinter) Enabled() bool      { return c.enabled }
func (c *ClaudeFingerprinter) TTL() time.Duration { return c.ttl }

// Compute returns (fp, true) when the request qualifies for affinity, or
// ("", false) otherwise. A request qualifies only when: model matches
// ^claude-.*$, path equals /v1/messages, body is valid JSON, the request
// actually opted into prompt caching (via a cache_control field somewhere),
// and the first user message contains at least one text block (see plan:
// empty first_user is intentionally not affinitized to avoid cross-session
// collapse onto a single key).
func (c *ClaudeFingerprinter) Compute(model, path string, body []byte) (string, bool) {
	if !claudeModelRegex.MatchString(model) {
		return "", false
	}
	if path != claudePathExact {
		return "", false
	}

	// Affinity only pays off when the request actually uses prompt caching.
	// Without any cache_control there is nothing on the server side to
	// preserve, so we let the request fall back to plain round-robin.
	// (hasCacheControl also acts as our JSON validity check — it returns
	// false on unmarshal error, so an invalid body just bails out here.)
	if !hasCacheControl(body) {
		return "", false
	}

	var req struct {
		System   json.RawMessage `json:"system"`
		Tools    json.RawMessage `json:"tools"`
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", false
	}

	system := normalizedSystem(req.System)
	tools := canonicalTools(req.Tools)
	firstUser := firstUserText(req.Messages)

	if firstUser == "" {
		return "", false
	}

	return computeClaudeKey(model, system, tools, firstUser), true
}

// hasCacheControl reports whether the request opts into prompt caching, in
// any of the four locations recognized by the Anthropic API:
//
//   - Top-level cache_control field (automatic caching mode)
//   - system[].cache_control (explicit breakpoint on a system block)
//   - tools[].cache_control (explicit breakpoint on a tool)
//   - messages[].content[].cache_control (explicit breakpoint on a content block)
//
// Returns true on the first hit so we don't pay for scanning the whole body
// when caching is clearly enabled. Field present but set to null counts as
// "not enabled" (matches how the API treats it).
func hasCacheControl(body []byte) bool {
	var top struct {
		CacheControl json.RawMessage   `json:"cache_control"`
		System       json.RawMessage   `json:"system"`
		Tools        json.RawMessage   `json:"tools"`
		Messages     []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return false
	}

	if rawIsPresent(top.CacheControl) {
		return true
	}
	if blocksHaveCacheControl(top.System) {
		return true
	}
	if blocksHaveCacheControl(top.Tools) {
		return true
	}
	for _, msg := range top.Messages {
		var m struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(msg, &m); err != nil {
			continue
		}
		if blocksHaveCacheControl(m.Content) {
			return true
		}
	}
	return false
}

func blocksHaveCacheControl(raw json.RawMessage) bool {
	blocks, err := decodeRawBlocks(raw)
	if err != nil {
		return false
	}
	for _, blk := range blocks {
		if rawIsPresent(blk["cache_control"]) {
			return true
		}
	}
	return false
}

func rawIsPresent(raw json.RawMessage) bool {
	s := bytes.TrimSpace(raw)
	return len(s) > 0 && !bytes.Equal(s, []byte("null"))
}

// normalizedSystem reduces the `system` field (string or []block) to a stable
// plain text. Only blocks with type=text are included; cache_control fields
// are dropped, so adding/removing a cache breakpoint never shifts the hash.
func normalizedSystem(raw json.RawMessage) string {
	s := bytes.TrimSpace(raw)
	if len(s) == 0 {
		return ""
	}
	switch s[0] {
	case '"':
		var str string
		if err := json.Unmarshal(s, &str); err != nil {
			return ""
		}
		return str
	case '[':
		blocks, err := decodeRawBlocks(s)
		if err != nil {
			return ""
		}
		return joinTextBlocks(blocks)
	}
	return ""
}

type toolDef struct {
	name        string
	description string
	schema      string
}

// canonicalTools reduces the tools array to a deterministic representation:
// sorted by name, cache_control stripped. Tools form part of the "session
// identity" because changing them invalidates the entire upstream prompt cache.
func canonicalTools(raw json.RawMessage) string {
	tools, err := decodeRawBlocks(raw)
	if err != nil {
		return ""
	}
	defs := make([]toolDef, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, toolDef{
			name:        decodeJSONString(t["name"]),
			description: decodeJSONString(t["description"]),
			schema:      string(bytes.TrimSpace(t["input_schema"])),
		})
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].name < defs[j].name })

	var sb strings.Builder
	for _, d := range defs {
		sb.WriteString(d.name)
		sb.WriteByte(0x1f)
		sb.WriteString(d.description)
		sb.WriteByte(0x1f)
		sb.WriteString(d.schema)
		sb.WriteByte(0x1e)
	}
	return sb.String()
}

// firstUserText returns the concatenated text from the first role=user message.
// If content is a string it is returned verbatim; if it is an array, only
// type=text blocks are joined with '\n'. image / tool_result blocks are ignored.
func firstUserText(raw json.RawMessage) string {
	msgs, err := decodeRawBlocks(raw)
	if err != nil {
		return ""
	}
	for _, msg := range msgs {
		if decodeJSONString(msg["role"]) != "user" {
			continue
		}
		content := bytes.TrimSpace(msg["content"])
		if len(content) == 0 {
			return ""
		}
		switch content[0] {
		case '"':
			var s string
			if err := json.Unmarshal(content, &s); err != nil {
				return ""
			}
			return s
		case '[':
			blocks, err := decodeRawBlocks(content)
			if err != nil {
				return ""
			}
			return joinTextBlocks(blocks)
		}
		return ""
	}
	return ""
}

// decodeRawBlocks parses a JSON array into a slice of {field -> raw bytes} maps.
// Returns (nil, nil) for empty input — callers treat that as an empty array.
func decodeRawBlocks(raw json.RawMessage) ([]map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var out []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func joinTextBlocks(blocks []map[string]json.RawMessage) string {
	var sb strings.Builder
	for _, blk := range blocks {
		if decodeJSONString(blk["type"]) != "text" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(decodeJSONString(blk["text"]))
	}
	return sb.String()
}

// computeClaudeKey hashes the normalized fields with explicit delimiters. The
// "strict" tier label is part of the input so we can later add other tiers
// without colliding with existing fingerprints.
func computeClaudeKey(model, system, tools, firstUser string) string {
	h := sha256.New()
	writeKV(h, "tier", "strict")
	writeKV(h, "model", model)
	writeKV(h, "system", system)
	writeKV(h, "tools", tools)
	writeKV(h, "first_user", firstUser)
	return hex.EncodeToString(h.Sum(nil))
}

func writeKV(w io.Writer, key, value string) {
	_, _ = w.Write([]byte(key))
	_, _ = w.Write([]byte{0x00})
	_, _ = w.Write([]byte(value))
	_, _ = w.Write([]byte{0x01})
}
