// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package fakegcp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testAudience = "https://clickhouse-unit.example.com"

var testIdentity = Identity{Email: "provider@example-project.iam.gserviceaccount.com", UniqueID: "100000000000000000042"}

func newTestServer(t *testing.T, opts Options) (*Server, *httptest.Server) {
	t.Helper()
	if opts.Identity == (Identity{}) {
		opts.Identity = testIdentity
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	t.Cleanup(s.Close) // runs first: releases hanging requests
	return s, hs
}

type response struct {
	status int
	header http.Header
	body   string
}

func get(t *testing.T, rawURL string, headers map[string]string) response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: res.StatusCode, header: res.Header, body: string(b)}
}

var googleFlavor = map[string]string{"Metadata-Flavor": "Google"}

func TestMetadataServer(t *testing.T) {
	_, hs := newTestServer(t, Options{})
	md := hs.URL + MetadataPrefix
	cases := []struct {
		name     string
		url      string
		headers  map[string]string
		status   int
		body     string // exact, if set
		contains string
		location string
	}{
		{name: "detection root needs no header", url: hs.URL + "/", status: 200, body: "computeMetadata/\n"},
		{name: "value without Metadata-Flavor", url: md + "project/project-id", status: 403, contains: "Metadata-Flavor"},
		{name: "wrong Metadata-Flavor", url: md + "project/project-id", headers: map[string]string{"Metadata-Flavor": "google-ish"}, status: 403},
		{name: "X-Forwarded-For is rejected", url: md + "project/project-id", headers: map[string]string{"Metadata-Flavor": "Google", "X-Forwarded-For": "203.0.113.7"}, status: 403, contains: "X-Forwarded-For"},
		{name: "X-Forwarded-For on root", url: hs.URL + "/", headers: map[string]string{"X-Forwarded-For": "203.0.113.7"}, status: 403},
		{name: "project id", url: md + "project/project-id", headers: googleFlavor, status: 200, body: DefaultProjectID},
		{name: "project id as JSON", url: md + "project/project-id?alt=json", headers: googleFlavor, status: 200, body: `"` + DefaultProjectID + `"`},
		{name: "numeric project id as JSON", url: md + "project/numeric-project-id?alt=json", headers: googleFlavor, status: 200, body: DefaultNumericProjectID},
		{name: "universe domain", url: md + "universe/universe-domain", headers: googleFlavor, status: 200, body: "googleapis.com"},
		{name: "root listing", url: md, headers: googleFlavor, status: 200, contains: "instance/\nproject/\n"},
		{name: "directory without slash", url: md + "project", headers: googleFlavor, status: 301, location: MetadataPrefix + "project/"},
		{name: "service account listing", url: md + "instance/service-accounts/", headers: googleFlavor, status: 200, body: "default/\n" + testIdentity.Email + "/\n"},
		{name: "default service account directory", url: md + "instance/service-accounts/default/", headers: googleFlavor, status: 200, contains: "identity\n"},
		{name: "service account recursive", url: md + "instance/service-accounts/default/?recursive=true", headers: googleFlavor, status: 200, contains: `"email":"` + testIdentity.Email + `"`},
		{name: "email of default", url: md + "instance/service-accounts/default/email", headers: googleFlavor, status: 200, body: testIdentity.Email},
		{name: "email by email", url: md + "instance/service-accounts/" + testIdentity.Email + "/email", headers: googleFlavor, status: 200, body: testIdentity.Email},
		{name: "unknown service account", url: md + "instance/service-accounts/other@example-project.iam.gserviceaccount.com/email", headers: googleFlavor, status: 404},
		{name: "scopes", url: md + "instance/service-accounts/default/scopes", headers: googleFlavor, status: 200, contains: "cloud-platform"},
		{name: "access token", url: md + "instance/service-accounts/default/token", headers: googleFlavor, status: 200, contains: `"token_type":"Bearer"`},
		{name: "identity without audience", url: md + "instance/service-accounts/default/identity", headers: googleFlavor, status: 400, contains: "audience"},
		{name: "identity with bad format", url: md + "instance/service-accounts/default/identity?audience=x&format=compact", headers: googleFlavor, status: 400},
		{name: "identity with bad licenses", url: md + "instance/service-accounts/default/identity?audience=x&format=full&licenses=yes", headers: googleFlavor, status: 400},
		{name: "wait_for_change", url: md + "project/project-id?wait_for_change=true", headers: googleFlavor, status: 400},
		{name: "unknown value", url: md + "instance/nonexistent", headers: googleFlavor, status: 404},
		{name: "outside computeMetadata", url: hs.URL + "/computeMetadata/v1beta1/project/project-id", headers: googleFlavor, status: 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := get(t, tc.url, tc.headers)
			if r.status != tc.status {
				t.Fatalf("status %d, want %d (body %q)", r.status, tc.status, r.body)
			}
			if got := r.header.Get("Metadata-Flavor"); got != "Google" {
				t.Errorf("Metadata-Flavor response header %q, want Google", got)
			}
			if tc.body != "" && r.body != tc.body {
				t.Errorf("body %q, want %q", r.body, tc.body)
			}
			if tc.contains != "" && !strings.Contains(r.body, tc.contains) {
				t.Errorf("body %q does not contain %q", r.body, tc.contains)
			}
			if tc.location != "" && r.header.Get("Location") != tc.location {
				t.Errorf("Location %q, want %q", r.header.Get("Location"), tc.location)
			}
		})
	}

	t.Run("only GET and HEAD", func(t *testing.T) {
		res, err := http.Post(md+"project/project-id", "text/plain", strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("POST: %d", res.StatusCode)
		}
	})
}

// keyfunc verifies tokens against the fake's published keys.
func keyfunc(s *Server) jwt.Keyfunc {
	return func(tok *jwt.Token) (any, error) {
		kid, _ := tok.Header["kid"].(string)
		for _, k := range s.JWKS().Keys {
			if k.Kid != kid {
				continue
			}
			n, err := base64.RawURLEncoding.DecodeString(k.N)
			if err != nil {
				return nil, err
			}
			e, err := base64.RawURLEncoding.DecodeString(k.E)
			if err != nil {
				return nil, err
			}
			return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}, nil
		}
		return nil, fmt.Errorf("kid %q is not published", kid)
	}
}

// verify checks a token the way a strict relying party does and returns its
// header and claims.
func verify(t *testing.T, s *Server, tok string) (map[string]any, jwt.MapClaims, error) {
	t.Helper()
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(tok, claims, keyfunc(s),
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithAudience(testAudience), jwt.WithIssuer(DefaultIssuer),
		jwt.WithIssuedAt(), jwt.WithExpirationRequired(), jwt.WithLeeway(60*time.Second))
	if err != nil {
		return nil, nil, err
	}
	return parsed.Header, claims, nil
}

// segments decodes a token's header and payload without verifying it.
func segments(t *testing.T, tok string) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments", len(parts))
	}
	dec := func(s string) map[string]any {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]any{}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	return dec(parts[0]), dec(parts[1])
}

func identityURL(hs *httptest.Server, query string) string {
	return hs.URL + IdentityPath + "?" + query
}

func TestIDTokenShape(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s, hs := newTestServer(t, Options{Now: func() time.Time { return now }})

	full := get(t, identityURL(hs, url.Values{"audience": {testAudience}, "format": {"full"}}.Encode()), googleFlavor)
	if full.status != 200 || strings.ContainsAny(full.body, "\n ") {
		t.Fatalf("identity: %d %q", full.status, full.header)
	}
	h, c, err := verify(t, s, full.body)
	if err != nil {
		t.Fatalf("format=full token does not verify: %v", err)
	}
	if h["alg"] != "RS256" || h["typ"] != "JWT" || h["kid"] != s.ActiveKID() || len(h) != 3 {
		t.Errorf("header %v", h)
	}
	want := map[string]any{
		"aud": testAudience, "azp": testIdentity.UniqueID, "sub": testIdentity.UniqueID, "iss": DefaultIssuer,
		"email": testIdentity.Email, "email_verified": true,
		"iat": float64(now.Unix()), "exp": float64(now.Add(time.Hour).Unix()),
	}
	if len(c) != len(want) {
		t.Errorf("claims %v, want exactly %v", c, want)
	}
	for k, v := range want {
		if c[k] != v {
			t.Errorf("claim %s = %v, want %v", k, c[k], v)
		}
	}

	std := get(t, identityURL(hs, url.Values{"audience": {testAudience}}.Encode()), googleFlavor)
	_, c, err = verify(t, s, std.body)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c["email"]; ok {
		t.Errorf("format=standard (the default) must not carry email: %v", c)
	}
	if _, ok := c["email_verified"]; ok {
		t.Errorf("format=standard must not carry email_verified: %v", c)
	}

	if got := s.Count(IdentityPath); got != 2 {
		t.Errorf("identity requests %d, want 2", got)
	}
	is := s.Issued()
	if len(is) != 2 || is[0].Email != testIdentity.Email || is[1].Email != "" || is[0].Via != "metadata" {
		t.Errorf("issued %+v", is)
	}
}

func TestComputeEngineClaims(t *testing.T) {
	s, hs := newTestServer(t, Options{ComputeEngine: &ComputeEngine{InstanceName: "vm-1", CreationTimestamp: 1700000000, LicenseIDs: []string{"1000204"}}})
	tok := get(t, identityURL(hs, "audience="+url.QueryEscape(testAudience)+"&format=full&licenses=TRUE"), googleFlavor).body
	_, c, err := verify(t, s, tok)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := c["google"].(map[string]any)
	ce, _ := g["compute_engine"].(map[string]any)
	if ce["instance_name"] != "vm-1" || ce["project_id"] != DefaultProjectID || ce["zone"] != DefaultZone ||
		ce["project_number"] != float64(123456789012) || fmt.Sprint(ce["license_id"]) != "[1000204]" {
		t.Errorf("google.compute_engine = %v", ce)
	}
	tok = get(t, identityURL(hs, "audience="+url.QueryEscape(testAudience)+"&format=full"), googleFlavor).body
	_, c, _ = verify(t, s, tok)
	if ce := c["google"].(map[string]any)["compute_engine"].(map[string]any); ce["license_id"] != nil {
		t.Errorf("license_id without licenses=TRUE: %v", ce)
	}
	if r := get(t, hs.URL+"/", nil); r.header.Get("Server") != "Metadata Server for VM" {
		t.Errorf("Server header %q", r.header.Get("Server"))
	}
}

func TestFaults(t *testing.T) {
	s, hs := newTestServer(t, Options{})
	fetch := func() string {
		t.Helper()
		r := get(t, identityURL(hs, url.Values{"audience": {testAudience}, "format": {"full"}}.Encode()), googleFlavor)
		if r.status != 200 {
			t.Fatalf("identity: %d", r.status)
		}
		return r.body
	}
	if _, _, err := verify(t, s, fetch()); err != nil {
		t.Fatalf("good token: %v", err)
	}
	for _, f := range Faults() {
		t.Run(string(f), func(t *testing.T) {
			if err := s.SetFault(f, 1); err != nil {
				t.Fatal(err)
			}
			tok := fetch()
			h, p := segments(t, tok)
			_, claims, verr := verify(t, s, tok)
			switch f {
			case FaultWrongAudience:
				if p["aud"] == testAudience || !errors.Is(verr, jwt.ErrTokenInvalidAudience) {
					t.Errorf("aud %v, err %v", p["aud"], verr)
				}
			case FaultWrongIssuer:
				if !errors.Is(verr, jwt.ErrTokenInvalidIssuer) {
					t.Errorf("iss %v, err %v", p["iss"], verr)
				}
			case FaultEmailUnverified:
				if verr != nil || claims["email_verified"] != false || claims["email"] != testIdentity.Email {
					t.Errorf("claims %v, err %v", claims, verr)
				}
			case FaultNoEmail:
				if _, ok := claims["email"]; verr != nil || ok {
					t.Errorf("claims %v, err %v", claims, verr)
				}
			case FaultWrongTyp:
				if verr != nil || h["typ"] != "at+jwt" {
					t.Errorf("header %v, err %v", h, verr)
				}
			case FaultNoTyp:
				if _, ok := h["typ"]; verr != nil || ok {
					t.Errorf("header %v, err %v", h, verr)
				}
			case FaultUnknownKid:
				if slices.Contains(s.PublishedKIDs(), h["kid"].(string)) || verr == nil {
					t.Errorf("kid %v published, err %v", h["kid"], verr)
				}
			case FaultBadSignature:
				if h["kid"] != s.ActiveKID() || !errors.Is(verr, jwt.ErrTokenSignatureInvalid) {
					t.Errorf("header %v, err %v", h, verr)
				}
			case FaultAlgNone:
				if h["alg"] != "none" || !strings.HasSuffix(tok, ".") || verr == nil {
					t.Errorf("header %v, err %v", h, verr)
				}
			case FaultHS256Confusion:
				if h["alg"] != "HS256" || h["kid"] != s.ActiveKID() || verr == nil {
					t.Errorf("header %v, err %v", h, verr)
				}
				// A verifier that used the RSA public key PEM as the HMAC
				// secret would accept it.
				s.mu.Lock()
				secret := s.active.publicPEM()
				s.mu.Unlock()
				if _, err := jwt.Parse(tok, func(*jwt.Token) (any, error) { return secret, nil }, jwt.WithValidMethods([]string{"HS256"})); err != nil {
					t.Errorf("the confusion token is not HMAC-signed with the public key PEM: %v", err)
				}
			case FaultExpired:
				if !errors.Is(verr, jwt.ErrTokenExpired) {
					t.Errorf("exp %v, err %v", p["exp"], verr)
				}
			case FaultIssuedInFuture:
				if !errors.Is(verr, jwt.ErrTokenUsedBeforeIssued) {
					t.Errorf("iat %v, err %v", p["iat"], verr)
				}
			default:
				t.Fatalf("fault %s is not checked", f)
			}
			// The fault applied to one token only.
			if _, _, err := verify(t, s, fetch()); err != nil {
				t.Errorf("token after the fault: %v", err)
			}
		})
	}

	t.Run("until cleared", func(t *testing.T) {
		if err := s.SetFault(FaultExpired, 0); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			if _, _, err := verify(t, s, fetch()); !errors.Is(err, jwt.ErrTokenExpired) {
				t.Fatalf("want expired, got %v", err)
			}
		}
		if err := s.SetFault(FaultNone, 0); err != nil {
			t.Fatal(err)
		}
		if _, _, err := verify(t, s, fetch()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("clock offset and ttl", func(t *testing.T) {
		s.SetClockOffset(-2 * time.Hour)
		if _, _, err := verify(t, s, fetch()); !errors.Is(err, jwt.ErrTokenExpired) {
			t.Errorf("offset -2h: %v", err)
		}
		s.SetClockOffset(90 * time.Second)
		if _, _, err := verify(t, s, fetch()); !errors.Is(err, jwt.ErrTokenUsedBeforeIssued) {
			t.Errorf("offset +90s: %v", err)
		}
		s.SetClockOffset(30 * time.Second)
		if _, _, err := verify(t, s, fetch()); err != nil {
			t.Errorf("offset +30s is within the leeway: %v", err)
		}
		if err := s.SetTTL(10 * time.Second); err != nil {
			t.Fatal(err)
		}
		s.SetClockOffset(0)
		_, p := segments(t, fetch())
		if d := p["exp"].(float64) - p["iat"].(float64); d != 10 {
			t.Errorf("exp - iat = %v, want 10", d)
		}
		s.Reset()
		if st := s.State(); st.TTL != "1h0m0s" || st.ClockOffset != "0s" || st.Identity != testIdentity {
			t.Errorf("after reset: %+v", st)
		}
	})

	if err := s.SetFault("bogus", 1); err == nil {
		t.Error("unknown fault accepted")
	}
}

func TestKeys(t *testing.T) {
	s, hs := newTestServer(t, Options{})
	first := s.ActiveKID()
	if len(first) != 40 {
		t.Errorf("kid %q is not 40 hex digits", first)
	}
	jwks := func() JWKS {
		t.Helper()
		r := get(t, hs.URL+JWKSPath, nil)
		if r.status != 200 || !strings.HasPrefix(r.header.Get("Cache-Control"), "public, max-age=21600") {
			t.Fatalf("JWKS: %d %v", r.status, r.header)
		}
		var set JWKS
		if err := json.Unmarshal([]byte(r.body), &set); err != nil {
			t.Fatal(err)
		}
		return set
	}
	kids := func(set JWKS) []string {
		var out []string
		for _, k := range set.Keys {
			if k.Alg != "RS256" || k.Kty != "RSA" || k.Use != "sig" || k.E != "AQAB" {
				t.Errorf("JWK %+v", k)
			}
			out = append(out, k.Kid)
		}
		return out
	}
	if got := kids(jwks()); !slices.Equal(got, []string{first}) {
		t.Fatalf("kids %v", got)
	}

	// Google style: publish first, sign later.
	next, err := s.AddKey()
	if err != nil {
		t.Fatal(err)
	}
	if s.ActiveKID() != first || !slices.Equal(kids(jwks()), []string{first, next}) {
		t.Fatalf("after AddKey: active %s, kids %v", s.ActiveKID(), kids(jwks()))
	}
	if err := s.Activate(next); err != nil {
		t.Fatal(err)
	}
	tok, err := s.Mint(MintOptions{Audience: testAudience})
	if err != nil {
		t.Fatal(err)
	}
	if h, _, err := verify(t, s, tok); err != nil || h["kid"] != next {
		t.Fatalf("token after activation: %v %v", h, err)
	}
	if err := s.Retire(next); err == nil {
		t.Error("the active key was retired")
	}
	if err := s.Retire(first); err != nil {
		t.Fatal(err)
	}
	if got := kids(jwks()); !slices.Equal(got, []string{next}) {
		t.Fatalf("after Retire: %v", got)
	}
	// A retired key still signs on request, and its tokens no longer verify.
	old, err := s.Mint(MintOptions{Audience: testAudience, KID: first})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := verify(t, s, old); err == nil {
		t.Error("a token of a retired key verified")
	}
	if err := s.Activate(first); err == nil {
		t.Error("a retired key was activated")
	}

	// Immediate rotation: the new key signs at once; the old one stays.
	rotated, err := s.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	if s.ActiveKID() != rotated || !slices.Equal(s.PublishedKIDs(), []string{next, rotated}) {
		t.Fatalf("after Rotate: active %s, published %v", s.ActiveKID(), s.PublishedKIDs())
	}
	if err := s.Activate("nope"); err == nil {
		t.Error("unknown kid activated")
	}
}

func TestSigningKeys(t *testing.T) {
	gen := func(bits int) *rsa.PrivateKey {
		t.Helper()
		k, err := rsa.GenerateKey(rand.Reader, bits)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	k1, k2 := gen(2048), gen(2048)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(k2)
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"PKCS #1": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k1)}),
		"PKCS #8": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}),
	} {
		if _, err := ParseRSAPrivateKeyPEM(b); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParseRSAPrivateKeyPEM([]byte("not a key")); err == nil {
		t.Error("garbage parsed")
	}

	s, err := New(Options{SigningKeys: []SigningKey{{KID: "stable-kid-1", Key: k1}, {Key: k2}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if s.ActiveKID() != "stable-kid-1" || len(s.PublishedKIDs()) != 2 || len(s.PublishedKIDs()[1]) != 40 {
		t.Fatalf("active %s, published %v", s.ActiveKID(), s.PublishedKIDs())
	}
	tok, err := s.Mint(MintOptions{Audience: testAudience})
	if err != nil {
		t.Fatal(err)
	}
	if h, _, err := verify(t, s, tok); err != nil || h["kid"] != "stable-kid-1" {
		t.Fatalf("token: %v %v", h, err)
	}
	if _, err := New(Options{SigningKeys: []SigningKey{{Key: gen(1024)}}}); err == nil {
		t.Error("a 1024-bit key was accepted")
	}
	if _, err := New(Options{SigningKeys: []SigningKey{{KID: "x", Key: k1}, {KID: "x", Key: k2}}}); err == nil {
		t.Error("duplicate kids were accepted")
	}
}

func TestMint(t *testing.T) {
	s, _ := newTestServer(t, Options{})
	other := Identity{Email: "app-api@example-project.iam.gserviceaccount.com", UniqueID: "100000000000000000777"}
	tok, err := s.Mint(MintOptions{Audience: testAudience, Identity: &other})
	if err != nil {
		t.Fatal(err)
	}
	_, c, err := verify(t, s, tok)
	if err != nil || c["email"] != other.Email || c["sub"] != other.UniqueID {
		t.Fatalf("claims %v, err %v", c, err)
	}
	if s.Identity() != testIdentity {
		t.Error("Mint changed the server identity")
	}
	if _, err := s.Mint(MintOptions{}); err == nil {
		t.Error("Mint without audience")
	}
	if _, err := s.Mint(MintOptions{Audience: "x", Format: "compact"}); err == nil {
		t.Error("Mint with an unknown format")
	}
	// A fault set for the metadata endpoint does not apply to Mint.
	if err := s.SetFault(FaultExpired, 0); err != nil {
		t.Fatal(err)
	}
	tok, _ = s.Mint(MintOptions{Audience: testAudience})
	if _, _, err := verify(t, s, tok); err != nil {
		t.Errorf("Mint used the metadata fault: %v", err)
	}
	if is := s.Issued(); is[len(is)-1].Via != "mint" {
		t.Errorf("issued %+v", is)
	}
}

func TestOutages(t *testing.T) {
	s, hs := newTestServer(t, Options{Hang: 5 * time.Second})
	idURL := identityURL(hs, "audience=x&format=full")
	pidURL := hs.URL + MetadataPrefix + "project/project-id"

	for _, tc := range []struct {
		mode   OutageMode
		status int
		lines  int
	}{
		{Outage500, 500, 2}, {Outage503, 503, 2}, {OutageForbidden, 403, 3},
	} {
		if err := s.SetOutage(TargetMetadata, tc.mode, 2); err != nil {
			t.Fatal(err)
		}
		for i := range 2 {
			r := get(t, pidURL, googleFlavor)
			if r.status != tc.status || strings.Count(r.body, "\n") < tc.lines {
				t.Errorf("%s request %d: %d %q", tc.mode, i, r.status, r.body)
			}
		}
		if r := get(t, pidURL, googleFlavor); r.status != 200 {
			t.Errorf("%s: after the count: %d", tc.mode, r.status)
		}
	}

	// The detection root is never affected; identity outages spare other
	// values.
	if err := s.SetOutage(TargetIdentity, Outage500, 0); err != nil {
		t.Fatal(err)
	}
	if r := get(t, hs.URL+"/", nil); r.status != 200 {
		t.Errorf("root during an outage: %d", r.status)
	}
	if r := get(t, pidURL, googleFlavor); r.status != 200 {
		t.Errorf("project id during an identity outage: %d", r.status)
	}
	for range 3 {
		if r := get(t, idURL, googleFlavor); r.status != 500 {
			t.Errorf("identity during an outage: %d", r.status)
		}
	}
	if err := s.SetOutage(TargetIdentity, OutageNone, 0); err != nil {
		t.Fatal(err)
	}
	if r := get(t, idURL, googleFlavor); r.status != 200 {
		t.Errorf("identity after the outage: %d", r.status)
	}

	// Timeout: no answer until the client gives up.
	if err := s.SetOutage(TargetMetadata, OutageTimeout, 1); err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Timeout: 200 * time.Millisecond}
	req, _ := http.NewRequest(http.MethodGet, pidURL, nil)
	req.Header.Set("Metadata-Flavor", "Google")
	start := time.Now()
	if res, err := c.Do(req); err == nil {
		_ = res.Body.Close()
		t.Fatalf("timeout outage answered %d", res.StatusCode)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("client waited %s", d)
	}

	// JWKS outage.
	if err := s.SetOutage(TargetJWKS, Outage503, 1); err != nil {
		t.Fatal(err)
	}
	if r := get(t, hs.URL+JWKSPath, nil); r.status != 503 {
		t.Errorf("JWKS during an outage: %d", r.status)
	}
	if r := get(t, hs.URL+JWKSPath, nil); r.status != 200 {
		t.Errorf("JWKS after the outage: %d", r.status)
	}

	if err := s.SetOutage("dns", Outage500, 0); err == nil {
		t.Error("unknown target accepted")
	}
	if err := s.SetOutage(TargetJWKS, "418", 0); err == nil {
		t.Error("unknown mode accepted")
	}
	var notes []string
	for _, r := range s.Requests() {
		if r.Note != "" {
			notes = append(notes, r.Note)
		}
	}
	if !slices.Contains(notes, "outage 500") || !slices.Contains(notes, "outage timeout (client gave up)") {
		t.Errorf("request notes %v", notes)
	}
}

func TestControlAPI(t *testing.T) {
	s, hs := newTestServer(t, Options{})
	ctl := httptest.NewServer(s.ControlHandler())
	t.Cleanup(ctl.Close)

	post := func(path, body string, want int) map[string]any {
		t.Helper()
		res, err := http.Post(ctl.URL+ControlPrefix+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out := map[string]any{}
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("POST %s %s: %d %v", path, body, res.StatusCode, out)
		}
		return out
	}

	if r := get(t, ctl.URL+"/healthz", nil); r.status != 200 {
		t.Fatalf("healthz %d", r.status)
	}
	post("identity", `{"email": "attacker@other-project.iam.gserviceaccount.com", "uniqueId": "100000000000000000666"}`, 200)
	if s.Identity().Email != "attacker@other-project.iam.gserviceaccount.com" {
		t.Error("identity not changed")
	}
	post("identity", `{"email": ""}`, 400)
	post("settings", `{"ttl": "5m", "clockOffset": "-30s"}`, 200)
	if st := s.State(); st.TTL != "5m0s" || st.ClockOffset != "-30s" {
		t.Errorf("settings: %+v", st)
	}
	post("settings", `{"ttl": "soon"}`, 400)
	post("fault", `{"fault": "wrong-typ", "count": 2}`, 200)
	if st := s.State(); st.Fault != FaultWrongTyp || st.FaultCount != 2 {
		t.Errorf("fault: %+v", st)
	}
	post("fault", `{"fault": "nonsense"}`, 400)
	post("outage", `{"target": "jwks", "mode": "500"}`, 200)
	if st := s.State(); st.Outages[TargetJWKS] != Outage500 {
		t.Errorf("outage: %+v", st)
	}
	out := post("keys", `{"action": "add"}`, 200)
	added, _ := out["kid"].(string)
	if !slices.Contains(s.PublishedKIDs(), added) || s.ActiveKID() == added {
		t.Errorf("keys add: %v", out)
	}
	post("keys", `{"action": "activate", "kid": "`+added+`"}`, 200)
	if s.ActiveKID() != added {
		t.Error("keys activate")
	}
	post("keys", `{"action": "explode"}`, 400)
	post("keys", `{"action": "retire", "kid": "`+added+`"}`, 400)
	out = post("mint", `{"audience": "`+testAudience+`", "email": "app-api@example-project.iam.gserviceaccount.com"}`, 200)
	tok, _ := out["token"].(string)
	if _, c, err := verify(t, s, tok); err != nil || c["email"] != "app-api@example-project.iam.gserviceaccount.com" {
		t.Errorf("mint: %v %v", c, err)
	}
	post("mint", `{}`, 400)
	post("mint", `{"audience": "x", "unknown": 1}`, 400)
	post("reset", ``, 200)
	if st := s.State(); st.Identity != testIdentity || st.Fault != FaultNone || len(st.Outages) != 0 || st.TTL != "1h0m0s" {
		t.Errorf("after reset: %+v", st)
	}
	var st State
	if err := json.Unmarshal([]byte(get(t, ctl.URL+ControlPrefix+"state", nil).body), &st); err != nil || st.ActiveKID != added {
		t.Errorf("state: %+v %v", st, err)
	}
	if r := get(t, ctl.URL+ControlPrefix+"issued", nil); r.status != 200 || strings.Contains(r.body, tok) {
		t.Errorf("issued must list summaries only: %d", r.status)
	}
	// The control API is not served on the metadata port.
	if r := get(t, hs.URL+ControlPrefix+"state", googleFlavor); r.status != 404 {
		t.Errorf("control API on the metadata port: %d", r.status)
	}
}

func TestProcessorYAML(t *testing.T) {
	p := ProductionGoogleProcessor("https://clickhouse.example.com")
	want := `token_processors:
  google_jwt:
    type: jwt_dynamic_jwks
    jwks_uri: "https://www.googleapis.com/oauth2/v3/certs"
    expected_issuer: "https://accounts.google.com"
    expected_audience: "https://clickhouse.example.com"
    expected_typ: JWT
    claims: '{"email_verified": true}'
    username_claim: email
    jwks_cache_lifetime: 3600
    token_cache_lifetime: 300
`
	if got := p.YAML(); got != want {
		t.Fatalf("production processor:\n%s\nwant:\n%s", got, want)
	}
	// The docs and the design's server configuration examples (Appendix B)
	// show exactly this rendering.
	for _, path := range []string{"docs/testing-google-tokens.md", "docs/design.md"} {
		doc, err := os.ReadFile("../../" + path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(doc), want) {
			t.Errorf("%s does not contain the production processor as rendered:\n%s", path, want)
		}
	}
	// The Kubernetes manifest renders the same processor against the fake.
	k8s := p
	k8s.JWKSURI = "http://fakegcp.fakegcp.svc.cluster.local" + JWKSPath
	k8s.Audience = "https://clickhouse-e2e.example.com"
	k8s.JWKSCacheLifetime, k8s.TokenCacheLifetime = 60*time.Second, 60*time.Second
	cm, err := os.ReadFile("deploy/clickhouse-google-jwt.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cm), indent(k8s.YAML(), "    ")) {
		t.Errorf("deploy/clickhouse-google-jwt.yaml does not contain the processor as rendered:\n%s", indent(k8s.YAML(), "    "))
	}
}

func indent(s, prefix string) string {
	lines := strings.SplitAfter(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "")
}

// TestGoogleLibraries runs the Google client libraries against the fake
// exactly as the provider uses them on GKE: Application Default Credentials
// with GCE_METADATA_HOST, and nothing else. It also pins two library
// behaviours the integration tests rely on: which library is in use, and
// that both cache a metadata ID token for a fixed time whatever its exp.
func TestGoogleLibraries(t *testing.T) {
	s, hs := newTestServer(t, Options{})
	UseFake(t, strings.TrimPrefix(hs.URL, "http://"))

	for _, newAuth := range []bool{false, true} {
		t.Run(fmt.Sprintf("newAuthLibrary=%t", newAuth), func(t *testing.T) {
			t.Setenv("GOOGLE_API_GO_EXPERIMENTAL_ENABLE_NEW_AUTH_LIB", fmt.Sprint(newAuth))
			// Tokens that expire in 5 min.
			if err := s.SetTTL(5 * time.Minute); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Reset)
			before, ids := len(s.Requests()), s.Count(IdentityPath)
			ts, otok, eager := libraryToken(t, s, testAudience)
			tok := otok.AccessToken
			if _, c, err := verify(t, s, tok); err != nil || c["email"] != testIdentity.Email {
				t.Fatalf("library token: %v %v", c, err)
			}
			// The switch took effect: both libraries send the same request,
			// but only cloud.google.com/go/auth returns an oauth2adapt token
			// source and fetches on the first Token call instead of in
			// NewTokenSource.
			wantEager := 1
			if newAuth {
				wantEager = 0
			}
			if got := UsesNewAuthLibrary(ts); got != newAuth || eager != wantEager {
				t.Fatalf("token source %T (new auth library %t) requested %d ID tokens in NewTokenSource; want the new auth library %t and %d",
					ts, got, eager, newAuth, wantEager)
			}
			// The library's expiry is fixed (55 min; the new library: 1 h,
			// and it refreshes 5 min early), not the token's exp: a
			// short-lived token is reused until it is refused.
			life := 55 * time.Minute
			if newAuth {
				life = time.Hour
			}
			if d := time.Until(otok.Expiry); d > life || d < life-time.Minute {
				t.Errorf("the library's token expires in %s, want about %s whatever the token's exp (5 min)", d.Round(time.Second), life)
			}
			if again, err := ts.Token(); err != nil || again.AccessToken != tok || s.Count(IdentityPath)-ids != 1 {
				t.Errorf("a second Token call: %v, %d ID token requests in all; want the cached token and 1", err, s.Count(IdentityPath)-ids)
			}
			// Google's own validator accepts it against the fake's JWKS.
			v := googleValidator(t, hs)
			p, err := v.Validate(context.Background(), tok, testAudience)
			if err != nil {
				t.Fatalf("Google's validator: %v", err)
			}
			if p.Issuer != DefaultIssuer || p.Claims["email"] != testIdentity.Email || p.Claims["email_verified"] != true || p.Subject != testIdentity.UniqueID {
				t.Errorf("payload %+v", p)
			}
			// The libraries asked for format=full with the audience, and
			// touched nothing but the project ID and the identity endpoint.
			// Not even GET /: with GCE_METADATA_HOST set they skip GCE
			// detection, so the fake's root handler is never exercised.
			for _, r := range s.Requests()[before:] {
				switch r.Path {
				case IdentityPath:
					q, _ := url.ParseQuery(r.RawQuery)
					if q.Get("audience") != testAudience || q.Get("format") != "full" || q.Has("licenses") {
						t.Errorf("identity query %q", r.RawQuery)
					}
				case MetadataPrefix + "project/project-id", JWKSPath:
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.Path)
				}
			}
		})
	}
}
