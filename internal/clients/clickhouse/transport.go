// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/auth"
)

// credentialHeaders are the authentication headers the server reads
// (src/Server/HTTP/authenticateUserByHTTP.cpp). With TLS, clickhouse-go fills
// in user "default" and, without a password, asks for certificate
// authentication (X-ClickHouse-SSL-Certificate-Auth: on); the server rejects
// that combined with a bearer token.
var credentialHeaders = []string{
	"X-ClickHouse-User",
	"X-ClickHouse-Key",
	"X-ClickHouse-SSL-Certificate-Auth",
}

// credentialParams are URL parameters the server reads as credentials.
var credentialParams = []string{"user", "password"}

// BearerTransport authenticates HTTP requests with "Authorization: Bearer".
// It is the only way tokens are sent over HTTP:
//   - it clears URL user info, every X-ClickHouse-* authentication header
//     and the user/password URL parameters;
//   - it sets the token only for requests to the configured scheme and host,
//     and fails every other request;
//   - it turns every 3xx response into an error. Go strips sensitive headers
//     on cross-host redirects only at the http.Client level, so a transport
//     that always sets the header would resend the token to a redirect target.
type BearerTransport struct {
	base   http.RoundTripper
	ts     auth.TokenSource
	scheme string
	host   string
}

// NewBearerTransport wraps base.
func NewBearerTransport(base http.RoundTripper, ts auth.TokenSource, scheme, host string) *BearerTransport {
	return &BearerTransport{base: base, ts: ts, scheme: scheme, host: host}
}

// RoundTrip implements http.RoundTripper.
func (t *BearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || req.URL.Scheme != t.scheme || req.URL.Host != t.host {
		return nil, fmt.Errorf("refusing to send credentials to an unexpected endpoint")
	}
	tok, err := t.ts.Token(req.Context())
	if err != nil {
		return nil, err
	}
	r := req.Clone(req.Context())
	r.URL.User = nil
	for _, h := range credentialHeaders {
		r.Header.Del(h)
	}
	if q := r.URL.Query(); hasAny(q, credentialParams) {
		for _, k := range credentialParams {
			q.Del(k)
		}
		r.URL.RawQuery = q.Encode()
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("unexpected HTTP redirect (status %d); redirects are not followed", resp.StatusCode)
	}
	return resp, nil
}

func hasAny(q url.Values, keys []string) bool {
	for _, k := range keys {
		if q.Has(k) {
			return true
		}
	}
	return false
}
