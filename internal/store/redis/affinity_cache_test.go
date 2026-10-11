package redis

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestAffinityLocalCacheRoundTrip(t *testing.T) {
	affinityCacheMu.Lock()
	affinityCache = map[string]affinityCacheEntry{}
	responseCache = map[string]responseCacheEntry{}
	affinityCacheMu.Unlock()

	if _, ok := affinityCacheGet("fp-1"); ok {
		t.Fatal("expected miss")
	}
	affinityCacheSet("fp-1", "acc-1")
	id, ok := affinityCacheGet("fp-1")
	if !ok || id != "acc-1" {
		t.Fatalf("got %q ok=%v", id, ok)
	}
	// Expired entry should miss.
	affinityCacheMu.Lock()
	e := affinityCache["fp-1"]
	e.expires = time.Now().Add(-time.Second)
	affinityCache["fp-1"] = e
	affinityCacheMu.Unlock()
	if _, ok := affinityCacheGet("fp-1"); ok {
		t.Fatal("expected expired miss")
	}
}

func TestResponseLocalCacheRoundTrip(t *testing.T) {
	affinityCacheMu.Lock()
	responseCache = map[string]responseCacheEntry{}
	affinityCacheMu.Unlock()

	responseCacheSet("resp_1", "acc-9", "pck-root")
	acc, pck, ok := responseCacheGet("resp_1")
	if !ok || acc != "acc-9" || pck != "pck-root" {
		t.Fatalf("got %q %q ok=%v", acc, pck, ok)
	}
}

func TestAffinityScheduleWriteCoalesces(t *testing.T) {
	// Reset pending maps
	affinityPendingMu.Lock()
	affinityPending = map[string]affinityPendingWrite{}
	affinityFlushing = map[string]bool{}
	affinityPendingMu.Unlock()

	var mu sync.Mutex
	var calls []string
	var accounts []string
	writer := func(ctx context.Context, fingerprint, accountID string, ttl time.Duration, sessionFP, promptCacheKey string) error {
		mu.Lock()
		calls = append(calls, fingerprint)
		accounts = append(accounts, accountID)
		mu.Unlock()
		// slow enough that subsequent schedules merge into pending
		time.Sleep(30 * time.Millisecond)
		return nil
	}

	// Burst: same key, three account updates — should flush latest, not 3 full serial if merged.
	affinityScheduleWrite("fp-coalesce", "a1", time.Hour, "", "", writer)
	affinityScheduleWrite("fp-coalesce", "a2", time.Hour, "", "", writer)
	affinityScheduleWrite("fp-coalesce", "a3", time.Hour, "", "", writer)

	// Wait for flush loop to drain
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		affinityPendingMu.Lock()
		pending := len(affinityPending)
		flushing := affinityFlushing["fp-coalesce"]
		affinityPendingMu.Unlock()
		if pending == 0 && !flushing {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("expected at least one redis write")
	}
	// Last write must be a3 (latest pending wins).
	if accounts[len(accounts)-1] != "a3" {
		t.Fatalf("last account=%v calls=%v", accounts, calls)
	}
	// Should be heavily coalesced (not 3+ independent starts without merge).
	if len(calls) > 3 {
		t.Fatalf("too many writes without coalesce: %d", len(calls))
	}
}

func TestAffinityCacheDeleteDropsPending(t *testing.T) {
	affinityCacheMu.Lock()
	affinityCache = map[string]affinityCacheEntry{}
	affinityCacheMu.Unlock()
	affinityPendingMu.Lock()
	affinityPending = map[string]affinityPendingWrite{}
	affinityFlushing = map[string]bool{}
	affinityPendingMu.Unlock()

	// Seed pending write without starting flusher by setting pending while flushing=true
	affinityPendingMu.Lock()
	affinityFlushing["fp-drop"] = true
	affinityPending["fp-drop"] = affinityPendingWrite{accountID: "acc-x", ttl: time.Hour}
	affinityPendingMu.Unlock()

	affinityCacheSet("fp-drop", "acc-x")
	affinityCacheDelete("fp-drop")

	if _, ok := affinityCacheGet("fp-drop"); ok {
		t.Fatal("local cache should be cleared")
	}
	affinityPendingMu.Lock()
	_, still := affinityPending["fp-drop"]
	affinityFlushing["fp-drop"] = false
	affinityPendingMu.Unlock()
	if still {
		t.Fatal("pending write should be dropped on delete")
	}
}

func TestAffinityCacheLongKeyAndHardCap(t *testing.T) {
	affinityCacheMu.Lock()
	affinityCache = map[string]affinityCacheEntry{}
	responseCache = map[string]responseCacheEntry{}
	affinityCacheMu.Unlock()

	longKey := string(make([]byte, 400))
	affinityCacheSet(longKey, "acc-long")
	if got, ok := affinityCacheGet(longKey); !ok || got != "acc-long" {
		t.Fatalf("long key lookup got=%q ok=%v", got, ok)
	}
	affinityCacheMu.Lock()
	for k := range affinityCache {
		if len(k) > maxAffinityCacheKeyLen {
			affinityCacheMu.Unlock()
			t.Fatalf("cache key length %d exceeds max %d", len(k), maxAffinityCacheKeyLen)
		}
	}
	affinityCacheMu.Unlock()

	// Fill beyond 8192 non-expired entries and verify hard cap holds.
	for i := 0; i < maxAffinityCacheSize+100; i++ {
		affinityCacheSet("k-"+time.Duration(i).String(), "acc")
	}
	affinityCacheMu.Lock()
	n := len(affinityCache)
	affinityCacheMu.Unlock()
	if n > maxAffinityCacheSize {
		t.Fatalf("affinityCache size %d exceeds hard cap %d", n, maxAffinityCacheSize)
	}
}

func TestAffinityAndResponseScheduleWriteLongKeyAndDeletePending(t *testing.T) {
	affinityPendingMu.Lock()
	affinityPending = map[string]affinityPendingWrite{}
	affinityFlushing = map[string]bool{}
	affinityPendingMu.Unlock()

	responsePendingMu.Lock()
	responsePending = map[string]responsePendingWrite{}
	responseFlushing = map[string]bool{}
	responsePendingMu.Unlock()

	longFP := string(make([]byte, 512))
	longRID := "resp_" + string(make([]byte, 512))
	longPCK := "pck_" + string(make([]byte, 512))
	normFP := normalizeAffinityCacheKey(longFP)
	normRID := normalizeAffinityCacheKey(longRID)
	normPCK := normalizeAffinityCacheKey(longPCK)

	// Mark normFP and normRID as already flushing so scheduleWrite queues into pending without starting a goroutine.
	affinityPendingMu.Lock()
	affinityFlushing[normFP] = true
	affinityPendingMu.Unlock()

	responsePendingMu.Lock()
	responseFlushing[normRID] = true
	responsePendingMu.Unlock()

	noopAffWriter := func(ctx context.Context, fingerprint, accountID string, ttl time.Duration, sessionFP, promptCacheKey string) error {
		return nil
	}
	noopRespWriter := func(ctx context.Context, responseID, accountID, promptCacheKey string, ttl time.Duration) error {
		return nil
	}

	affinityScheduleWrite(longFP, "acc-1", time.Hour, longFP, longPCK, noopAffWriter)
	responseScheduleWrite(longRID, "acc-2", longPCK, time.Hour, noopRespWriter)

	affinityPendingMu.Lock()
	affJob, affOK := affinityPending[normFP]
	_, rawAffExists := affinityPending[longFP]
	affinityPendingMu.Unlock()
	if !affOK || rawAffExists {
		t.Fatalf("expected normalized pending key %q (ok=%v, raw=%v)", normFP, affOK, rawAffExists)
	}
	if affJob.promptCacheKey != normPCK || affJob.sessionFP != normFP {
		t.Fatalf("expected normalized metadata pck=%q sfp=%q", affJob.promptCacheKey, affJob.sessionFP)
	}

	responsePendingMu.Lock()
	respJob, respOK := responsePending[normRID]
	_, rawRespExists := responsePending[longRID]
	responsePendingMu.Unlock()
	if !respOK || rawRespExists {
		t.Fatalf("expected normalized response pending key %q (ok=%v, raw=%v)", normRID, respOK, rawRespExists)
	}
	if respJob.promptCacheKey != normPCK {
		t.Fatalf("expected normalized response pck=%q, got %q", normPCK, respJob.promptCacheKey)
	}

	// Delete pending using raw overlong keys and verify normalized entries are removed.
	affinityDeletePending(longFP)
	responseDeletePending(longRID)

	affinityPendingMu.Lock()
	_, affStill := affinityPending[normFP]
	delete(affinityFlushing, normFP)
	affinityPendingMu.Unlock()
	if affStill {
		t.Fatal("expected affinityDeletePending(longFP) to remove normalized pending entry")
	}

	responsePendingMu.Lock()
	_, respStill := responsePending[normRID]
	delete(responseFlushing, normRID)
	responsePendingMu.Unlock()
	if respStill {
		t.Fatal("expected responseDeletePending(longRID) to remove normalized pending entry")
	}

	// Verify pending map hard cap (> 8192) evicts old entries.
	affinityPendingMu.Lock()
	for i := 0; i < maxAffinityCacheSize+100; i++ {
		k := "p-" + time.Duration(i).String()
		affinityFlushing[k] = true
	}
	affinityPendingMu.Unlock()
	for i := 0; i < maxAffinityCacheSize+100; i++ {
		k := "p-" + time.Duration(i).String()
		affinityScheduleWrite(k, "acc", time.Hour, "", "", noopAffWriter)
	}
	affinityPendingMu.Lock()
	pendingLen := len(affinityPending)
	affinityPending = map[string]affinityPendingWrite{}
	affinityFlushing = map[string]bool{}
	affinityPendingMu.Unlock()
	if pendingLen > maxAffinityCacheSize {
		t.Fatalf("affinityPending size %d exceeds hard cap %d", pendingLen, maxAffinityCacheSize)
	}
}
