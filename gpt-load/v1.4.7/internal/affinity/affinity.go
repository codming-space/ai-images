// Package affinity provides per-channel request affinity to maximize upstream
// prompt cache hit rate. Same request content -> same upstream key, while
// preserving the existing round-robin load balancing for non-matching traffic.
//
// This package is strictly read-only with respect to request bodies, headers
// and URLs. It only computes a fingerprint from the body bytes and reads/writes
// (fingerprint -> key_id) entries in the shared Store.
package affinity

import (
	"fmt"
	"strconv"
	"time"

	"gpt-load/internal/store"
)

// Status describes what happened to a single request from the affinity
// layer's point of view. Exposed via RequestLog.AffinityStatus so operators
// can spot-check hit rates and re-binding behavior in the admin UI.
const (
	// StatusNone — request is *outside the affinity surface*: no Fingerprinter
	// registered for this channel_type (e.g. OpenAI/Gemini requests) or the
	// feature is globally disabled. Stored as "" so existing rows / non-Claude
	// channels naturally fall in this bucket.
	StatusNone = ""
	// StatusSkip — request *belongs* to a channel that supports affinity, but
	// this particular request did not qualify (e.g. wrong path, no
	// cache_control, empty first_user). Distinct from StatusNone so the UI
	// can tell "request didn't meet the criteria" apart from "channel doesn't
	// participate in affinity at all".
	StatusSkip = "skip"
	// StatusMiss — request qualified for affinity but no prior binding was
	// found. If the request succeeds, a binding will be recorded.
	StatusMiss = "miss"
	// StatusHit — an existing binding was found and the bound key is active.
	// The request is using that key.
	StatusHit = "hit"
	// StatusUnbind — an existing binding was cleared, either because the
	// bound key turned out to be invalid (stale binding) or because the
	// bound key just failed on this request.
	StatusUnbind = "unbind"
)

// Fingerprinter computes an affinity fingerprint for a request belonging to a
// specific channel type. Implementations decide whether a given (model, path)
// is eligible for affinity and how to derive a stable fingerprint from the
// request body.
type Fingerprinter interface {
	// Enabled reports whether affinity is turned on for this channel type.
	Enabled() bool
	// TTL is the lifetime for new (fp -> key_id) mappings.
	TTL() time.Duration
	// Compute returns the fingerprint for the given request.
	// matched=false means the request does not qualify for affinity and the
	// caller should fall back to plain round-robin.
	Compute(model, path string, body []byte) (fp string, matched bool)
}

// Provider persists (group_id, fp) -> key_id mappings.
type Provider interface {
	// Fingerprinter returns the Fingerprinter registered for the given channel
	// type, or (nil, false) when affinity is not supported for that channel.
	Fingerprinter(channelType string) (Fingerprinter, bool)
	// Lookup returns the previously bound key_id for (group_id, fp), if any.
	Lookup(groupID uint, fp string) (keyID uint, found bool)
	// Record binds fp -> keyID with the given TTL. Uses SET NX semantics: an
	// existing mapping is preserved, avoiding concurrent-write thrash.
	Record(groupID uint, fp string, keyID uint, ttl time.Duration) error
	// Delete removes the mapping for (group_id, fp), used when the bound key
	// turns out to be invalid or when a finally-failed request must clear a
	// poisoned binding.
	Delete(groupID uint, fp string) error
}

type provider struct {
	store store.Store
	fps   map[string]Fingerprinter
}

// NewProvider builds the affinity provider with per-channel Fingerprinters.
// Add new channel types here (e.g. "openai", "gemini") when supported.
func NewProvider(s store.Store) Provider {
	return &provider{
		store: s,
		fps: map[string]Fingerprinter{
			claudeChannelType: newClaudeFingerprinter(),
		},
	}
}

func (p *provider) Fingerprinter(channelType string) (Fingerprinter, bool) {
	fp, ok := p.fps[channelType]
	return fp, ok
}

func (p *provider) key(groupID uint, fp string) string {
	return fmt.Sprintf("gpt-load:affinity:v1:%d:%s", groupID, fp)
}

func (p *provider) Lookup(groupID uint, fp string) (uint, bool) {
	raw, err := p.store.Get(p.key(groupID, fp))
	if err != nil || len(raw) == 0 {
		return 0, false
	}
	id, err := strconv.ParseUint(string(raw), 10, 64)
	if err != nil {
		return 0, false
	}
	return uint(id), true
}

func (p *provider) Record(groupID uint, fp string, keyID uint, ttl time.Duration) error {
	val := []byte(strconv.FormatUint(uint64(keyID), 10))
	_, err := p.store.SetNX(p.key(groupID, fp), val, ttl)
	return err
}

func (p *provider) Delete(groupID uint, fp string) error {
	return p.store.Delete(p.key(groupID, fp))
}
