// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// Cache defaults.
const (
	DefaultIdleTTL       = 15 * time.Minute
	DefaultSweepInterval = time.Minute
)

// A Key identifies a cache entry.
type Key struct {
	// Identity is "<kind>/<namespace>/<name>" of the ProviderConfig.
	Identity string
	// Version changes whenever an input to the connection options changes:
	// the config's UID and generation, and the resourceVersion of every
	// Secret that went into the options. Token files and token Secrets are
	// read on every dial and are not part of it.
	Version string
}

// A Client is a reference to a cached connection pool. External clients use
// it for one reconcile and Close it in Disconnect.
type Client interface {
	DB
	// Info returns the server version, the identity's user name and the
	// server UUID, probed when the pool was created.
	Info() ServerInfo
	// Capabilities returns the pool's capability cache.
	Capabilities() *Capabilities
	// ProbeServer records the UUID of the server currently reached, and
	// returns every server UUID seen recently behind the endpoint.
	ProbeServer(ctx context.Context) ([]string, error)
	// Close releases the reference.
	Close() error
}

// Cache holds one connection pool per ProviderConfig identity. An entry stays
// valid while its key's Version matches; when it changes, the old entry is
// marked stale and closed once its last reference is released. Entries that
// have not been used for the idle TTL (for example of a deleted config) are
// closed by Sweep.
type Cache struct {
	open    Opener
	now     func() time.Time
	idleTTL time.Duration

	mu      sync.Mutex
	entries map[string]*entry
	retired map[*entry]struct{}
}

// A CacheOption configures a Cache.
type CacheOption func(*Cache)

// WithClock sets the clock. Used by tests.
func WithClock(now func() time.Time) CacheOption { return func(c *Cache) { c.now = now } }

// WithIdleTTL sets how long an unused entry is kept.
func WithIdleTTL(d time.Duration) CacheOption { return func(c *Cache) { c.idleTTL = d } }

// NewCache returns a cache that opens pools with open (OpenDriver in
// production).
func NewCache(open Opener, opts ...CacheOption) *Cache {
	c := &Cache{
		open:    open,
		now:     time.Now,
		idleTTL: DefaultIdleTTL,
		entries: map[string]*entry{},
		retired: map[*entry]struct{}{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

type entry struct {
	key     Key
	cfg     Config
	conn    Conn
	info    ServerInfo
	caps    *Capabilities
	servers *ServerSet

	refs     int
	stale    bool
	closed   bool
	lastUsed time.Time
}

// errStale is returned by renew when the pool it was asked to replace is no
// longer the identity's current pool and no pool of the same version exists.
var errStale = errors.New("the connection pool was replaced by a newer configuration")

// Acquire returns a reference to the pool for key, creating it if needed. A
// new pool probes ServerInfo before it is returned; with token
// authentication a 516 there invalidates the token and is retried once.
func (c *Cache) Acquire(ctx context.Context, key Key, cfg Config) (Client, error) {
	c.mu.Lock()
	if e, ok := c.entries[key.Identity]; ok {
		if e.key.Version == key.Version && !e.stale {
			e.refs++
			e.lastUsed = c.now()
			c.mu.Unlock()
			return &Handle{cache: c, e: e}, nil
		}
		c.retireLocked(e)
	}
	c.mu.Unlock()

	e, err := c.build(ctx, key, cfg, true)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.entries[key.Identity]; ok {
		if cur.key.Version == key.Version && !cur.stale {
			// A concurrent Acquire won the race. Use its pool.
			cur.refs++
			cur.lastUsed = c.now()
			_ = e.conn.Close()
			return &Handle{cache: c, e: cur}, nil
		}
		c.retireLocked(cur)
	}
	e.refs = 1
	e.lastUsed = c.now()
	c.entries[key.Identity] = e
	return &Handle{cache: c, e: e}, nil
}

// build opens and probes a new pool. With retry and token authentication, a
// 516 from the probe invalidates the token and is retried once.
func (c *Cache) build(ctx context.Context, key Key, cfg Config, retry bool) (*entry, error) {
	conn, info, err := c.openAndProbe(ctx, cfg)
	if retry && err != nil && IsAuthenticationFailed(err) && cfg.TokenAuth() {
		cfg.TokenSource.Invalidate()
		conn, info, err = c.openAndProbe(ctx, cfg)
	}
	if err != nil {
		return nil, WithDefaultReason(err)
	}
	window := cfg.ConnMaxLifetime
	if window <= 0 {
		window = DefaultConnMaxLifetime
	}
	e := &entry{key: key, cfg: cfg, conn: conn, info: info, caps: NewCapabilities(), servers: NewServerSet(window)}
	e.servers.Add(info.ServerUUID)
	return e, nil
}

func (c *Cache) openAndProbe(ctx context.Context, cfg Config) (Conn, ServerInfo, error) {
	conn, err := c.open(cfg)
	if err != nil {
		return nil, ServerInfo{}, err
	}
	info, err := ProbeInfo(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, ServerInfo{}, err
	}
	return conn, info, nil
}

// retireLocked marks e stale and closes it if it is unused. c.mu must be held.
func (c *Cache) retireLocked(e *entry) {
	if cur, ok := c.entries[e.key.Identity]; ok && cur == e {
		delete(c.entries, e.key.Identity)
	}
	e.stale = true
	if e.refs <= 0 {
		c.closeLocked(e)
		return
	}
	c.retired[e] = struct{}{}
}

func (c *Cache) closeLocked(e *entry) {
	delete(c.retired, e)
	if e.closed {
		return
	}
	e.closed = true
	_ = e.conn.Close()
}

func (c *Cache) release(e *entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.refs--
	e.lastUsed = c.now()
	if e.stale && e.refs <= 0 {
		c.closeLocked(e)
	}
}

// renew replaces a pool after an authentication failure, for the operation
// that failed on it. The caller has already invalidated the token.
//
//   - If old is still the identity's current pool, it is retired and rebuilt
//     from the same configuration, without another 516 retry: one operation
//     dials at most once more.
//   - If a pool of the same version already replaced it (a concurrent
//     renew), that pool is used.
//   - Otherwise the configuration changed meanwhile, or the pool was
//     retired, and errStale is returned. A stale configuration (an old
//     password or token source, or an old endpoint) never replaces a newer
//     pool; the next reconcile uses the new configuration.
func (c *Cache) renew(ctx context.Context, old *entry) (*Handle, error) {
	c.mu.Lock()
	cur, ok := c.entries[old.key.Identity]
	switch {
	case ok && cur == old:
		c.retireLocked(old)
	case ok && cur.key.Version == old.key.Version && !cur.stale:
		cur.refs++
		cur.lastUsed = c.now()
		c.mu.Unlock()
		return &Handle{cache: c, e: cur}, nil
	default:
		c.mu.Unlock()
		return nil, errStale
	}
	c.mu.Unlock()

	e, err := c.build(ctx, old.key, old.cfg, false)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.entries[old.key.Identity]; ok {
		_ = e.conn.Close()
		if cur.key.Version == old.key.Version && !cur.stale {
			// A concurrent renew of the same version won the race.
			cur.refs++
			cur.lastUsed = c.now()
			return &Handle{cache: c, e: cur}, nil
		}
		// A newer configuration was installed while this pool was built.
		return nil, errStale
	}
	e.refs = 1
	e.lastUsed = c.now()
	c.entries[old.key.Identity] = e
	return &Handle{cache: c, e: e}, nil
}

// Sweep closes entries that have been unused for the idle TTL.
func (c *Cache) Sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for _, e := range c.entries {
		if e.refs <= 0 && now.Sub(e.lastUsed) > c.idleTTL {
			c.retireLocked(e)
		}
	}
}

// Close closes every pool, including ones still referenced.
func (c *Cache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		e.stale = true
		c.closeLocked(e)
	}
	for e := range c.retired {
		c.closeLocked(e)
	}
	c.entries = map[string]*entry{}
}

// Start runs Sweep every minute until ctx is done, then closes every pool. It
// implements controller-runtime's manager.Runnable.
func (c *Cache) Start(ctx context.Context) error {
	t := time.NewTicker(DefaultSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.Close()
			return nil
		case <-t.C:
			c.Sweep()
		}
	}
}

// NeedLeaderElection reports false: sweeping is safe on every replica.
func (c *Cache) NeedLeaderElection() bool { return false }

// Handle is a reference to a cache entry. It is not safe for concurrent use
// across reconciles; each reconcile acquires its own.
type Handle struct {
	cache *Cache

	mu     sync.Mutex
	e      *entry
	closed bool
}

func (h *Handle) entry() *entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.e
}

// do runs op and, with token authentication, retries it once on 516 with a
// fresh token and a fresh pool. That is safe for every statement, including
// DDL: 516 is returned during authentication, before any statement runs.
func (h *Handle) do(ctx context.Context, op func(DB) error) error {
	e := h.entry()
	err := op(e.conn)
	if err == nil || !IsAuthenticationFailed(err) || !e.cfg.TokenAuth() {
		return err
	}
	e.cfg.TokenSource.Invalidate()
	fresh, rerr := h.cache.renew(ctx, e)
	if errors.Is(rerr, errStale) {
		return reason.Wrap(reason.AuthenticationFailed, err)
	}
	if rerr != nil {
		return reason.Wrap(reason.AuthenticationFailed, rerr)
	}
	// The new handle's reference moves to h.
	h.mu.Lock()
	old := h.e
	h.e = fresh.e
	h.mu.Unlock()
	h.cache.release(old)

	err = op(fresh.e.conn)
	if IsAuthenticationFailed(err) {
		return reason.Wrap(reason.AuthenticationFailed, err)
	}
	return err
}

// Exec implements DB.
func (h *Handle) Exec(ctx context.Context, stmt string) error {
	return h.do(ctx, func(db DB) error { return db.Exec(ctx, stmt) })
}

// Query implements DB.
func (h *Handle) Query(ctx context.Context, query string, params Params, fn func(Row) error) error {
	return h.do(ctx, func(db DB) error { return db.Query(ctx, query, params, fn) })
}

// Info implements Client.
func (h *Handle) Info() ServerInfo { return h.entry().info }

// Capabilities implements Client.
func (h *Handle) Capabilities() *Capabilities { return h.entry().caps }

// ProbeServer implements Client.
func (h *Handle) ProbeServer(ctx context.Context) ([]string, error) {
	var id string
	if err := QueryRow(ctx, h, "SELECT toString(serverUUID())", nil, &id); err != nil {
		return nil, WithDefaultReason(err)
	}
	e := h.entry()
	e.servers.Add(id)
	return e.servers.Servers(), nil
}

// Close implements Client. It is idempotent.
func (h *Handle) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	e := h.e
	h.mu.Unlock()
	h.cache.release(e)
	return nil
}
