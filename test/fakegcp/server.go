// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package fakegcp

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// Defaults of a new Server.
const (
	// DefaultIssuer is the iss of Google ID tokens.
	DefaultIssuer = "https://accounts.google.com"
	// DefaultTTL is the lifetime of Google ID tokens.
	DefaultTTL = time.Hour
	// DefaultHang is how long a request waits in a timeout outage before the
	// fake gives up on it (the client normally gives up first: the Go
	// metadata client times out after 5 s).
	DefaultHang = 30 * time.Second
	// DefaultJWKSMaxAge is the Cache-Control max-age of the JWKS, in the
	// range Google serves.
	DefaultJWKSMaxAge = 6 * time.Hour

	DefaultEmail            = "fake-sa@example-project.iam.gserviceaccount.com"
	DefaultUniqueID         = "100000000000000000001"
	DefaultProjectID        = "example-project"
	DefaultNumericProjectID = "123456789012"
	DefaultZone             = "europe-west1-b"
	DefaultInstanceID       = "1234567890123456789"
	DefaultInstanceName     = "gke-example-default-pool-0a1b2c3d-x1y2"
)

// Identity is the Google service account the fake serves tokens for.
type Identity struct {
	// Email is the service account's email, e.g.
	// provider@example-project.iam.gserviceaccount.com. It becomes the email
	// claim (format=full).
	Email string `json:"email"`
	// UniqueID is the service account's numeric unique ID. It becomes the sub
	// and azp claims. A service account that is deleted and recreated with
	// the same email gets a new unique ID.
	UniqueID string `json:"uniqueId"`
}

// ComputeEngine adds the google.compute_engine claim to format=full tokens,
// as the metadata server of a Compute Engine VM does. Without it the fake
// behaves like the GKE metadata server with Workload Identity, whose tokens
// are service account ID tokens without instance details.
type ComputeEngine struct {
	Zone              string
	InstanceID        string
	InstanceName      string
	CreationTimestamp int64
	// LicenseIDs are added as license_id when licenses=TRUE.
	LicenseIDs []string
}

// Options configure a Server. Zero values select the defaults above.
type Options struct {
	Identity         Identity
	ProjectID        string
	NumericProjectID string
	Issuer           string
	// TTL is exp - iat of issued tokens.
	TTL time.Duration
	// ClockOffset shifts the fake's clock (iat and exp) against the real
	// one, to simulate skew between the metadata server and ClickHouse.
	ClockOffset time.Duration
	// SigningKeys are published at start; the first one signs. A key is
	// generated if there is none. Supplying keys keeps the JWKS stable
	// across restarts of the binary.
	SigningKeys []SigningKey
	// KeyBits is the size of generated RSA keys (at least 2048).
	KeyBits int
	// ComputeEngine, if set, makes format=full tokens carry instance
	// details (a Compute Engine VM instead of GKE Workload Identity).
	ComputeEngine *ComputeEngine
	// Hang bounds a request in a timeout outage.
	Hang time.Duration
	// JWKSMaxAge is the Cache-Control max-age of the JWKS.
	JWKSMaxAge time.Duration
	// Now is the clock; time.Now if nil.
	Now func() time.Time
	// Logger receives one line per request and per issued token (key IDs
	// and claim summaries only, never tokens or keys). Discarded if nil.
	Logger *slog.Logger
}

// Fault makes the next tokens defective in one way each.
type Fault string

// Faults.
const (
	FaultNone Fault = ""
	// FaultWrongAudience: aud is another relying party's audience.
	FaultWrongAudience Fault = "wrong-audience"
	// FaultWrongIssuer: iss is not https://accounts.google.com.
	FaultWrongIssuer Fault = "wrong-issuer"
	// FaultEmailUnverified: email_verified is false.
	FaultEmailUnverified Fault = "email-unverified"
	// FaultNoEmail: no email claims, as with format=standard.
	FaultNoEmail Fault = "no-email"
	// FaultWrongTyp: the typ header is at+jwt instead of JWT.
	FaultWrongTyp Fault = "wrong-typ"
	// FaultNoTyp: no typ header.
	FaultNoTyp Fault = "no-typ"
	// FaultUnknownKid: signed with a key that is not in the JWKS, under its
	// own kid.
	FaultUnknownKid Fault = "unknown-kid"
	// FaultBadSignature: the active key's kid, but signed with another key.
	FaultBadSignature Fault = "bad-signature"
	// FaultAlgNone: alg none and an empty signature.
	FaultAlgNone Fault = "alg-none"
	// FaultHS256Confusion: alg HS256 with the active kid, HMAC-signed with
	// the active key's public key PEM (key-confusion attack).
	FaultHS256Confusion Fault = "hs256-confusion"
	// FaultExpired: exp one hour in the past.
	FaultExpired Fault = "expired"
	// FaultIssuedInFuture: iat one hour in the future.
	FaultIssuedInFuture Fault = "issued-in-future"
)

// Faults returns every fault except FaultNone.
func Faults() []Fault {
	return []Fault{
		FaultWrongAudience, FaultWrongIssuer, FaultEmailUnverified, FaultNoEmail,
		FaultWrongTyp, FaultNoTyp, FaultUnknownKid, FaultBadSignature,
		FaultAlgNone, FaultHS256Confusion, FaultExpired, FaultIssuedInFuture,
	}
}

func (f Fault) valid() bool { return f == FaultNone || slices.Contains(Faults(), f) }

// Target selects the endpoints an outage affects.
type Target string

// Targets.
const (
	// TargetMetadata: every /computeMetadata/ path (not the detection root).
	TargetMetadata Target = "metadata"
	// TargetIdentity: only the identity and token endpoints.
	TargetIdentity Target = "identity"
	// TargetJWKS: the JWKS endpoint.
	TargetJWKS Target = "jwks"
)

func (t Target) valid() bool {
	return t == TargetMetadata || t == TargetIdentity || t == TargetJWKS
}

// OutageMode is how an endpoint fails.
type OutageMode string

// Outage modes.
const (
	OutageNone OutageMode = ""
	// Outage500 answers 500 Internal Server Error (the Go metadata client
	// retries 5xx five times).
	Outage500 OutageMode = "500"
	// Outage503 answers 503 Service Unavailable, as during maintenance.
	Outage503 OutageMode = "503"
	// OutageForbidden answers 403 with a multi-line message, like the GKE
	// metadata server when the Kubernetes service account may not
	// impersonate its Google service account. Not retried by the client.
	OutageForbidden OutageMode = "403"
	// OutageTimeout does not answer until the client gives up (or Hang).
	OutageTimeout OutageMode = "timeout"
)

func (m OutageMode) valid() bool {
	return m == OutageNone || m == Outage500 || m == Outage503 || m == OutageForbidden || m == OutageTimeout
}

type outage struct {
	mode OutageMode
	left int // requests still affected; < 0 means until cleared
}

// Request is one request the fake served, for assertions.
type Request struct {
	Time     time.Time `json:"time"`
	Method   string    `json:"method"`
	Path     string    `json:"path"`
	RawQuery string    `json:"rawQuery,omitempty"`
	Status   int       `json:"status"`
	// Note says why a request was refused or failed, e.g. "outage 500".
	Note string `json:"note,omitempty"`
}

// Issued summarizes a token the fake signed. It never holds the token.
type Issued struct {
	Time     time.Time `json:"time"`
	Kid      string    `json:"kid"`
	Alg      string    `json:"alg"`
	Typ      string    `json:"typ,omitempty"`
	Audience string    `json:"aud"`
	Issuer   string    `json:"iss"`
	Email    string    `json:"email,omitempty"`
	Subject  string    `json:"sub"`
	IssuedAt time.Time `json:"iat"`
	Expiry   time.Time `json:"exp"`
	Fault    Fault     `json:"fault,omitempty"`
	// Via is "metadata" for tokens served by the identity endpoint and
	// "mint" for Mint.
	Via string `json:"via"`
}

const maxLog = 4096

// Server is a fake metadata server and JWKS endpoint. It is safe for
// concurrent use.
type Server struct {
	opts Options
	log  *slog.Logger
	done chan struct{}

	mu       sync.Mutex
	identity Identity
	ttl      time.Duration
	offset   time.Duration
	keys     []*key // in creation order
	active   *key
	stranger *key // never published; signs unknown-kid and bad-signature tokens
	fault    Fault
	faultN   int // tokens still affected; < 0 means until cleared
	outages  map[Target]*outage
	requests []Request
	issued   []Issued
	closed   bool
}

// New returns a Server whose JWKS holds Options.SigningKeys (or one generated
// key); the first key signs.
func New(opts Options) (*Server, error) {
	if opts.Identity.Email == "" {
		opts.Identity.Email = DefaultEmail
	}
	if opts.Identity.UniqueID == "" {
		opts.Identity.UniqueID = DefaultUniqueID
	}
	if opts.ProjectID == "" {
		opts.ProjectID = DefaultProjectID
	}
	if opts.NumericProjectID == "" {
		opts.NumericProjectID = DefaultNumericProjectID
	}
	if opts.Issuer == "" {
		opts.Issuer = DefaultIssuer
	}
	if opts.TTL == 0 {
		opts.TTL = DefaultTTL
	}
	if opts.Hang == 0 {
		opts.Hang = DefaultHang
	}
	if opts.JWKSMaxAge == 0 {
		opts.JWKSMaxAge = DefaultJWKSMaxAge
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if c := opts.ComputeEngine; c != nil {
		if c.Zone == "" {
			c.Zone = DefaultZone
		}
		if c.InstanceID == "" {
			c.InstanceID = DefaultInstanceID
		}
		if c.InstanceName == "" {
			c.InstanceName = DefaultInstanceName
		}
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	var keys []*key
	for _, sk := range opts.SigningKeys {
		k, err := keyFrom(sk)
		if err != nil {
			return nil, err
		}
		for _, other := range keys {
			if other.kid == k.kid {
				return nil, fmt.Errorf("duplicate kid %q", k.kid)
			}
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		k, err := newKey(opts.KeyBits)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	for _, k := range keys {
		k.published = true
	}
	stranger, err := newKey(opts.KeyBits)
	if err != nil {
		return nil, err
	}
	return &Server{
		opts:     opts,
		log:      log,
		done:     make(chan struct{}),
		identity: opts.Identity,
		ttl:      opts.TTL,
		offset:   opts.ClockOffset,
		keys:     keys,
		active:   keys[0],
		stranger: stranger,
		outages:  map[Target]*outage{},
	}, nil
}

// Close releases requests that hang in a timeout outage. The handlers keep
// working afterwards, but outages no longer hang.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
}

// Identity returns the identity the metadata endpoint serves tokens for.
func (s *Server) Identity() Identity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identity
}

// SetIdentity changes the identity of the next tokens, e.g. to another
// service account (an attacker) or to one recreated with the same email.
func (s *Server) SetIdentity(id Identity) error {
	if id.Email == "" || id.UniqueID == "" {
		return errors.New("identity needs an email and a unique ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.identity = id
	s.log.Info("identity changed", "email", id.Email, "uniqueId", id.UniqueID)
	return nil
}

// SetTTL changes the lifetime (exp - iat) of the next tokens.
func (s *Server) SetTTL(ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("ttl must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ttl = ttl
	s.log.Info("token ttl changed", "ttl", ttl)
	return nil
}

// SetClockOffset shifts iat and exp of the next tokens against the real
// clock.
func (s *Server) SetClockOffset(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offset = d
	s.log.Info("clock offset changed", "offset", d)
}

// SetFault makes the next n tokens served by the identity endpoint
// defective; n <= 0 means every token until SetFault(FaultNone, 0).
func (s *Server) SetFault(f Fault, n int) error {
	if !f.valid() {
		return fmt.Errorf("unknown fault %q (want one of %v)", f, Faults())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fault = f
	s.faultN = n
	if n <= 0 {
		s.faultN = -1
	}
	s.log.Info("fault set", "fault", string(f), "count", n)
	return nil
}

// SetOutage makes the target's next n requests fail with mode; n <= 0
// means every request until SetOutage(target, OutageNone, 0).
func (s *Server) SetOutage(target Target, mode OutageMode, n int) error {
	if !target.valid() {
		return fmt.Errorf("unknown target %q (want metadata, identity or jwks)", target)
	}
	if !mode.valid() {
		return fmt.Errorf("unknown outage mode %q (want 500, 503, 403 or timeout)", mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if mode == OutageNone {
		delete(s.outages, target)
	} else {
		left := n
		if n <= 0 {
			left = -1
		}
		s.outages[target] = &outage{mode: mode, left: left}
	}
	s.log.Info("outage set", "target", string(target), "mode", string(mode), "count", n)
	return nil
}

// Reset clears faults and outages and restores the identity, TTL and clock
// offset of Options. Keys are kept.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.identity = s.opts.Identity
	s.ttl = s.opts.TTL
	s.offset = s.opts.ClockOffset
	s.fault, s.faultN = FaultNone, 0
	s.outages = map[Target]*outage{}
	s.log.Info("reset")
}

// ActiveKID returns the kid of the key that signs new tokens.
func (s *Server) ActiveKID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active.kid
}

// PublishedKIDs returns the kids in the JWKS, oldest first.
func (s *Server) PublishedKIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, k := range s.keys {
		if k.published {
			out = append(out, k.kid)
		}
	}
	return out
}

// AddKey generates a key and publishes it without signing with it yet, as
// Google publishes a new key ahead of using it. It returns the new kid.
func (s *Server) AddKey() (string, error) {
	k, err := newKey(s.opts.KeyBits)
	if err != nil {
		return "", err
	}
	k.published = true
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, k)
	s.log.Info("key added", "kid", k.kid)
	return k.kid, nil
}

// Activate makes a published key sign the next tokens.
func (s *Server) Activate(kid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.keyLocked(kid)
	if k == nil {
		return fmt.Errorf("no key %q", kid)
	}
	if !k.published {
		return fmt.Errorf("key %q is retired", kid)
	}
	s.active = k
	s.log.Info("key activated", "kid", kid)
	return nil
}

// Rotate generates a key, publishes it and signs the next tokens with it at
// once, without the head start Google gives new keys. The previous key
// stays published. It returns the new kid.
func (s *Server) Rotate() (string, error) {
	kid, err := s.AddKey()
	if err != nil {
		return "", err
	}
	return kid, s.Activate(kid)
}

// Retire removes a key from the JWKS. The active key cannot be retired. A
// retired key can still sign tokens through Mint with MintOptions.KID.
func (s *Server) Retire(kid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.keyLocked(kid)
	if k == nil {
		return fmt.Errorf("no key %q", kid)
	}
	if k == s.active {
		return fmt.Errorf("key %q is active; activate another key first", kid)
	}
	k.published = false
	s.log.Info("key retired", "kid", kid)
	return nil
}

func (s *Server) keyLocked(kid string) *key {
	for _, k := range s.keys {
		if k.kid == kid {
			return k
		}
	}
	return nil
}

// JWKS returns the published keys.
func (s *Server) JWKS() JWKS {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jwksLocked()
}

func (s *Server) jwksLocked() JWKS {
	set := JWKS{Keys: []JWK{}}
	for _, k := range s.keys {
		if k.published {
			set.Keys = append(set.Keys, k.jwk())
		}
	}
	return set
}

// Requests returns the requests served so far (at most the last 4096).
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// Count returns how many requests for path were served (any status).
func (s *Server) Count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if r.Path == path {
			n++
		}
	}
	return n
}

// Issued returns summaries of the tokens signed so far (at most the last
// 4096).
func (s *Server) Issued() []Issued {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.issued)
}

func (s *Server) record(r Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == maxLog {
		s.requests = slices.Delete(s.requests, 0, 1)
	}
	s.requests = append(s.requests, r)
}

// takeOutage returns the outage that applies to a request for target (and,
// for identity endpoints, TargetIdentity), consuming one count.
func (s *Server) takeOutage(targets ...Target) OutageMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range targets {
		o, ok := s.outages[t]
		if !ok {
			continue
		}
		if o.left > 0 {
			o.left--
			if o.left == 0 {
				delete(s.outages, t)
			}
		}
		return o.mode
	}
	return OutageNone
}

// takeFault returns the fault for the next metadata token, consuming one
// count.
func (s *Server) takeFault() Fault {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.fault
	if f != FaultNone && s.faultN > 0 {
		s.faultN--
		if s.faultN == 0 {
			s.fault = FaultNone
		}
	}
	return f
}

// State is a snapshot of the fake's configuration, for the control API.
type State struct {
	Identity      Identity `json:"identity"`
	TTL           string   `json:"ttl"`
	ClockOffset   string   `json:"clockOffset"`
	ActiveKID     string   `json:"activeKid"`
	PublishedKIDs []string `json:"publishedKids"`
	Fault         Fault    `json:"fault,omitempty"`
	// FaultCount is how many tokens the fault still affects; -1 means
	// until cleared.
	FaultCount int                   `json:"faultCount,omitempty"`
	Outages    map[Target]OutageMode `json:"outages,omitempty"`
	Requests   int                   `json:"requests"`
	Issued     int                   `json:"issued"`
}

// State returns a snapshot of the configuration.
func (s *Server) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{
		Identity:    s.identity,
		TTL:         s.ttl.String(),
		ClockOffset: s.offset.String(),
		ActiveKID:   s.active.kid,
		Fault:       s.fault,
		FaultCount:  s.faultN,
		Outages:     map[Target]OutageMode{},
		Requests:    len(s.requests),
		Issued:      len(s.issued),
	}
	for _, k := range s.keys {
		if k.published {
			st.PublishedKIDs = append(st.PublishedKIDs, k.kid)
		}
	}
	for t, o := range s.outages {
		st.Outages[t] = o.mode
	}
	return st
}
