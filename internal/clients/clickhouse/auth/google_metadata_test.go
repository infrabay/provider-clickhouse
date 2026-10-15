// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/infrabay/provider-clickhouse/internal/reason"
	"github.com/infrabay/provider-clickhouse/test/fakegcp"
)

// TestGoogleSourceWithMetadataServer runs NewGoogleSource without a key
// (Application Default Credentials, the GKE Workload Identity path) against
// the fake metadata server: caching, Invalidate, the library's retries and
// scrubbed errors.
func TestGoogleSourceWithMetadataServer(t *testing.T) {
	const aud = "https://clickhouse-auth-unit.example.com"
	id := fakegcp.Identity{Email: "provider@example-project.iam.gserviceaccount.com", UniqueID: "100000000000000000042"}
	fake, err := fakegcp.New(fakegcp.Options{Identity: id})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(fake.Handler())
	t.Cleanup(hs.Close)
	t.Cleanup(fake.Close)
	fakegcp.UseFake(t, strings.TrimPrefix(hs.URL, "http://"))

	s, err := NewGoogleSource(aud, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tok, err := s.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got, err := Audiences(tok); err != nil || !slices.Equal(got, []string{aud}) {
		t.Fatalf("aud %v, %v", got, err)
	}
	if _, err := s.Token(ctx); err != nil {
		t.Fatal(err)
	}
	if n := fake.Count(fakegcp.IdentityPath); n != 1 {
		t.Fatalf("identity requests after two Token calls: %d, want 1 (cached)", n)
	}
	s.Invalidate()
	if _, err := s.Token(ctx); err != nil {
		t.Fatal(err)
	}
	if n := fake.Count(fakegcp.IdentityPath); n != 2 {
		t.Fatalf("identity requests after Invalidate: %d, want 2", n)
	}

	// 5xx is retried by the metadata client: two failures are absorbed.
	if err := fake.SetOutage(fakegcp.TargetIdentity, fakegcp.Outage500, 2); err != nil {
		t.Fatal(err)
	}
	s.Invalidate()
	if _, err := s.Token(ctx); err != nil {
		t.Fatalf("Token with two 500s: %v", err)
	}

	// Errors carry the reason and only the first line of the response.
	for _, mode := range []fakegcp.OutageMode{fakegcp.OutageForbidden, fakegcp.Outage500} {
		if err := fake.SetOutage(fakegcp.TargetIdentity, mode, 0); err != nil {
			t.Fatal(err)
		}
		s.Invalidate()
		_, err := s.Token(ctx)
		var ae *Error
		if !errors.As(err, &ae) || ae.Source != SourceGoogleIDToken || !reason.Is(err, reason.AuthenticationFailed) {
			t.Fatalf("%s: %v, want a GoogleIDToken AuthenticationFailed error", mode, err)
		}
		msg := err.Error()
		if !strings.Contains(msg, string(mode)) || strings.ContainsAny(msg, "\r\n") || strings.Contains(msg, "fakegcp") || len(msg) > 300 {
			t.Errorf("%s: unscrubbed error %q", mode, msg)
		}
	}
	if err := fake.SetOutage(fakegcp.TargetIdentity, fakegcp.OutageNone, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token(ctx); err != nil {
		t.Fatalf("Token after the outage: %v", err)
	}
}

// expiringSource is an underlying Google token source whose tokens expire at
// the times in expiries, one per call (the last one repeats).
type expiringSource struct {
	mu       sync.Mutex
	expiries []time.Time
	calls    int
}

func (e *expiringSource) Token() (*oauth2.Token, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	exp := e.expiries[min(e.calls, len(e.expiries)-1)]
	e.calls++
	return &oauth2.Token{AccessToken: fmt.Sprintf("id-token-%d", e.calls), Expiry: exp}, nil
}

// TestGoogleSourceHonoursExpiry: GoogleSource never returns a token past the
// expiry its underlying source reports (oauth2 refreshes 10 s early), without
// waiting for a 516 and Invalidate. The Google libraries report 55 min (or
// 1 h) for every metadata ID token, so no integration test can tell this from
// a source that caches the first token forever; only this test can.
func TestGoogleSourceHonoursExpiry(t *testing.T) {
	now := time.Now()
	cases := map[string]struct {
		expiries []time.Time
		// want are the tokens of three Token calls without Invalidate.
		want []string
	}{
		"Expired": {
			expiries: []time.Time{now.Add(-time.Second)},
			want:     []string{"id-token-1", "id-token-2", "id-token-3"},
		},
		"ExpiresWithinTheEarlyRefresh": {
			expiries: []time.Time{now.Add(2 * time.Second)},
			want:     []string{"id-token-1", "id-token-2", "id-token-3"},
		},
		"ExpiredThenValid": {
			expiries: []time.Time{now.Add(-time.Minute), now.Add(time.Hour)},
			want:     []string{"id-token-1", "id-token-2", "id-token-2"},
		},
		"Valid": {
			expiries: []time.Time{now.Add(time.Hour)},
			want:     []string{"id-token-1", "id-token-1", "id-token-1"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			built := 0
			src := &expiringSource{expiries: tc.expiries}
			s := NewGoogleSourceWith(func() (oauth2.TokenSource, error) {
				built++
				return src, nil
			})
			var got []string
			for range tc.want {
				tok, err := s.Token(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, tok)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("tokens %v, want %v", got, tc.want)
			}
			if built != 1 {
				t.Errorf("the underlying source was built %d times, want 1 (expiry is not Invalidate)", built)
			}
		})
	}
}
