package affinity

import (
	"strings"
	"testing"
	"time"
)

// withCC injects a top-level cache_control field into a JSON body so the
// fingerprinter's "must opt into prompt caching" gate doesn't block existing
// tests that focus on normalization logic. Tests targeting the cache_control
// gate itself build their bodies explicitly without this helper.
func withCC(body string) []byte {
	return []byte(strings.Replace(body, "{", `{"cache_control":{"type":"ephemeral"},`, 1))
}

func TestClaude_Compute_QualifiesOnText(t *testing.T) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}
	body := withCC(`{
        "model": "claude-sonnet-4-5",
        "system": "you are helpful",
        "messages": [{"role": "user", "content": "hello"}]
    }`)
	fp, ok := f.Compute("claude-sonnet-4-5", "/v1/messages", body)
	if !ok {
		t.Fatal("expected matched=true for plain text user message")
	}
	if fp == "" {
		t.Fatal("expected non-empty fp")
	}
}

func TestClaude_Compute_RejectsNonClaudeModel(t *testing.T) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}
	body := withCC(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if _, ok := f.Compute("gpt-4o", "/v1/messages", body); ok {
		t.Fatal("expected matched=false for non-claude model")
	}
}

func TestClaude_Compute_RejectsWrongPath(t *testing.T) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}
	body := withCC(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	if _, ok := f.Compute("claude-sonnet-4-5", "/v1/chat/completions", body); ok {
		t.Fatal("expected matched=false for wrong path")
	}
}

func TestClaude_Compute_RejectsInvalidJSON(t *testing.T) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}
	if _, ok := f.Compute("claude-sonnet-4-5", "/v1/messages", []byte("not json")); ok {
		t.Fatal("expected matched=false for invalid json")
	}
}

func TestClaude_Compute_EmptyFirstUserDoesNotMatch(t *testing.T) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}
	cases := []struct {
		name string
		body string
	}{
		{
			name: "empty messages array",
			body: `{"model":"claude-sonnet-4-5","messages":[]}`,
		},
		{
			name: "first message is assistant",
			body: `{"model":"claude-sonnet-4-5","messages":[{"role":"assistant","content":"hi"}]}`,
		},
		{
			name: "user content has only image",
			body: `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"..."}}]}]}`,
		},
		{
			name: "user content has only tool_result",
			body: `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"y"}]}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := f.Compute("claude-sonnet-4-5", "/v1/messages", withCC(tc.body)); ok {
				t.Fatalf("expected matched=false for %s", tc.name)
			}
		})
	}
}

// Adding or removing cache_control on system / tools / messages must not
// change the fingerprint — that's the whole point of stripping it during
// normalization.
func TestClaude_Fingerprint_StableAcrossCacheControl(t *testing.T) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}

	// Both versions must opt into caching for Compute to run; the difference
	// is *where* the cache_control sits, which must not change the fp.
	rootOnly := []byte(`{
        "cache_control": {"type": "ephemeral"},
        "model": "claude-sonnet-4-5",
        "system": [
            {"type": "text", "text": "you are a helpful assistant"}
        ],
        "tools": [
            {"name": "get_weather", "description": "Get weather", "input_schema": {"type":"object"}}
        ],
        "messages": [{"role": "user", "content": "what's the weather?"}]
    }`)
	allBlocks := []byte(`{
        "model": "claude-sonnet-4-5",
        "system": [
            {"type": "text", "text": "you are a helpful assistant", "cache_control": {"type": "ephemeral"}}
        ],
        "tools": [
            {"name": "get_weather", "description": "Get weather", "input_schema": {"type":"object"}, "cache_control": {"type": "ephemeral"}}
        ],
        "messages": [{"role": "user", "content": [{"type": "text", "text": "what's the weather?", "cache_control": {"type": "ephemeral"}}]}]
    }`)

	fp1, ok1 := f.Compute("claude-sonnet-4-5", "/v1/messages", rootOnly)
	fp2, ok2 := f.Compute("claude-sonnet-4-5", "/v1/messages", allBlocks)
	if !ok1 || !ok2 {
		t.Fatalf("expected both to match: ok1=%v ok2=%v", ok1, ok2)
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprint changed depending on where cache_control sits:\n  fp1=%s\n  fp2=%s", fp1, fp2)
	}
}

// Tools must be canonicalized — same set in different array order should
// produce identical fingerprints.
func TestClaude_Fingerprint_ToolsOrderIndependent(t *testing.T) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}

	orderA := withCC(`{
        "model": "claude-sonnet-4-5",
        "tools": [
            {"name": "alpha", "description": "A", "input_schema": {"type":"object"}},
            {"name": "beta", "description": "B", "input_schema": {"type":"object"}}
        ],
        "messages": [{"role": "user", "content": "hi"}]
    }`)
	orderB := withCC(`{
        "model": "claude-sonnet-4-5",
        "tools": [
            {"name": "beta", "description": "B", "input_schema": {"type":"object"}},
            {"name": "alpha", "description": "A", "input_schema": {"type":"object"}}
        ],
        "messages": [{"role": "user", "content": "hi"}]
    }`)

	fp1, _ := f.Compute("claude-sonnet-4-5", "/v1/messages", orderA)
	fp2, _ := f.Compute("claude-sonnet-4-5", "/v1/messages", orderB)
	if fp1 != fp2 {
		t.Fatalf("fingerprint depends on tools array order:\n  A=%s\n  B=%s", fp1, fp2)
	}
}

// Changing the actual content (system / tools / first_user) must produce a
// different fingerprint — otherwise affinity would group unrelated requests.
func TestClaude_Fingerprint_DifferentContent(t *testing.T) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}
	base := withCC(`{"model":"claude-sonnet-4-5","system":"a","messages":[{"role":"user","content":"hi"}]}`)
	diffs := map[string]string{
		"different model":      `{"model":"claude-opus-4-7","system":"a","messages":[{"role":"user","content":"hi"}]}`,
		"different system":     `{"model":"claude-sonnet-4-5","system":"b","messages":[{"role":"user","content":"hi"}]}`,
		"different first_user": `{"model":"claude-sonnet-4-5","system":"a","messages":[{"role":"user","content":"bye"}]}`,
		"extra tool":           `{"model":"claude-sonnet-4-5","system":"a","tools":[{"name":"x","description":"","input_schema":{}}],"messages":[{"role":"user","content":"hi"}]}`,
	}
	baseFP, _ := f.Compute("claude-sonnet-4-5", "/v1/messages", base)
	for name, body := range diffs {
		t.Run(name, func(t *testing.T) {
			model := "claude-sonnet-4-5"
			if name == "different model" {
				model = "claude-opus-4-7"
			}
			fp, ok := f.Compute(model, "/v1/messages", withCC(body))
			if !ok {
				t.Fatalf("expected matched=true for %s", name)
			}
			if fp == baseFP {
				t.Fatalf("fingerprint did not change for %s", name)
			}
		})
	}
}

// Array-form system with empty description tool — ensures empty fields don't
// crash and produce a stable fingerprint.
func TestClaude_Fingerprint_EmptyFieldsAreStable(t *testing.T) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}
	body := withCC(`{
        "model": "claude-sonnet-4-5",
        "system": [],
        "tools": [{"name": "noop", "input_schema": {}}],
        "messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]
    }`)
	fp1, ok1 := f.Compute("claude-sonnet-4-5", "/v1/messages", body)
	fp2, ok2 := f.Compute("claude-sonnet-4-5", "/v1/messages", body)
	if !ok1 || !ok2 {
		t.Fatal("expected matched=true")
	}
	if fp1 != fp2 {
		t.Fatal("repeat call must produce identical fp")
	}
}

// Various "empty" forms of system / tools must (a) not crash, (b) still
// produce a fingerprint when first_user_text is non-empty, and (c) collapse
// to the same fingerprint regardless of how the emptiness is expressed
// (missing field, null, "", []).
func TestClaude_Fingerprint_EmptySystemAndToolsRobustness(t *testing.T) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}

	cases := []struct {
		name string
		body string
	}{
		{"both missing", `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`},
		{"system null, tools missing", `{"model":"claude-sonnet-4-5","system":null,"messages":[{"role":"user","content":"hi"}]}`},
		{"system empty string, tools missing", `{"model":"claude-sonnet-4-5","system":"","messages":[{"role":"user","content":"hi"}]}`},
		{"system empty array, tools missing", `{"model":"claude-sonnet-4-5","system":[],"messages":[{"role":"user","content":"hi"}]}`},
		{"system missing, tools null", `{"model":"claude-sonnet-4-5","tools":null,"messages":[{"role":"user","content":"hi"}]}`},
		{"system missing, tools empty array", `{"model":"claude-sonnet-4-5","tools":[],"messages":[{"role":"user","content":"hi"}]}`},
		{"both null", `{"model":"claude-sonnet-4-5","system":null,"tools":null,"messages":[{"role":"user","content":"hi"}]}`},
		{"both empty", `{"model":"claude-sonnet-4-5","system":"","tools":[],"messages":[{"role":"user","content":"hi"}]}`},
	}

	var canonical string
	for i, tc := range cases {
		fp, ok := f.Compute("claude-sonnet-4-5", "/v1/messages", withCC(tc.body))
		if !ok {
			t.Fatalf("[%s] expected matched=true with non-empty first_user", tc.name)
		}
		if i == 0 {
			canonical = fp
			continue
		}
		if fp != canonical {
			t.Fatalf("[%s] fingerprint differs from canonical empty form:\n  got=%s\n  want=%s", tc.name, fp, canonical)
		}
	}
}

// Non-empty system / tools must produce a different fingerprint from the
// all-empty form — guards against accidentally collapsing real content into
// the "everything empty" bucket.
func TestClaude_Fingerprint_NonEmptyDiffersFromEmpty(t *testing.T) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}

	emptyFP, _ := f.Compute("claude-sonnet-4-5", "/v1/messages",
		withCC(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`))

	withSystem, _ := f.Compute("claude-sonnet-4-5", "/v1/messages",
		withCC(`{"model":"claude-sonnet-4-5","system":"x","messages":[{"role":"user","content":"hi"}]}`))
	if emptyFP == withSystem {
		t.Fatal("adding system must change fp")
	}

	withTools, _ := f.Compute("claude-sonnet-4-5", "/v1/messages",
		withCC(`{"model":"claude-sonnet-4-5","tools":[{"name":"x","input_schema":{}}],"messages":[{"role":"user","content":"hi"}]}`))
	if emptyFP == withTools {
		t.Fatal("adding tools must change fp")
	}
}

// Server tools (computer use, bash, text editor, web search) carry a `type`
// field that distinguishes versions — e.g. computer_20241022 vs
// computer_20250124 share name="computer" but are different upstream tools
// and trigger cache invalidation when swapped. The fingerprint must reflect
// the type so a version bump rebinds to a fresh key rather than silently
// reusing the old binding.
func TestClaude_Fingerprint_ServerToolVersionDiffers(t *testing.T) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}

	v1 := withCC(`{
        "model": "claude-sonnet-4-5",
        "tools": [{"type":"computer_20241022","name":"computer"}],
        "messages": [{"role":"user","content":"hi"}]
    }`)
	v2 := withCC(`{
        "model": "claude-sonnet-4-5",
        "tools": [{"type":"computer_20250124","name":"computer"}],
        "messages": [{"role":"user","content":"hi"}]
    }`)

	fp1, ok1 := f.Compute("claude-sonnet-4-5", "/v1/messages", v1)
	fp2, ok2 := f.Compute("claude-sonnet-4-5", "/v1/messages", v2)
	if !ok1 || !ok2 {
		t.Fatalf("expected both to match: ok1=%v ok2=%v", ok1, ok2)
	}
	if fp1 == fp2 {
		t.Fatal("server tool version bump must change fp (computer_20241022 vs computer_20250124)")
	}
}

// input_schema is intentionally not part of the fingerprint, because its
// raw-byte representation depends on the client's JSON serializer. Two
// requests sharing (name, description, type) but differing in input_schema
// must collide on the same affinity key. The trade-off is documented in
// canonicalTools: we route similar requests to the same key and let the
// upstream prompt cache decide actual hit/miss on byte equality.
func TestClaude_Fingerprint_IgnoresInputSchema(t *testing.T) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}

	schemaA := withCC(`{
        "model": "claude-sonnet-4-5",
        "tools": [{"name":"x","description":"d","input_schema":{"type":"object","properties":{"a":{"type":"string"}}}}],
        "messages": [{"role":"user","content":"hi"}]
    }`)
	schemaB := withCC(`{
        "model": "claude-sonnet-4-5",
        "tools": [{"name":"x","description":"d","input_schema":{"type":"object","properties":{"b":{"type":"integer"}}}}],
        "messages": [{"role":"user","content":"hi"}]
    }`)

	fp1, _ := f.Compute("claude-sonnet-4-5", "/v1/messages", schemaA)
	fp2, _ := f.Compute("claude-sonnet-4-5", "/v1/messages", schemaB)
	if fp1 != fp2 {
		t.Fatalf("input_schema must not affect fp:\n  fp1=%s\n  fp2=%s", fp1, fp2)
	}
}

// Each of (name, description, type) is read via decodeJSONString, which
// returns "" for: missing key, JSON null, empty string, and any non-string
// type. All these forms must collapse to the same fingerprint so that a
// tool spelled `{}` or `{"name":""}` or `{"name":null,"type":null}` are
// indistinguishable — otherwise small client-side serialization differences
// would fragment the affinity bucket.
func TestClaude_Fingerprint_ToolFieldEmptyForms(t *testing.T) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}

	wrap := func(toolJSON string) []byte {
		return withCC(`{
            "model": "claude-sonnet-4-5",
            "tools": [` + toolJSON + `],
            "messages": [{"role":"user","content":"hi"}]
        }`)
	}

	cases := []struct {
		name string
		tool string
	}{
		{"empty object", `{}`},
		{"all keys null", `{"name":null,"description":null,"type":null}`},
		{"all keys empty string", `{"name":"","description":"","type":""}`},
		{"name only, empty string", `{"name":""}`},
		{"non-string description", `{"description":123}`},
		{"non-string type", `{"type":{"nested":"object"}}`},
	}

	var canonical string
	for i, tc := range cases {
		fp, ok := f.Compute("claude-sonnet-4-5", "/v1/messages", wrap(tc.tool))
		if !ok {
			t.Fatalf("[%s] expected matched=true", tc.name)
		}
		if i == 0 {
			canonical = fp
			continue
		}
		if fp != canonical {
			t.Fatalf("[%s] fp differs from canonical empty form:\n  got=%s\n  want=%s", tc.name, fp, canonical)
		}
	}
}

// Only requests that opt into prompt caching (somewhere) qualify for
// affinity. Without any cache_control there is no upstream cache to preserve,
// so we want the request to fall back to pure round-robin.
func TestClaude_Compute_RequiresCacheControl(t *testing.T) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}

	// 1) No cache_control anywhere — must NOT match.
	noCC := []byte(`{
        "model": "claude-sonnet-4-5",
        "system": "hello",
        "messages": [{"role": "user", "content": "hi"}]
    }`)
	if _, ok := f.Compute("claude-sonnet-4-5", "/v1/messages", noCC); ok {
		t.Fatal("expected matched=false when no cache_control is set")
	}

	// 2) cache_control: null in every position — must NOT match.
	nullCC := []byte(`{
        "cache_control": null,
        "model": "claude-sonnet-4-5",
        "system": [{"type":"text","text":"x","cache_control":null}],
        "tools": [{"name":"t","input_schema":{},"cache_control":null}],
        "messages": [{"role":"user","content":[{"type":"text","text":"hi","cache_control":null}]}]
    }`)
	if _, ok := f.Compute("claude-sonnet-4-5", "/v1/messages", nullCC); ok {
		t.Fatal("expected matched=false when cache_control is null everywhere")
	}

	// 3) Each of the four legitimate locations alone must enable affinity.
	enablingLocations := map[string]string{
		"top-level (automatic caching)": `{
            "cache_control": {"type": "ephemeral"},
            "model": "claude-sonnet-4-5",
            "messages": [{"role": "user", "content": "hi"}]
        }`,
		"system block": `{
            "model": "claude-sonnet-4-5",
            "system": [{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}],
            "messages": [{"role": "user", "content": "hi"}]
        }`,
		"tool definition": `{
            "model": "claude-sonnet-4-5",
            "tools": [{"name":"t","input_schema":{},"cache_control":{"type":"ephemeral"}}],
            "messages": [{"role": "user", "content": "hi"}]
        }`,
		"message content block": `{
            "model": "claude-sonnet-4-5",
            "messages": [{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]
        }`,
	}
	for name, body := range enablingLocations {
		t.Run(name, func(t *testing.T) {
			if _, ok := f.Compute("claude-sonnet-4-5", "/v1/messages", []byte(body)); !ok {
				t.Fatalf("expected matched=true when cache_control is set on %s", name)
			}
		})
	}
}
