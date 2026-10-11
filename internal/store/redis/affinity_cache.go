package redis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// affinityLocalCache avoids repeated Redis GETs for the same sticky fingerprint
// under multi-turn Codex bursts (same prompt_cache_key / response chain).
type affinityCacheEntry struct {
	accountID string
	expires   time.Time
}

type responseCacheEntry struct {
	accountID      string
	promptCacheKey string
	expires        time.Time
}

var (
	affinityCacheMu sync.Mutex
	affinityCache   = map[string]affinityCacheEntry{}
	responseCache   = map[string]responseCacheEntry{}

	// Coalesce background Redis writes for the same sticky key so multi-turn
	// bursts (Claude Code / Codex) do not enqueue one goroutine+SET per token hit.
	affinityPendingMu sync.Mutex
	affinityPending   = map[string]affinityPendingWrite{} // fingerprint -> latest
	affinityFlushing  = map[string]bool{}                 // in-flight flush per key

	responsePendingMu sync.Mutex
	responsePending   = map[string]responsePendingWrite{}
	responseFlushing  = map[string]bool{}
)

type affinityPendingWrite struct {
	accountID string
	ttl       time.Duration
	// optional metadata for BindAffinity
	sessionFP      string
	promptCacheKey string
	writer         func(ctx context.Context, fingerprint, accountID string, ttl time.Duration, sessionFP, promptCacheKey string) error
}

type responsePendingWrite struct {
	accountID      string
	promptCacheKey string
	ttl            time.Duration
	writer         func(ctx context.Context, responseID, accountID, promptCacheKey string, ttl time.Duration) error
}

// Long enough to cover multi-turn Codex sessions in-process without thrashing Redis.
// Still short enough that kick/disable eventually re-resolves via Redis/PG eligibility.
const (
	affinityLocalTTL       = 30 * time.Minute
	responseLocalTTL       = 30 * time.Minute
	maxAffinityCacheSize   = 8192
	maxAffinityCacheKeyLen = 256
)

func normalizeAffinityCacheKey(key string) string {
	key = stringsTrim(key)
	if len(key) > maxAffinityCacheKeyLen {
		sum := sha256.Sum256([]byte(key))
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	return key
}

func affinityCacheGet(fingerprint string) (string, bool) {
	fingerprint = normalizeAffinityCacheKey(fingerprint)
	if fingerprint == "" {
		return "", false
	}
	now := time.Now()
	affinityCacheMu.Lock()
	defer affinityCacheMu.Unlock()
	entry, ok := affinityCache[fingerprint]
	if !ok || now.After(entry.expires) {
		if ok {
			delete(affinityCache, fingerprint)
		}
		return "", false
	}
	return entry.accountID, true
}

func affinityCacheSet(fingerprint, accountID string) {
	fingerprint = normalizeAffinityCacheKey(fingerprint)
	accountID = stringsTrim(accountID)
	if fingerprint == "" || accountID == "" {
		return
	}
	affinityCacheMu.Lock()
	affinityCache[fingerprint] = affinityCacheEntry{accountID: accountID, expires: time.Now().Add(affinityLocalTTL)}
	if len(affinityCache) > maxAffinityCacheSize {
		now := time.Now()
		for k, v := range affinityCache {
			if now.After(v.expires) {
				delete(affinityCache, k)
			}
		}
		for k := range affinityCache {
			if len(affinityCache) <= maxAffinityCacheSize {
				break
			}
			if k == fingerprint {
				continue
			}
			delete(affinityCache, k)
		}
	}
	affinityCacheMu.Unlock()
}

func responseCacheGet(responseID string) (accountID, promptCacheKey string, ok bool) {
	responseID = normalizeAffinityCacheKey(responseID)
	if responseID == "" {
		return "", "", false
	}
	now := time.Now()
	affinityCacheMu.Lock()
	defer affinityCacheMu.Unlock()
	entry, found := responseCache[responseID]
	if !found || now.After(entry.expires) {
		if found {
			delete(responseCache, responseID)
		}
		return "", "", false
	}
	return entry.accountID, entry.promptCacheKey, true
}

func responseCacheSet(responseID, accountID, promptCacheKey string) {
	responseID = normalizeAffinityCacheKey(responseID)
	accountID = stringsTrim(accountID)
	promptCacheKey = normalizeAffinityCacheKey(promptCacheKey)
	if responseID == "" || accountID == "" {
		return
	}
	affinityCacheMu.Lock()
	responseCache[responseID] = responseCacheEntry{
		accountID:      accountID,
		promptCacheKey: promptCacheKey,
		expires:        time.Now().Add(responseLocalTTL),
	}
	if len(responseCache) > maxAffinityCacheSize {
		now := time.Now()
		for k, v := range responseCache {
			if now.After(v.expires) {
				delete(responseCache, k)
			}
		}
		for k := range responseCache {
			if len(responseCache) <= maxAffinityCacheSize {
				break
			}
			if k == responseID {
				continue
			}
			delete(responseCache, k)
		}
	}
	affinityCacheMu.Unlock()
}

func stringsTrim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		last := s[len(s)-1]
		if last == ' ' || last == '\t' || last == '\n' || last == '\r' {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}

// affinityScheduleWrite merges Redis sticky writes per fingerprint.
// Local cache is assumed already updated by caller. Only one flush goroutine
// runs per key; later binds overwrite the pending payload.
func affinityScheduleWrite(
	fingerprint, accountID string,
	ttl time.Duration,
	sessionFP, promptCacheKey string,
	writer func(ctx context.Context, fingerprint, accountID string, ttl time.Duration, sessionFP, promptCacheKey string) error,
) {
	fingerprint = normalizeAffinityCacheKey(fingerprint)
	accountID = stringsTrim(accountID)
	sessionFP = normalizeAffinityCacheKey(sessionFP)
	promptCacheKey = normalizeAffinityCacheKey(promptCacheKey)
	if fingerprint == "" || accountID == "" || writer == nil {
		return
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	affinityPendingMu.Lock()
	affinityPending[fingerprint] = affinityPendingWrite{
		accountID: accountID, ttl: ttl,
		sessionFP: sessionFP, promptCacheKey: promptCacheKey,
		writer: writer,
	}
	if len(affinityPending) > maxAffinityCacheSize {
		for k := range affinityPending {
			if len(affinityPending) <= maxAffinityCacheSize {
				break
			}
			if k == fingerprint {
				continue
			}
			delete(affinityPending, k)
		}
	}
	if affinityFlushing[fingerprint] {
		affinityPendingMu.Unlock()
		return
	}
	affinityFlushing[fingerprint] = true
	affinityPendingMu.Unlock()

	go func(fp string) {
		for {
			affinityPendingMu.Lock()
			job, ok := affinityPending[fp]
			if !ok {
				delete(affinityFlushing, fp)
				affinityPendingMu.Unlock()
				return
			}
			delete(affinityPending, fp)
			affinityPendingMu.Unlock()

			bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = job.writer(bg, fp, job.accountID, job.ttl, job.sessionFP, job.promptCacheKey)
			cancel()
		}
	}(fingerprint)
}

func responseScheduleWrite(
	responseID, accountID, promptCacheKey string,
	ttl time.Duration,
	writer func(ctx context.Context, responseID, accountID, promptCacheKey string, ttl time.Duration) error,
) {
	responseID = normalizeAffinityCacheKey(responseID)
	accountID = stringsTrim(accountID)
	promptCacheKey = normalizeAffinityCacheKey(promptCacheKey)
	if responseID == "" || accountID == "" || writer == nil {
		return
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	responsePendingMu.Lock()
	responsePending[responseID] = responsePendingWrite{
		accountID: accountID, promptCacheKey: promptCacheKey,
		ttl: ttl, writer: writer,
	}
	if len(responsePending) > maxAffinityCacheSize {
		for k := range responsePending {
			if len(responsePending) <= maxAffinityCacheSize {
				break
			}
			if k == responseID {
				continue
			}
			delete(responsePending, k)
		}
	}
	if responseFlushing[responseID] {
		responsePendingMu.Unlock()
		return
	}
	responseFlushing[responseID] = true
	responsePendingMu.Unlock()

	go func(rid string) {
		for {
			responsePendingMu.Lock()
			job, ok := responsePending[rid]
			if !ok {
				delete(responseFlushing, rid)
				responsePendingMu.Unlock()
				return
			}
			delete(responsePending, rid)
			responsePendingMu.Unlock()

			bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = job.writer(bg, rid, job.accountID, job.promptCacheKey, job.ttl)
			cancel()
		}
	}(responseID)
}

func affinityDeletePending(fingerprint string) {
	rawFP := stringsTrim(fingerprint)
	if rawFP == "" {
		return
	}
	normKey := normalizeAffinityCacheKey(rawFP)
	affinityPendingMu.Lock()
	delete(affinityPending, normKey)
	if normKey != rawFP {
		delete(affinityPending, rawFP)
	}
	affinityPendingMu.Unlock()
}

func responseDeletePending(responseID string) {
	rawRID := stringsTrim(responseID)
	if rawRID == "" {
		return
	}
	normKey := normalizeAffinityCacheKey(rawRID)
	responsePendingMu.Lock()
	delete(responsePending, normKey)
	if normKey != rawRID {
		delete(responsePending, rawRID)
	}
	responsePendingMu.Unlock()
}

func affinityCacheDelete(fingerprint string) {
	rawFP := stringsTrim(fingerprint)
	if rawFP == "" {
		return
	}
	normKey := normalizeAffinityCacheKey(rawFP)
	affinityCacheMu.Lock()
	delete(affinityCache, normKey)
	if normKey != rawFP {
		delete(affinityCache, rawFP)
	}
	affinityCacheMu.Unlock()
	// Drop any coalesced pending Redis write so a stale rebind cannot resurrect
	// a just-cleared pin after sticky failover.
	affinityDeletePending(rawFP)
}

func responseCacheDelete(responseID string) {
	rawRID := stringsTrim(responseID)
	if rawRID == "" {
		return
	}
	normKey := normalizeAffinityCacheKey(rawRID)
	affinityCacheMu.Lock()
	delete(responseCache, normKey)
	if normKey != rawRID {
		delete(responseCache, rawRID)
	}
	affinityCacheMu.Unlock()
	responseDeletePending(rawRID)
}
