// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// testConn is a fake Conn. While its opener's failLeft is positive, statements
// and probes fail with the opener's failCode.
type testConn struct {
	id     int
	o      *opener
	mu     sync.Mutex
	closed bool
}

func (c *testConn) fail() error {
	c.o.mu.Lock()
	defer c.o.mu.Unlock()
	if c.o.failLeft > 0 {
		c.o.failLeft--
		return &ServerError{Code: c.o.failCode, Message: "failed"}
	}
	return nil
}

func (c *testConn) Exec(context.Context, string) error { return c.fail() }

func (c *testConn) Query(_ context.Context, q string, _ Params, fn func(Row) error) error {
	if err := c.fail(); err != nil {
		return err
	}
	if q == InfoQuery {
		return fn(infoRow{})
	}
	return nil
}

func (c *testConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *testConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

type infoRow struct{}

func (infoRow) Scan(dest ...any) error {
	if len(dest) != 3 {
		return errors.New("want 3 destinations")
	}
	*dest[0].(*string) = "26.8.23.6"
	*dest[1].(*string) = "provider"
	*dest[2].(*string) = "server-1"
	return nil
}

type countingTS struct {
	mu          sync.Mutex
	invalidated int
}

func (s *countingTS) Token(context.Context) (string, error) { return "h.p.s", nil }
func (s *countingTS) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated++
}

type opener struct {
	mu       sync.Mutex
	conns    []*testConn
	failLeft int
	failCode int32
}

func (o *opener) open(Config) (Conn, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	c := &testConn{id: len(o.conns) + 1, o: o}
	o.conns = append(o.conns, c)
	return c, nil
}

func (o *opener) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.conns)
}

var pwCfg = Config{Host: "h", Port: 9000, Protocol: ProtocolNative, Username: "u", Password: "p"}

func TestCacheReuseAndRefCountedClose(t *testing.T) {
	o := &opener{}
	c := NewCache(o.open)
	ctx := context.Background()
	k := Key{Identity: "ClusterProviderConfig//main", Version: "uid;1"}

	a, err := c.Acquire(ctx, k, pwCfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Acquire(ctx, k, pwCfg)
	if err != nil {
		t.Fatal(err)
	}
	if o.count() != 1 {
		t.Fatalf("same key must share one pool, opened %d", o.count())
	}
	if a.Info().Version != "26.8.23.6" || a.Info().CurrentUser != "provider" {
		t.Errorf("info = %+v", a.Info())
	}

	// The key changes (new generation): a new pool; the old one stays open
	// while referenced.
	k2 := Key{Identity: k.Identity, Version: "uid;2"}
	n, err := c.Acquire(ctx, k2, pwCfg)
	if err != nil {
		t.Fatal(err)
	}
	if o.count() != 2 {
		t.Fatalf("a changed key must open a new pool")
	}
	if o.conns[0].isClosed() {
		t.Fatal("a stale pool must stay open while referenced")
	}
	_ = a.Close()
	_ = a.Close() // idempotent
	if o.conns[0].isClosed() {
		t.Fatal("a stale pool must stay open until the last reference is released")
	}
	_ = b.Close()
	if !o.conns[0].isClosed() {
		t.Fatal("a stale pool must be closed when its last reference is released")
	}
	_ = n.Close()
	if o.conns[1].isClosed() {
		t.Fatal("the current pool must stay open when unused")
	}
}

func TestCacheSweep(t *testing.T) {
	o := &opener{}
	now := time.Now()
	c := NewCache(o.open, WithClock(func() time.Time { return now }), WithIdleTTL(15*time.Minute))
	ctx := context.Background()
	k := Key{Identity: "ProviderConfig/ns/pc", Version: "v"}

	h, _ := c.Acquire(ctx, k, pwCfg)
	now = now.Add(time.Hour)
	c.Sweep()
	if o.conns[0].isClosed() {
		t.Fatal("a referenced pool must not be swept")
	}
	_ = h.Close()
	now = now.Add(10 * time.Minute)
	c.Sweep()
	if o.conns[0].isClosed() {
		t.Fatal("a pool idle for less than the TTL must not be swept")
	}
	now = now.Add(6 * time.Minute)
	c.Sweep()
	if !o.conns[0].isClosed() {
		t.Fatal("a pool idle for longer than the TTL must be swept")
	}
	if _, err := c.Acquire(ctx, k, pwCfg); err != nil || o.count() != 2 {
		t.Fatalf("Acquire after sweep: %v, opened %d", err, o.count())
	}
	c.Close()
	if !o.conns[1].isClosed() {
		t.Fatal("Close must close every pool")
	}
}

func TestCacheRetryOn516WithTokens(t *testing.T) {
	ts := &countingTS{}
	cfg := Config{Host: "h", Port: 9440, Protocol: ProtocolNative, TokenSource: ts}
	o := &opener{}
	c := NewCache(o.open)
	ctx := context.Background()
	k := Key{Identity: "ClusterProviderConfig//jwt", Version: "v"}

	h, err := c.Acquire(ctx, k, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The next statement fails once with 516: the token is invalidated, the
	// pool is rebuilt and the statement is retried exactly once.
	o.mu.Lock()
	o.failLeft, o.failCode = 1, CodeAuthenticationFailed
	o.mu.Unlock()
	if err := h.Exec(ctx, "CREATE DATABASE `x`"); err != nil {
		t.Fatalf("Exec() after one 516 = %v, want success", err)
	}
	if ts.invalidated != 1 {
		t.Errorf("invalidated %d times, want 1", ts.invalidated)
	}
	if o.count() != 2 || !o.conns[0].isClosed() {
		t.Errorf("the failed pool must be replaced and closed (opened %d, first closed %t)", o.count(), o.conns[0].isClosed())
	}

	// Two consecutive 516s: AuthenticationFailed after one retry. The
	// rebuilt pool's probe consumes the second failure and is not retried
	// again: one operation invalidates the token once and dials once more.
	o.mu.Lock()
	o.failLeft = 2
	o.mu.Unlock()
	err = h.Exec(ctx, "SELECT 1")
	if !reason.Is(err, reason.AuthenticationFailed) {
		t.Fatalf("Exec() after repeated 516 = %v, want AuthenticationFailed", err)
	}
	if ts.invalidated != 2 || o.count() != 3 {
		t.Errorf("after the second operation: invalidated %d (want 2), opened %d (want 3)", ts.invalidated, o.count())
	}
	o.mu.Lock()
	left := o.failLeft
	o.mu.Unlock()
	if left != 0 {
		t.Errorf("%d scripted failures were not consumed", left)
	}
	_ = h.Close()
}

// TestCacheRenewNeverUsesStaleConfig: a 516 on a pool whose configuration
// has since changed must not rebuild that old configuration, nor retire the
// newer pool.
func TestCacheRenewNeverUsesStaleConfig(t *testing.T) {
	ts := &countingTS{}
	cfg := Config{Host: "h", Port: 9440, Protocol: ProtocolNative, TokenSource: ts}
	o := &opener{}
	c := NewCache(o.open)
	ctx := context.Background()
	id := "ClusterProviderConfig//jwt"

	old, err := c.Acquire(ctx, Key{Identity: id, Version: "v1"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := c.Acquire(ctx, Key{Identity: id, Version: "v2"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_ = newer.Close()

	o.mu.Lock()
	o.failLeft, o.failCode = 1, CodeAuthenticationFailed
	o.mu.Unlock()
	if err := old.Exec(ctx, "SELECT 1"); !reason.Is(err, reason.AuthenticationFailed) || !IsAuthenticationFailed(err) {
		t.Fatalf("Exec() on a stale pool = %v, want the original 516 as AuthenticationFailed", err)
	}
	if o.count() != 2 {
		t.Errorf("a stale configuration was rebuilt: opened %d pools, want 2", o.count())
	}
	if o.conns[1].isClosed() {
		t.Error("the newer pool was retired")
	}
	again, err := c.Acquire(ctx, Key{Identity: id, Version: "v2"}, cfg)
	if err != nil || o.count() != 2 {
		t.Fatalf("Acquire(v2) after the failure: %v, opened %d (want the existing pool)", err, o.count())
	}
	_ = again.Close()
	_ = old.Close()
	if !o.conns[0].isClosed() {
		t.Error("the stale pool must be closed once released")
	}
}

// TestCacheRenewReusesConcurrentRenewal: two handles on one pool both hit
// 516; the second reuses the pool the first one rebuilt.
func TestCacheRenewReusesConcurrentRenewal(t *testing.T) {
	ts := &countingTS{}
	cfg := Config{Host: "h", Port: 9440, Protocol: ProtocolNative, TokenSource: ts}
	o := &opener{}
	c := NewCache(o.open)
	ctx := context.Background()
	k := Key{Identity: "ClusterProviderConfig//jwt", Version: "v"}

	h1, _ := c.Acquire(ctx, k, cfg)
	h2, _ := c.Acquire(ctx, k, cfg)
	o.mu.Lock()
	o.failLeft, o.failCode = 1, CodeAuthenticationFailed
	o.mu.Unlock()
	if err := h1.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	o.mu.Lock()
	o.failLeft = 1
	o.mu.Unlock()
	// h2 still uses the first pool; its statement fails there once.
	if err := h2.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("Exec() on h2 = %v", err)
	}
	if o.count() != 2 {
		t.Errorf("opened %d pools, want 2 (the second renew reuses the first one's pool)", o.count())
	}
	_ = h1.Close()
	_ = h2.Close()
	if !o.conns[0].isClosed() || o.conns[1].isClosed() {
		t.Errorf("first closed %t (want true), second closed %t (want false)", o.conns[0].isClosed(), o.conns[1].isClosed())
	}
}

func TestCacheNoRetryWithPassword(t *testing.T) {
	o := &opener{}
	c := NewCache(o.open)
	ctx := context.Background()
	h, err := c.Acquire(ctx, Key{Identity: "x", Version: "v"}, pwCfg)
	if err != nil {
		t.Fatal(err)
	}
	o.mu.Lock()
	o.failLeft, o.failCode = 1, CodeAuthenticationFailed
	o.mu.Unlock()
	if err := h.Exec(ctx, "SELECT 1"); !IsAuthenticationFailed(err) {
		t.Fatalf("Exec() = %v, want the 516 unchanged", err)
	}
	if o.count() != 1 {
		t.Errorf("password auth must not rebuild the pool")
	}
}

func TestCacheProbe516RetriedOnceOnAcquire(t *testing.T) {
	ts := &countingTS{}
	cfg := Config{Host: "h", Port: 9440, Protocol: ProtocolNative, TokenSource: ts}
	o := &opener{failLeft: 1, failCode: CodeAuthenticationFailed}
	c := NewCache(o.open)
	if _, err := c.Acquire(context.Background(), Key{Identity: "x", Version: "v"}, cfg); err != nil {
		t.Fatalf("Acquire() = %v", err)
	}
	if ts.invalidated != 1 || o.count() != 2 || !o.conns[0].isClosed() {
		t.Errorf("invalidated %d, opened %d", ts.invalidated, o.count())
	}

	o2 := &opener{failLeft: 2, failCode: CodeAuthenticationFailed}
	c2 := NewCache(o2.open)
	if _, err := c2.Acquire(context.Background(), Key{Identity: "x", Version: "v"}, cfg); !reason.Is(err, reason.AuthenticationFailed) {
		t.Fatalf("Acquire() = %v, want AuthenticationFailed", err)
	}
}

func TestServerSetAndAccessStorage(t *testing.T) {
	now := time.Now()
	s := NewServerSet(30 * time.Minute)
	s.now = func() time.Time { return now }
	s.Add("b")
	s.Add("a")
	s.Add("")
	if got := s.Servers(); len(got) != 2 || got[0] != "a" {
		t.Fatalf("Servers() = %v", got)
	}
	now = now.Add(31 * time.Minute)
	s.Add("c")
	if got := s.Servers(); len(got) != 1 || got[0] != "c" {
		t.Fatalf("Servers() after window = %v", got)
	}

	if err := CheckAccessStorage("h:9000", StorageLocalDirectory, 1, []string{"a"}); err != nil {
		t.Errorf("one server with local_directory is fine: %v", err)
	}
	if err := CheckAccessStorage("h:9000", StorageLocalDirectory, 1, []string{"a", "b"}); !reason.Is(err, reason.AccessStorageNotReplicated) {
		t.Errorf("two servers with local_directory: %v", err)
	}
	if err := CheckAccessStorage("h:9000", StorageLocalDirectory, 2, nil); !reason.Is(err, reason.AccessStorageNotReplicated) {
		t.Errorf("a two-host cluster with local_directory: %v", err)
	}
	if err := CheckAccessStorage("h:9000", StorageReplicated, 4, []string{"a", "b"}); err != nil {
		t.Errorf("replicated storage is always fine: %v", err)
	}
}

// TestCacheStartLeavesPoolsOpen: the manager stops the cache before the
// controllers, so Start must not close the pools that running reconciles
// still use; Close does, once the manager has stopped.
func TestCacheStartLeavesPoolsOpen(t *testing.T) {
	o := &opener{}
	c := NewCache(o.open)
	h, err := c.Acquire(context.Background(), Key{Identity: "ClusterProviderConfig/cpc", Version: "v"}, pwCfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after its context ended")
	}
	if o.conns[0].isClosed() {
		t.Fatal("Start closed a pool that a reconcile still uses")
	}
	if err := h.Exec(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("a reconcile running at shutdown cannot use its pool: %v", err)
	}
	_ = h.Close()
	c.Close()
	if !o.conns[0].isClosed() {
		t.Fatal("Close must close every pool")
	}
}
