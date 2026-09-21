package keypool

import (
	"errors"
	"fmt"
	"testing"

	"gpt-load/internal/encryption"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/models"
	"gpt-load/internal/store"
)

type selectionStore struct {
	store.Store
	rotations int
	readError error
}

func (s *selectionStore) Rotate(key string) (string, error) {
	s.rotations++
	return s.Store.Rotate(key)
}

func (s *selectionStore) HGetAll(key string) (map[string]string, error) {
	if s.readError != nil {
		return nil, s.readError
	}
	return s.Store.HGetAll(key)
}

func TestSelectKeyExcluding(t *testing.T) {
	s := &selectionStore{Store: store.NewMemoryStore()}
	t.Cleanup(func() { _ = s.Close() })
	crypt, err := encryption.NewService("")
	if err != nil {
		t.Fatal(err)
	}
	p := NewProvider(nil, s, nil, crypt)
	for _, key := range []struct {
		id, group uint
		status    string
	}{
		{1, 7, models.KeyStatusActive},
		{2, 7, models.KeyStatusInvalid},
		{3, 8, models.KeyStatusActive},
		// Key 4 is deliberately absent from the hash store.
		{5, 7, models.KeyStatusActive},
	} {
		if err := s.HSet(fmt.Sprintf("key:%d", key.id), map[string]any{
			"id": key.id, "group_id": key.group, "status": key.status, "key_string": "test-key",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.LPush("group:7:active_keys", 5, 4, 3, 2, 1); err != nil {
		t.Fatal(err)
	}
	key, err := p.SelectKeyExcluding(7, map[uint]struct{}{1: {}})
	if err != nil || key.ID != 5 || s.rotations != 5 {
		t.Fatalf("expected key 5 after skipping failed/stale keys, got %v, %v, rotations=%d", key, err, s.rotations)
	}
	s.rotations = 0
	if key, err := p.SelectKeyExcluding(7, map[uint]struct{}{1: {}, 5: {}}); key != nil || !errors.Is(err, app_errors.ErrNoActiveKeys) || s.rotations != 5 {
		t.Fatalf("exhaustion must stop after one list length: key=%v err=%v rotations=%d", key, err, s.rotations)
	}
	if key, err := p.SelectKeyExcluding(99, nil); key != nil || !errors.Is(err, app_errors.ErrNoActiveKeys) {
		t.Fatalf("empty group: key=%v err=%v", key, err)
	}
	s.readError = errors.New("store unavailable")
	if _, err := p.SelectKeyExcluding(7, nil); !errors.Is(err, s.readError) {
		t.Fatalf("store error was hidden: %v", err)
	}
}

func TestSelectKeyExcludingSkipsFailedKeyBeforeLoading(t *testing.T) {
	s := &selectionStore{Store: store.NewMemoryStore(), readError: errors.New("must not load excluded key")}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.LPush("group:7:active_keys", 42); err != nil {
		t.Fatal(err)
	}
	p := NewProvider(nil, s, nil, nil)
	if _, err := p.SelectKeyExcluding(7, map[uint]struct{}{42: {}}); !errors.Is(err, app_errors.ErrNoActiveKeys) {
		t.Fatalf("excluded key should not be read: %v", err)
	}
}
