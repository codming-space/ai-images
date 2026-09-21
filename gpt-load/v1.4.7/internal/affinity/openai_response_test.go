package affinity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func responseFingerprint(t *testing.T, body string) string {
	t.Helper()
	data := []byte(body)
	before := bytes.Clone(data)
	f := &OpenAIResponseFingerprinter{}
	fp, ok := f.Compute("upstream-model", openAIResponsePath, data)
	if !ok || !strings.HasPrefix(fp, openAIResponsePrefix) {
		t.Fatalf("expected Responses fingerprint, got %q, %v", fp, ok)
	}
	if !bytes.Equal(data, before) {
		t.Fatal("fingerprinting modified the body")
	}
	return fp
}

func TestOpenAIResponseEquivalentText(t *testing.T) {
	want := responseFingerprint(t, `{"instructions":"规则\n保持原文", "input":"你好\nhelp"}`)
	cases := []string{
		`{"input":"\u4f60\u597d\nhelp","instructions":"规则\n保持原文"}`,
		`{"instructions":"规则\n保持原文","input":[{"role":"user","content":"你好\nhelp"}]}`,
		`{"instructions":"规则\n保持原文","input":[{"type":"message","role":"user","id":"changing","status":"in_progress","content":[{"type":"input_text","text":"你好\nhelp","prompt_cache_breakpoint":{"mode":"explicit"}}]}]}`,
		`{"instructions":"规则","input":[{"role":"developer","content":"保持原文"},{"role":"user","content":[{"type":"input_text","text":""},{"type":"input_text","text":"你好"},{"type":"input_text","text":""},{"type":"input_text","text":"help"},{"type":"input_text","text":""}]}]}`,
		`{"input":[{"role":"system","content":[{"type":"input_text","text":"规则"}]},{"role":"developer","content":""},{"role":"system","content":"保持原文"},{"role":"user","content":"你好\nhelp"}]}`,
		`{"instructions":null,"input":[{"role":"developer","content":[{"type":"input_text","text":"规则"},{"type":"input_text","text":""},{"type":"input_text","text":"保持原文"}]},{"role":"user","content":"你好\nhelp"}]}`,
		`{"instructions":"规则\n保持原文","input":[{"role":"user","content":"你好\nhelp"},{"type":"reasoning","id":"rs_1","encrypted_content":"changing"},{"type":"function_call","call_id":"c1","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"dynamic"},{"role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"role":"developer","content":"later guidance"},{"role":"user","content":[{"type":"input_image","image_url":"https://example.invalid/image"}]}]}`,
	}
	for i, body := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			if got := responseFingerprint(t, body); got != want {
				t.Fatalf("equivalent text drifted: %s != %s", got, want)
			}
		})
	}
}

func TestOpenAIResponseEmptyInstructionsAndTools(t *testing.T) {
	want := responseFingerprint(t, `{"input":"help"}`)
	for _, instructions := range []string{"", `,"instructions":null`, `,"instructions":""`} {
		for _, tools := range []string{"", `,"tools":null`, `,"tools":[]`} {
			body := `{"input":[{"role":"system","content":[]},{"role":"developer","content":[{"type":"input_text","text":""}]},{"role":"user","content":"help"}]` + instructions + tools + `}`
			if got := responseFingerprint(t, body); got != want {
				t.Fatalf("empty representation drifted: %s", body)
			}
		}
	}
}

func TestOpenAIResponseToolNormalization(t *testing.T) {
	want := responseFingerprint(t, `{"input":"help","tools":[{"type":"function","name":"read","description":"Read files","parameters":{"type":"object"},"strict":true},{"type":"web_search"},{"name":"read","description":"Other description","type":"function"}]}`)
	body := `{"input":"help","tools":[{"description":"Other description","type":"function","name":"read","parameters":null},{"type":"web_search","name":null,"description":42,"search_context_size":"high"},{"description":"Read files","name":"read","type":"function","strict":false,"parameters":{"properties":{"n":{"const":9007199254740993}},"type":"object"}}]}`
	if got := responseFingerprint(t, body); got != want {
		t.Fatal("tool ordering, schema or optional settings changed the fingerprint")
	}
	empty := responseFingerprint(t, `{"input":"help","tools":[{}]}`)
	for _, field := range []string{"name", "description", "type"} {
		for _, value := range []string{`""`, `null`, `123`, `false`, `{}`, `[]`} {
			body := fmt.Sprintf(`{"input":"help","tools":[{%q:%s}]}`, field, value)
			if responseFingerprint(t, body) != empty {
				t.Fatalf("empty tool field drifted: %s", body)
			}
		}
	}
}

func TestOpenAIResponseIgnoresRuntimeParameters(t *testing.T) {
	want := responseFingerprint(t, `{"model":"alias-a","instructions":"rules","input":"help"}`)
	body := `{
		"model":"alias-b", "instructions":"rules", "input":"help",
		"stream":true, "store":false, "background":false,
		"previous_response_id":null, "conversation":null, "prompt":null,
		"reasoning":{"effort":"high","context":"all_turns"},
		"text":{"verbosity":"high","format":{"type":"json_schema","schema":{"type":"object"}}},
		"prompt_cache_key":"changing", "prompt_cache_retention":"24h",
		"prompt_cache_options":{"ttl":"30m","mode":"explicit"},
		"tool_choice":"none", "parallel_tool_calls":false, "truncation":"auto",
		"context_management":[{"type":"compaction","compact_threshold":1000}],
		"temperature":0.2, "top_p":0.8, "max_output_tokens":100,
		"metadata":{"id":"random"}, "user":"random", "safety_identifier":"random",
		"service_tier":"priority", "include":["reasoning.encrypted_content"]
	}`
	if got := responseFingerprint(t, body); got != want {
		t.Fatal("runtime parameters or pre-redirect model changed the fingerprint")
	}
}

func TestOpenAIResponseDistinguishesContent(t *testing.T) {
	base := `{"instructions":"rules","input":"help","tools":[{"type":"function","name":"read","description":"Read files"}]}`
	want := responseFingerprint(t, base)
	cases := []string{
		strings.Replace(base, `"rules"`, `"other rules"`, 1),
		strings.Replace(base, `"help"`, `"other input"`, 1),
		strings.Replace(base, `"help"`, `" help "`, 1),
		strings.Replace(base, `"help"`, `"help\n"`, 1),
		strings.Replace(base, `"read"`, `"write"`, 1),
		strings.Replace(base, `"Read files"`, `"Read other files"`, 1),
		strings.Replace(base, `"function"`, `"custom"`, 1),
		`{"instructions":"rules","input":"help","tools":[]}`,
		`{"instructions":"rules","input":"help","tools":[{"type":"function","name":"read","description":"Read files"},{"type":"function","name":"read","description":"Read files"}]}`,
	}
	for i, body := range cases {
		if responseFingerprint(t, body) == want {
			t.Errorf("content change %d did not change fingerprint", i)
		}
	}
	f := &OpenAIResponseFingerprinter{}
	if got, ok := f.Compute("different-model", openAIResponsePath, []byte(base)); !ok || got == want {
		t.Fatal("effective model did not distinguish the fingerprint")
	}
	// A newline/control character in one field cannot move into another field.
	for _, pair := range [][2]string{
		{`{"instructions":"a\nb","input":"c"}`, `{"instructions":"a","input":"b\nc"}`},
		{`{"input":"help","tools":[{"name":"a\u001fb","description":"c"}]}`, `{"input":"help","tools":[{"name":"a","description":"b\u001fc"}]}`},
	} {
		if responseFingerprint(t, pair[0]) == responseFingerprint(t, pair[1]) {
			t.Fatal("field boundaries collided")
		}
	}
}

func TestOpenAIResponseSkipsUnsupportedRequests(t *testing.T) {
	cases := []string{
		``, `null`, `[]`, `{}`, `{"input":"help"} {}`, `{"input":`,
		`{"instructions":[],"input":"help"}`, `{"instructions":5,"input":"help"}`,
		`{"input":"help","previous_response_id":"resp_1"}`, `{"input":"help","previous_response_id":""}`,
		`{"input":"help","previous_response_id":false}`, `{"input":"help","conversation":{}}`,
		`{"input":"help","conversation":"conv_1"}`, `{"input":"help","prompt":{"id":"p1"}}`,
		`{"input":"help","background":true}`, `{"input":"help","background":"false"}`,
		`{"input":""}`, `{"input":null}`, `{"input":[]}`, `{"input":{}}`, `{"input":42}`,
		`{"input":[{"role":"developer","content":"rules"}]}`,
		`{"input":[{"role":"assistant","content":"answer"},{"role":"user","content":"help"}]}`,
		`{"input":[{"type":"function_call_output","output":"result"},{"role":"user","content":"help"}]}`,
		`{"input":[{"type":"reasoning"},{"role":"user","content":"help"}]}`,
		`{"input":[{"type":"compaction"},{"role":"user","content":"help"}]}`,
		`{"input":[{"type":"item_reference","id":"msg1"},{"role":"user","content":"help"}]}`,
		`{"input":[{"role":"user","content":"help"},{"type":"item_reference","id":"msg1"}]}`,
		`{"input":[{"role":"user","content":"help"},5]}`,
		`{"input":[null,{"role":"user","content":"help"}]}`,
		`{"input":[{"type":"unknown","role":"user","content":"help"}]}`,
		`{"input":[{"content":"help"}]}`,
		`{"input":[{"role":"user"}]}`, `{"input":[{"role":"user","content":null}]}`,
		`{"input":[{"role":"user","content":42}]}`, `{"input":[{"role":"user","content":[]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_text","text":""}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_text"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_text","text":null}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_text","text":42}]}]}`,
		`{"input":[{"role":"user","content":[null]}]}`,
		`{"input":[{"role":"user","content":[{"type":"text","text":"help"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_text","text":"help"},{"type":"input_image","image_url":"x"}]}]}`,
		`{"input":[{"role":"system","content":[{"type":"input_file","file_id":"file1"}]},{"role":"user","content":"help"}]}`,
		`{"input":"help","tools":{}}`, `{"input":"help","tools":"invalid"}`,
		`{"input":"help","tools":[null]}`, `{"input":"help","tools":[1]}`,
	}
	f := &OpenAIResponseFingerprinter{}
	for i, body := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			if fp, ok := f.Compute("model", openAIResponsePath, []byte(body)); ok || fp != "" {
				t.Fatalf("unsupported request was fingerprinted: %s", body)
			}
		})
	}
	for _, path := range []string{"/responses", "/v1/responses/", "/v1/responses/compact", "/v1/chat/completions"} {
		if _, ok := f.Compute("model", path, []byte(`{"input":"help"}`)); ok {
			t.Errorf("unexpected path matched: %s", path)
		}
	}
	if _, ok := f.Compute("", openAIResponsePath, []byte(`{"input":"help"}`)); ok {
		t.Fatal("empty effective model matched")
	}
}

func TestOpenAIResponseConfiguration(t *testing.T) {
	t.Setenv("CLAUDE_AFFINITY_ENABLED", "true")
	t.Setenv("CLAUDE_AFFINITY_TTL", "300")
	t.Setenv("OPENAI_AFFINITY_ENABLED", "")
	t.Setenv("OPENAI_AFFINITY_TTL", "")
	if f := newOpenAIResponseFingerprinter(); f.Enabled() || f.TTL() != 2*time.Hour {
		t.Fatalf("unexpected defaults: enabled=%v ttl=%v", f.Enabled(), f.TTL())
	}
	t.Setenv("OPENAI_AFFINITY_ENABLED", "true")
	for _, value := range []string{"", "invalid", "0", "-1", "9223372037", "9223372036854775808"} {
		t.Setenv("OPENAI_AFFINITY_TTL", value)
		if f := newOpenAIResponseFingerprinter(); !f.Enabled() || f.TTL() != 2*time.Hour {
			t.Errorf("invalid/default TTL %q was not 7200 seconds", value)
		}
	}
	t.Setenv("OPENAI_AFFINITY_TTL", "900")
	if newOpenAIResponseFingerprinter().TTL() != 15*time.Minute {
		t.Fatal("valid custom TTL ignored")
	}
}

func BenchmarkOpenAIResponseCompute(b *testing.B) {
	for _, turns := range []int{1, 50, 250} {
		b.Run(fmt.Sprintf("turns_%d", turns), func(b *testing.B) {
			input := []any{map[string]any{"role": "user", "content": "Initial request"}}
			for range turns {
				input = append(input, map[string]any{"role": "assistant", "content": strings.Repeat("history ", 128)})
			}
			body, err := json.Marshal(map[string]any{
				"instructions": strings.Repeat("Rules. ", 256), "input": input,
				"tools": []any{map[string]any{"name": "read", "type": "function", "description": "Read files", "parameters": map[string]any{"description": strings.Repeat("schema ", 256)}}},
			})
			if err != nil {
				b.Fatal(err)
			}
			f := &OpenAIResponseFingerprinter{}
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for b.Loop() {
				if _, ok := f.Compute("model", openAIResponsePath, body); !ok {
					b.Fatal("benchmark request skipped")
				}
			}
		})
	}
}
