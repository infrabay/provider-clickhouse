// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

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
