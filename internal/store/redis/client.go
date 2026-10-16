package redis

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	adminSessionTTLSeconds = 7 * 24 * 3600
	adminSessionMaxAge     = 30 * 24 * time.Hour
)

const (
	defaultPoolSize     = 32
	defaultMaxOpenConns = 128
	defaultDialTimeout  = 800 * time.Millisecond
	defaultIdleTimeout  = 45 * time.Second
	defaultCmdTimeout   = 2 * time.Second
)

type Client struct {
	URL    string
	Prefix string

	poolOnce sync.Once
	pool     *connPool
}

type connPool struct {
	addr     string
	password string
	db       string

	maxIdle int
	maxOpen int
	sem     chan struct{}
	idle    chan *pooledConn
	dialer  net.Dialer

	mu     sync.Mutex
	active map[*pooledConn]struct{}

	// closed is set on Close() / process exit paths.
	closed atomic.Bool
}

type pooledConn struct {
	net.Conn
	reader   *bufio.Reader
	lastUsed time.Time
}

func New(urlValue, prefix string) *Client {
	if strings.TrimSpace(prefix) == "" {
		prefix = "g2a"
	}
	return &Client{URL: strings.TrimSpace(urlValue), Prefix: strings.Trim(prefix, ":")}
}

func (c *Client) Enabled() bool {
	return c != nil && strings.TrimSpace(c.URL) != ""
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.command(ctx, "PING")
	return err
}

func (c *Client) Close() error {
	if c == nil || c.pool == nil {
		return nil
	}
	return c.pool.Close()
}

func hashAdminSessionToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func isAdminSessionValid(raw string, now time.Time) bool {
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil || payload == nil {
		return false
	}
	var ts float64
	for _, field := range []string{"ts", "created_at"} {
		if v, ok := payload[field]; ok && v != nil {
			switch x := v.(type) {
			case float64:
				ts = x
			case float32:
				ts = float64(x)
			case int64:
				ts = float64(x)
			case int:
				ts = float64(x)
			case json.Number:
				ts, _ = x.Float64()
			case string:
				ts, _ = strconv.ParseFloat(strings.TrimSpace(x), 64)
			}
			if ts > 0 {
				break
			}
		}
	}
	if ts <= 0 {
		return false
	}
	if ts > 1e12 {
		ts = ts / 1000
	}
	return now.Sub(time.Unix(int64(ts), 0)) <= adminSessionMaxAge
}

func adminSessionExceedsMaxAge(raw string, now time.Time) bool {
	return !isAdminSessionValid(raw, now)
}

func (c *Client) VerifyAdminSession(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" || !c.Enabled() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hashed := hashAdminSessionToken(token)
	hashedKey := c.key("admin", "sess", hashed)
	legacyKey := c.key("admin", "sess", token)
	matchedLegacy := false
	value, err := c.command(ctx, "GET", hashedKey)
	if err != nil || value == "" {
		matchedLegacy = true
		value, err = c.command(ctx, "GET", legacyKey)
		if err != nil || value == "" {
			return false
		}
	}
	if !isAdminSessionValid(value, time.Now()) {
		_, _ = c.command(ctx, "DEL", hashedKey, legacyKey)
		return false
	}
	if matchedLegacy {
		_, _ = c.command(ctx, "SET", hashedKey, value, "EX", strconv.Itoa(adminSessionTTLSeconds))
		if legacyKey != hashedKey {
			_, _ = c.command(ctx, "DEL", legacyKey)
		}
		return true
	}
	_, _ = c.command(ctx, "EXPIRE", hashedKey, strconv.Itoa(adminSessionTTLSeconds))
	return true
}

// CreateAdminSession stores an admin session payload under
// g2a:admin:sess:{sha256(token)} with a 7-day TTL.
func (c *Client) CreateAdminSession(token string) error {
	token = strings.TrimSpace(token)
	if token == "" || !c.Enabled() {
		return errors.New("redis admin session store unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	payload := fmt.Sprintf(`{"ts":%d}`, time.Now().Unix())
	hashedKey := c.key("admin", "sess", hashAdminSessionToken(token))
	_, err := c.command(ctx, "SET", hashedKey, payload, "EX", strconv.Itoa(adminSessionTTLSeconds))
	return err
}

func (c *Client) DeleteAdminSession(token string) error {
	token = strings.TrimSpace(token)
	if token == "" || !c.Enabled() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hashedKey := c.key("admin", "sess", hashAdminSessionToken(token))
	legacyKey := c.key("admin", "sess", token)
	_, err := c.command(ctx, "DEL", hashedKey, legacyKey)
	return err
}

func (c *Client) key(parts ...string) string {
	segments := []string{strings.Trim(c.Prefix, ":")}
	for _, part := range parts {
		part = strings.Trim(strings.TrimSpace(part), ":")
		if part != "" {
			segments = append(segments, part)
		}
	}
	return strings.Join(segments, ":")
}

func (c *Client) Get(ctx context.Context, key string) (string, error) {
	return c.command(ctx, "GET", key)
}

func (c *Client) SetEX(ctx context.Context, key, value string, ttlSeconds int) error {
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	_, err := c.command(ctx, "SET", key, value, "EX", strconv.Itoa(ttlSeconds))
	return err
}

func (c *Client) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	_, err := c.command(ctx, append([]string{"DEL"}, keys...)...)
	return err
}

func (c *Client) Expire(ctx context.Context, key string, ttlSeconds int) error {
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	_, err := c.command(ctx, "EXPIRE", key, strconv.Itoa(ttlSeconds))
	return err
}

func (c *Client) Incr(ctx context.Context, key string) (int64, error) {
	raw, err := c.command(ctx, "INCR", key)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}

func (c *Client) Decr(ctx context.Context, key string) (int64, error) {
	raw, err := c.command(ctx, "DECR", key)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}

func (c *Client) HIncrBy(ctx context.Context, key, field string, amount int64) (int64, error) {
	raw, err := c.command(ctx, "HINCRBY", key, field, strconv.FormatInt(amount, 10))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}

func (c *Client) HSetMap(ctx context.Context, key string, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	args := []string{"HSET", key}
	for k, v := range values {
		args = append(args, k, v)
	}
	_, err := c.command(ctx, args...)
	return err
}

func (c *Client) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	values, err := c.commandArray(ctx, "HGETALL", key)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(values)/2)
	for i := 0; i+1 < len(values); i += 2 {
		out[values[i]] = values[i+1]
	}
	return out, nil
}

// SetNXEX is SET key value NX EX ttl. Returns true when acquired.
func (c *Client) SetNXEX(ctx context.Context, key, value string, ttlSeconds int) (bool, error) {
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	raw, err := c.command(ctx, "SET", key, value, "NX", "EX", strconv.Itoa(ttlSeconds))
	if err != nil {
		// redis returns nil bulk for not acquired; our reader maps that to empty string without error
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(raw), "OK"), nil
}

// CompareAndDelete deletes key only when current value equals expected.
func (c *Client) CompareAndDelete(ctx context.Context, key, expected string) (bool, error) {
	script := "if redis.call('get', KEYS[1]) == ARGV[1] then return redis.call('del', KEYS[1]) else return 0 end"
	raw, err := c.command(ctx, "EVAL", script, "1", key, expected)
	if err != nil {
		return false, err
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	return n > 0, nil
}

// RenewIfOwner refreshes TTL only when the key still holds expected value.
func (c *Client) RenewIfOwner(ctx context.Context, key, expected string, ttlSeconds int) (bool, error) {
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	script := "if redis.call('get', KEYS[1]) == ARGV[1] then return redis.call('expire', KEYS[1], ARGV[2]) else return 0 end"
	raw, err := c.command(ctx, "EVAL", script, "1", key, expected, strconv.Itoa(ttlSeconds))
	if err != nil {
		return false, err
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	return n > 0, nil
}

// SAdd adds members to a set and optionally refreshes TTL (ttlSeconds<=0 skips expire).
func (c *Client) SAdd(ctx context.Context, key string, ttlSeconds int, members ...string) (int64, error) {
	if len(members) == 0 {
		return 0, nil
	}
	args := append([]string{"SADD", key}, members...)
	raw, err := c.command(ctx, args...)
	if err != nil {
		return 0, err
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if ttlSeconds > 0 {
		_ = c.Expire(ctx, key, ttlSeconds)
	}
	return n, nil
}

// SMembers returns all members of a set.
func (c *Client) SMembers(ctx context.Context, key string) ([]string, error) {
	return c.commandArray(ctx, "SMEMBERS", key)
}

// SCard returns set cardinality.
func (c *Client) SCard(ctx context.Context, key string) (int64, error) {
	raw, err := c.command(ctx, "SCARD", key)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}

// SRem removes members from a set.
func (c *Client) SRem(ctx context.Context, key string, members ...string) (int64, error) {
	if len(members) == 0 {
		return 0, nil
	}
	args := append([]string{"SREM", key}, members...)
	raw, err := c.command(ctx, args...)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}

func (c *Client) command(ctx context.Context, args ...string) (string, error) {
	value, err := c.do(ctx, args...)
	if err != nil {
		return "", err
	}
	switch v := value.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case []string:
		if len(v) == 0 {
			return "", nil
		}
		return v[0], nil
	default:
		return fmt.Sprint(v), nil
	}
}

func (c *Client) commandArray(ctx context.Context, args ...string) ([]string, error) {
	value, err := c.do(ctx, args...)
	if err != nil {
		return nil, err
	}
	switch v := value.(type) {
	case nil:
		return nil, nil
	case []string:
		return v, nil
	case string:
		if v == "" {
			return nil, nil
		}
		return []string{v}, nil
	default:
		return nil, fmt.Errorf("unexpected redis array response %T", value)
	}
}

// pipeline runs multiple commands on one pooled connection (one RTT).
func (c *Client) pipeline(ctx context.Context, cmds [][]string) ([]any, error) {
	if !c.Enabled() {
		return nil, errors.New("redis unavailable")
	}
	if len(cmds) == 0 {
		return nil, nil
	}
	for _, cmd := range cmds {
		if len(cmd) == 0 {
			return nil, errors.New("empty redis command")
		}
	}
	pool, err := c.ensurePool()
	if err != nil {
		return nil, err
	}
	pc, err := pool.borrow(ctx)
	if err != nil {
		return nil, err
	}
	deadline := deadlineFrom(ctx, defaultCmdTimeout)
	_ = pc.SetDeadline(deadline)

	for _, cmd := range cmds {
		if err := writeRESP(pc.Conn, cmd...); err != nil {
			pool.discard(pc)
			return nil, err
		}
	}
	out := make([]any, len(cmds))
	for i := range cmds {
		value, err := readRESPValue(pc.reader)
		if err != nil {
			pool.discard(pc)
			return nil, err
		}
		out[i] = value
	}
	pool.put(pc)
	return out, nil
}

func (c *Client) do(ctx context.Context, args ...string) (any, error) {
	if !c.Enabled() {
		return nil, errors.New("redis unavailable")
	}
	if len(args) == 0 {
		return nil, errors.New("empty redis command")
	}
	pool, err := c.ensurePool()
	if err != nil {
		return nil, err
	}
	pc, err := pool.borrow(ctx)
	if err != nil {
		return nil, err
	}
	deadline := deadlineFrom(ctx, defaultCmdTimeout)
	_ = pc.SetDeadline(deadline)
	if err := writeRESP(pc.Conn, args...); err != nil {
		pool.discard(pc)
		return nil, err
	}
	value, err := readRESPValue(pc.reader)
	if err != nil {
		pool.discard(pc)
		return nil, err
	}
	pool.put(pc)
	return value, nil
}

func (c *Client) ensurePool() (*connPool, error) {
	var initErr error
	c.poolOnce.Do(func() {
		addr, password, db, err := parseRedisURL(c.URL)
		if err != nil {
			initErr = err
			return
		}
		c.pool = &connPool{
			addr:     addr,
			password: password,
			db:       db,
			maxIdle:  defaultPoolSize,
			maxOpen:  defaultMaxOpenConns,
			sem:      make(chan struct{}, defaultMaxOpenConns),
			idle:     make(chan *pooledConn, defaultPoolSize),
			active:   make(map[*pooledConn]struct{}),
			dialer: net.Dialer{
				Timeout:   defaultDialTimeout,
				KeepAlive: 30 * time.Second,
			},
		}
	})
	if initErr != nil {
		return nil, initErr
	}
	if c.pool == nil {
		return nil, errors.New("redis pool unavailable")
	}
	return c.pool, nil
}

func (p *connPool) acquireSem(ctx context.Context) error {
	if p == nil || p.sem == nil {
		return nil
	}
	if ctx != nil {
		if _, ok := ctx.Deadline(); ok {
			select {
			case p.sem <- struct{}{}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	timer := time.NewTimer(defaultDialTimeout)
	defer timer.Stop()
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-timer.C:
		return errors.New("redis connection pool exhausted")
	}
}

func (p *connPool) releaseSem() {
	if p == nil || p.sem == nil {
		return
	}
	select {
	case <-p.sem:
	default:
	}
}

func (p *connPool) borrow(ctx context.Context) (*pooledConn, error) {
	if p == nil || p.closed.Load() {
		return nil, errors.New("redis pool unavailable")
	}
	if err := p.acquireSem(ctx); err != nil {
		return nil, err
	}
	for {
		if p.closed.Load() {
			p.releaseSem()
			return nil, errors.New("redis pool closed")
		}
		select {
		case pc := <-p.idle:
			if pc == nil {
				continue
			}
			if time.Since(pc.lastUsed) > defaultIdleTimeout {
				p.closeConn(pc)
				continue
			}
			// No PING on borrow: extra RTT would defeat TTFT pooling gains.
			// Dead conns are discarded on the next command error path.
			_ = pc.SetDeadline(time.Time{})
			return pc, nil
		default:
			pc, err := p.dial(ctx)
			if err != nil {
				p.releaseSem()
				return nil, err
			}
			return pc, nil
		}
	}
}

func (p *connPool) dial(ctx context.Context) (*pooledConn, error) {
	conn, err := p.dialer.DialContext(ctx, "tcp", p.addr)
	if err != nil {
		return nil, err
	}
	deadline := deadlineFrom(ctx, defaultCmdTimeout)
	_ = conn.SetDeadline(deadline)
	reader := bufio.NewReader(conn)
	if p.password != "" {
		if err := writeRESP(conn, "AUTH", p.password); err != nil {
			_ = conn.Close()
			return nil, err
		}
		if _, err := readRESPValue(reader); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	if p.db != "" && p.db != "0" {
		if err := writeRESP(conn, "SELECT", p.db); err != nil {
			_ = conn.Close()
			return nil, err
		}
		if _, err := readRESPValue(reader); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	_ = conn.SetDeadline(time.Time{})
	pc := &pooledConn{Conn: conn, reader: reader, lastUsed: time.Now()}
	p.mu.Lock()
	if p.closed.Load() {
		p.mu.Unlock()
		_ = conn.Close()
		return nil, errors.New("redis pool closed")
	}
	if p.active == nil {
		p.active = make(map[*pooledConn]struct{})
	}
	p.active[pc] = struct{}{}
	p.mu.Unlock()
	return pc, nil
}

func (p *connPool) closeConn(pc *pooledConn) {
	if pc == nil {
		return
	}
	if p != nil {
		p.mu.Lock()
		delete(p.active, pc)
		p.mu.Unlock()
	}
	if pc.Conn != nil {
		_ = pc.Conn.Close()
	}
}

func (p *connPool) put(pc *pooledConn) {
	defer p.releaseSem()
	if pc == nil {
		return
	}
	if p == nil || p.closed.Load() {
		p.closeConn(pc)
		return
	}
	_ = pc.SetDeadline(time.Time{})
	pc.lastUsed = time.Now()
	// Reset any leftover buffered data (should be empty after clean command).
	if pc.reader.Buffered() > 0 {
		_, _ = pc.reader.Discard(pc.reader.Buffered())
	}
	select {
	case p.idle <- pc:
		if p.closed.Load() {
			select {
			case dead := <-p.idle:
				p.closeConn(dead)
			default:
			}
		}
	default:
		p.closeConn(pc)
	}
}

func (p *connPool) discard(pc *pooledConn) {
	defer p.releaseSem()
	if pc != nil {
		p.closeConn(pc)
	}
}

func (p *connPool) Close() error {
	if p == nil {
		return nil
	}
	p.closed.Store(true)
	for {
		select {
		case pc := <-p.idle:
			if pc != nil {
				p.closeConn(pc)
			}
		default:
			goto closeActive
		}
	}
closeActive:
	p.mu.Lock()
	for pc := range p.active {
		if pc != nil && pc.Conn != nil {
			_ = pc.Conn.Close()
		}
		delete(p.active, pc)
	}
	p.mu.Unlock()
	return nil
}

func deadlineFrom(ctx context.Context, fallback time.Duration) time.Time {
	if ctx != nil {
		if deadline, ok := ctx.Deadline(); ok {
			return deadline
		}
	}
	return time.Now().Add(fallback)
}

func parseRedisURL(raw string) (addr, password, db string, err error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", "", err
	}
	if parsed.Scheme != "redis" && parsed.Scheme != "rediss" {
		return "", "", "", fmt.Errorf("unsupported Redis URL scheme %q", parsed.Scheme)
	}
	if parsed.Scheme == "rediss" {
		return "", "", "", errors.New("rediss is not supported by the built-in lightweight readiness client")
	}
	addr = parsed.Host
	if !strings.Contains(addr, ":") {
		addr += ":6379"
	}
	if parsed.User != nil {
		password, _ = parsed.User.Password()
		if password == "" {
			password = parsed.User.Username()
		}
	}
	db = strings.Trim(parsed.Path, "/")
	if db == "" {
		db = "0"
	}
	return addr, password, db, nil
}

func writeRESP(conn net.Conn, args ...string) error {
	var b strings.Builder
	b.WriteString("*")
	b.WriteString(strconv.Itoa(len(args)))
	b.WriteString("\r\n")
	for _, arg := range args {
		b.WriteString("$")
		b.WriteString(strconv.Itoa(len(arg)))
		b.WriteString("\r\n")
		b.WriteString(arg)
		b.WriteString("\r\n")
	}
	_, err := conn.Write([]byte(b.String()))
	return err
}

func readRESPValue(reader *bufio.Reader) (any, error) {
	prefix, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	switch prefix {
	case '+':
		line, err := reader.ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	case '-':
		line, _ := reader.ReadString('\n')
		return nil, errors.New(strings.TrimRight(line, "\r\n"))
	case ':':
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseInt(strings.TrimRight(line, "\r\n"), 10, 64)
		return n, err
	case '$':
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		length, err := strconv.Atoi(strings.TrimRight(line, "\r\n"))
		if err != nil {
			return nil, err
		}
		if length < 0 {
			return nil, nil
		}
		buf := make([]byte, length+2)
		if _, err := ioReadFull(reader, buf); err != nil {
			return nil, err
		}
		return string(buf[:length]), nil
	case '*':
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		count, err := strconv.Atoi(strings.TrimRight(line, "\r\n"))
		if err != nil {
			return nil, err
		}
		if count < 0 {
			return nil, nil
		}
		out := make([]string, 0, count)
		for i := 0; i < count; i++ {
			item, err := readRESPValue(reader)
			if err != nil {
				return nil, err
			}
			switch v := item.(type) {
			case nil:
				out = append(out, "")
			case string:
				out = append(out, v)
			case int64:
				out = append(out, strconv.FormatInt(v, 10))
			default:
				out = append(out, fmt.Sprint(v))
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unexpected RESP prefix %q", prefix)
	}
}

// ioReadFull is a tiny local helper so client.go stays free of io import churn in tests.
func ioReadFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		nn, err := r.Read(buf[n:])
		n += nn
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
