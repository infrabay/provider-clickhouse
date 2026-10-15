// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package fakegcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/option"
)

// libraryToken fetches an ID token the way the provider does: through
// idtoken.NewTokenSource with Application Default Credentials. It returns
// the token source, its token, and how many ID tokens NewTokenSource itself
// requested from s (golang.org/x/oauth2 fetches at once,
// cloud.google.com/go/auth on the first Token call).
func libraryToken(t *testing.T, s *Server, audience string) (oauth2.TokenSource, *oauth2.Token, int) {
	t.Helper()
	before := s.Count(IdentityPath)
	ts, err := idtoken.NewTokenSource(context.Background(), audience)
	if err != nil {
		t.Fatalf("idtoken.NewTokenSource: %v", err)
	}
	eager := s.Count(IdentityPath) - before
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	return ts, tok, eager
}

// toFake sends requests for Google's JWKS to the fake and fails every other
// request, so that a test never reaches Google.
type toFake struct{ fake *httptest.Server }

func (f toFake) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "www.googleapis.com" || req.URL.Path != JWKSPath {
		return nil, fmt.Errorf("test transport: refusing request to %s", req.URL.Redacted())
	}
	r := req.Clone(req.Context())
	r.URL.Scheme, r.URL.Host, r.Host = "http", strings.TrimPrefix(f.fake.URL, "http://"), ""
	return http.DefaultTransport.RoundTrip(r)
}

// googleValidator returns Google's own ID token validator, fetching keys
// from the fake.
func googleValidator(t *testing.T, fake *httptest.Server) *idtoken.Validator {
	t.Helper()
	v, err := idtoken.NewValidator(context.Background(), option.WithHTTPClient(&http.Client{Transport: toFake{fake: fake}}))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
