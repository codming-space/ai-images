package affinity

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gpt-load/internal/store"
)

// realisticBody mimics a Claude CLI request: a moderately long system prompt,
// a few tools with non-trivial schemas, and a multi-turn message history with
// an explicit cache_control breakpoint. Adjust `userTurns` to vary body size.
func realisticBody(userTurns int) []byte {
	var sb strings.Builder
	sb.WriteString(`{
        "model": "claude-sonnet-4-5",
        "system": [
            {"type": "text", "text": "You are an expert software engineer. Follow these guidelines: be concise, use the project's conventions, prefer existing utilities, and never invent APIs. When writing code, match the surrounding style. Avoid unnecessary comments. Keep diffs minimal. Use the Read tool before editing any file you have not already inspected.", "cache_control": {"type": "ephemeral"}}
        ],
        "tools": [
            {"name": "read_file", "description": "Read a file from disk", "input_schema": {"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["path"]}},
            {"name": "write_file", "description": "Write a file to disk", "input_schema": {"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}},
            {"name": "bash", "description": "Run a shell command", "input_schema": {"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"integer"}},"required":["command"]}}
        ],
        "messages": [`)
	for i := 0; i < userTurns; i++ {
		if i > 0 {
			sb.WriteString(`,`)
		}
		sb.WriteString(fmt.Sprintf(`{"role":"user","content":[{"type":"text","text":"Turn %d: please help me with a task that requires reasoning about the codebase. Context: line %d of file foo.go has an issue."}]}`, i, i*42))
		sb.WriteString(fmt.Sprintf(`,{"role":"assistant","content":[{"type":"text","text":"Sure, let me investigate turn %d in detail. The code at line %d appears to handle the case where..."}]}`, i, i*42))
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

func BenchmarkClaudeCompute_Small(b *testing.B) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}
	body := realisticBody(1)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Compute("claude-sonnet-4-5", "/v1/messages", body)
	}
}

func BenchmarkClaudeCompute_Medium(b *testing.B) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}
	body := realisticBody(10)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Compute("claude-sonnet-4-5", "/v1/messages", body)
	}
}

func BenchmarkClaudeCompute_Large(b *testing.B) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}
	body := realisticBody(50)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Compute("claude-sonnet-4-5", "/v1/messages", body)
	}
}

// Compute on a request that won't match — exercises the early-exit cost.
func BenchmarkClaudeCompute_NoMatch_NoCacheControl(b *testing.B) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}
	body := []byte(`{"model":"claude-sonnet-4-5","system":"hi","messages":[{"role":"user","content":"hello"}]}`)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Compute("claude-sonnet-4-5", "/v1/messages", body)
	}
}

func BenchmarkClaudeCompute_NoMatch_WrongModel(b *testing.B) {
	f := &ClaudeFingerprinter{enabled: true, ttl: time.Hour}
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Compute("gpt-4o", "/v1/messages", body)
	}
}

func BenchmarkStoreProvider_LookupMiss(b *testing.B) {
	s := store.NewMemoryStore()
	p := NewProvider(s)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Lookup(7, "fp-not-here")
	}
}

func BenchmarkStoreProvider_LookupHit(b *testing.B) {
	s := store.NewMemoryStore()
	p := NewProvider(s)
	_ = p.Record(7, "fp", 42, time.Hour)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Lookup(7, "fp")
	}
}

func BenchmarkStoreProvider_RecordSETNX(b *testing.B) {
	s := store.NewMemoryStore()
	p := NewProvider(s)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.Record(7, "fp", 42, time.Hour)
	}
}
