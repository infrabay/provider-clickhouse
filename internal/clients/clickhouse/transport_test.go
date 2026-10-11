// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/auth"
)

func TestBearerTransport(t *testing.T) {
	var gotAuth, gotUser, gotKey atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		gotUser.Store(r.Header.Get("X-ClickHouse-User"))
		gotKey.Store(r.Header.Get("X-ClickHouse-Key"))
		if _, _, ok := r.BasicAuth(); ok {
			t.Error("basic auth must be stripped")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	bt := NewBearerTransport(http.DefaultTransport, auth.Static{Value: "h.p.s"}, "http", u.Host)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/?query=1", strings.NewReader("SELECT 1"))
	req.URL.User = url.UserPassword("default", "pw")
	req.Header.Set("X-ClickHouse-User", "default")
	req.Header.Set("X-ClickHouse-Key", "pw")
	resp, err := bt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotAuth.Load() != "Bearer h.p.s" || gotUser.Load() != "" || gotKey.Load() != "" {
		t.Errorf("headers: auth %q user %q key %q", gotAuth.Load(), gotUser.Load(), gotKey.Load())
	}
	if req.Header.Get("X-ClickHouse-User") != "default" {
		t.Error("the original request must not be modified")
	}
}

// TestBearerTransportStripsTLSCredentials covers what clickhouse-go sends
// over HTTPS without a password: user "default" and a request for
// certificate authentication, which the server rejects together with a bearer
// token.
func TestBearerTransportStripsTLSCredentials(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Clone(r.Context()))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	bt := NewBearerTransport(http.DefaultTransport, auth.Static{Value: "h.p.s"}, "http", u.Host)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/?database=default&user=default&password=pw&query_id=q1", strings.NewReader("SELECT 1"))
	req.Header.Set("X-ClickHouse-User", "default")
	req.Header.Set("X-ClickHouse-Key", "pw")
	req.Header.Set("X-ClickHouse-SSL-Certificate-Auth", "on")
	req.Header.Set("X-ClickHouse-Format", "Native")
	resp, err := bt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	r := got.Load().(*http.Request)
	for name := range r.Header {
		if strings.EqualFold(name, "X-ClickHouse-Format") {
			continue
		}
		if strings.HasPrefix(strings.ToLower(name), "x-clickhouse-") {
			t.Errorf("credential header %s was sent", name)
		}
	}
	if r.Header.Get("X-ClickHouse-Format") != "Native" {
		t.Error("non-credential headers must be kept")
	}
	q := r.URL.Query()
	if q.Has("user") || q.Has("password") || q.Get("database") != "default" || q.Get("query_id") != "q1" {
		t.Errorf("query = %v", q)
	}
	if r.Header.Get("Authorization") != "Bearer h.p.s" {
		t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
	}
}

func TestBearerTransportRefusesOtherHost(t *testing.T) {
	bt := NewBearerTransport(http.DefaultTransport, auth.Static{Value: "h.p.s"}, "https", "ch.example.com:8443")
	for _, raw := range []string{"https://evil.example.com:8443/", "http://ch.example.com:8443/"} {
		req, _ := http.NewRequest(http.MethodPost, raw, nil)
		if _, err := bt.RoundTrip(req); err == nil {
			t.Errorf("RoundTrip(%s) must fail", raw)
		}
	}
}

func TestBearerTransportRedirectNotFollowed(t *testing.T) {
	var leaked atomic.Bool
	second := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL, http.StatusTemporaryRedirect)
	}))
	defer first.Close()
	u, _ := url.Parse(first.URL)

	c := &http.Client{Transport: NewBearerTransport(http.DefaultTransport, auth.Static{Value: "h.p.s"}, "http", u.Host)}
	resp, err := c.Post(first.URL, "text/plain", strings.NewReader("SELECT 1"))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a redirect must be an error")
	}
	if leaked.Load() {
		t.Fatal("the token was sent to the redirect target")
	}
}
