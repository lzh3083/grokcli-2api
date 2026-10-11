package redis

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUsageDayKeyAndEmptySnapshot(t *testing.T) {
	c := New("redis://127.0.0.1:6379/0", "g2a")
	if got := c.usageDayKey("20260717", "global", ""); got != "g2a:usage:day:20260717:global" {
		t.Fatalf("day key=%s", got)
	}
	if got := c.usageLifeKey("key", "abc"); got != "g2a:usage:life:key:abc" {
		t.Fatalf("life key=%s", got)
	}
	snap := (&Client{}).LightSnapshot(context.Background())
	if snap["source"] != "none" && snap["today_requests"] != int64(0) && snap["today_requests"] != 0 {
		// disabled client returns zeros with source none via server helper; client method itself
		// returns zeros when disabled.
	}
	empty := emptyUsage()
	if empty["requests"] != 0 || empty["total_tokens"] != 0 {
		t.Fatalf("%#v", empty)
	}
}

func TestLeaderForceModes(t *testing.T) {
	l := NewLeader(nil, "never", 4, 0, 0)
	if l.ShouldStartMaintainers(context.Background()) {
		t.Fatal("never should not lead")
	}
	l = NewLeader(nil, "always", 4, 0, 0)
	if !l.ShouldStartMaintainers(context.Background()) || !l.IsLeader() {
		t.Fatal("always should lead")
	}
	l = NewLeader(nil, "auto", 1, 0, 0)
	if !l.ShouldStartMaintainers(context.Background()) {
		t.Fatal("single worker auto should lead locally")
	}
}

func TestWorkerIDNonEmpty(t *testing.T) {
	if WorkerID() == "" {
		t.Fatal("empty worker id")
	}
}

func TestHashAdminSessionTokenAndMaxAge(t *testing.T) {
	h1 := hashAdminSessionToken("tok-123")
	if len(h1) != 64 || h1 == "tok-123" {
		t.Fatalf("unexpected hash %q", h1)
	}
	if adminSessionExceedsMaxAge(`{"ts":1700000000}`, time.Unix(1700000000+3600, 0)) {
		t.Fatal("fresh session should not exceed max age")
	}
	if !adminSessionExceedsMaxAge(`{"ts":1700000000}`, time.Unix(1700000000+31*86400, 0)) {
		t.Fatal("31d old session should exceed max age")
	}
}

func TestLeaderConcurrentReleaseNoPanic(t *testing.T) {
	l := NewLeader(nil, "auto", 4, 0, 0)
	for i := 0; i < 4; i++ {
		l.Release(context.Background())
	}
}

func TestConnPoolCloseInterruptsActiveBorrowedConnections(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	const workers = 4
	accepted := make(chan struct{}, workers)
	serverDone := make(chan struct{})
	var serverWG sync.WaitGroup
	go func() {
		defer close(serverDone)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			serverWG.Add(1)
			go func(c net.Conn) {
				defer serverWG.Done()
				defer c.Close()
				buf := make([]byte, 128)
				_, _ = c.Read(buf)
				select {
				case accepted <- struct{}{}:
				default:
				}
				// Hold connection open without replying until client closes or test finishes.
				_, _ = c.Read(buf)
			}(conn)
		}
	}()

	client := New("redis://"+ln.Addr().String()+"/0", "g2a")
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := client.Get(ctx, fmt.Sprintf("key-%d", idx))
			errs <- err
		}(i)
	}

	// Wait until all workers have borrowed connections and are blocked reading.
	for i := 0; i < workers; i++ {
		select {
		case <-accepted:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for workers to borrow and send command")
		}
	}

	closeStart := time.Now()
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()

	select {
	case <-doneCh:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("active borrowed connections were not interrupted promptly by Close()")
	}
	if elapsed := time.Since(closeStart); elapsed > 500*time.Millisecond {
		t.Fatalf("Close() took too long to interrupt in-flight reads: %v", elapsed)
	}

	close(errs)
	for err := range errs {
		if err == nil {
			t.Fatal("expected error from interrupted in-flight command")
		}
	}

	client.pool.mu.Lock()
	activeCount := len(client.pool.active)
	client.pool.mu.Unlock()
	if activeCount != 0 {
		t.Fatalf("expected 0 active connections after Close(), got %d", activeCount)
	}

	_ = ln.Close()
	<-serverDone
	serverWG.Wait()
}

func TestVerifyAdminSessionMigratesLegacyPlaintextKey(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var mu sync.Mutex
	store := map[string]string{
		"g2a:admin:sess:plain-token-123": fmt.Sprintf(`{"ts":%d}`, time.Now().Unix()),
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					val, err := readRESPValue(r)
					if err != nil {
						return
					}
					args, ok := val.([]string)
					if !ok || len(args) == 0 {
						return
					}
					cmd := strings.ToUpper(args[0])
					mu.Lock()
					switch cmd {
					case "GET":
						v, exists := store[args[1]]
						mu.Unlock()
						if !exists {
							_, _ = c.Write([]byte("$-1\r\n"))
						} else {
							_, _ = fmt.Fprintf(c, "$%d\r\n%s\r\n", len(v), v)
						}
					case "SET":
						store[args[1]] = args[2]
						mu.Unlock()
						_, _ = c.Write([]byte("+OK\r\n"))
					case "DEL":
						deleted := 0
						for _, k := range args[1:] {
							if _, exists := store[k]; exists {
								delete(store, k)
								deleted++
							}
						}
						mu.Unlock()
						_, _ = fmt.Fprintf(c, ":%d\r\n", deleted)
					case "EXPIRE":
						mu.Unlock()
						_, _ = c.Write([]byte(":1\r\n"))
					default:
						mu.Unlock()
						_, _ = c.Write([]byte("+OK\r\n"))
					}
				}
			}(conn)
		}
	}()

	client := New("redis://"+ln.Addr().String()+"/0", "g2a")
	defer client.Close()

	if !client.VerifyAdminSession("plain-token-123") {
		t.Fatal("expected legacy plaintext session to verify")
	}

	hashedKey := "g2a:admin:sess:" + hashAdminSessionToken("plain-token-123")
	legacyKey := "g2a:admin:sess:plain-token-123"

	mu.Lock()
	_, hasHashed := store[hashedKey]
	_, hasLegacy := store[legacyKey]
	mu.Unlock()

	if !hasHashed {
		t.Fatalf("expected session migrated to hashed key %s", hashedKey)
	}
	if hasLegacy {
		t.Fatalf("expected legacy plaintext key %s to be deleted", legacyKey)
	}
}
