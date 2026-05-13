package affinity

import (
	"testing"
	"time"

	"gpt-load/internal/store"
)

func TestStoreProvider_RecordLookupDelete(t *testing.T) {
	s := store.NewMemoryStore()
	t.Cleanup(func() { _ = s.Close() })

	p := NewProvider(s)
	const (
		gid uint = 7
		fp       = "fp-abc"
	)

	if _, ok := p.Lookup(gid, fp); ok {
		t.Fatal("expected miss before any Record")
	}

	if err := p.Record(gid, fp, 42, time.Minute); err != nil {
		t.Fatalf("Record failed: %v", err)
	}
	id, ok := p.Lookup(gid, fp)
	if !ok || id != 42 {
		t.Fatalf("expected (42, true), got (%d, %v)", id, ok)
	}

	if err := p.Delete(gid, fp); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, ok := p.Lookup(gid, fp); ok {
		t.Fatal("expected miss after Delete")
	}
}

// SETNX semantics: an existing mapping must not be overwritten by a later
// Record. This avoids concurrent-write thrash when multiple requests with the
// same fingerprint race past the Lookup miss.
func TestStoreProvider_RecordIsSetNX(t *testing.T) {
	s := store.NewMemoryStore()
	t.Cleanup(func() { _ = s.Close() })

	p := NewProvider(s)
	const (
		gid uint = 1
		fp       = "fp"
	)
	if err := p.Record(gid, fp, 100, time.Minute); err != nil {
		t.Fatal(err)
	}
	// Second Record should be a no-op.
	if err := p.Record(gid, fp, 200, time.Minute); err != nil {
		t.Fatal(err)
	}
	id, ok := p.Lookup(gid, fp)
	if !ok || id != 100 {
		t.Fatalf("expected (100, true), got (%d, %v)", id, ok)
	}
}

func TestStoreProvider_FingerprinterRegistration(t *testing.T) {
	s := store.NewMemoryStore()
	t.Cleanup(func() { _ = s.Close() })

	p := NewProvider(s)
	if _, ok := p.Fingerprinter("anthropic"); !ok {
		t.Fatal("expected fingerprinter registered for anthropic")
	}
	if _, ok := p.Fingerprinter("openai"); ok {
		t.Fatal("expected no fingerprinter for openai (not yet implemented)")
	}
}
