// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"crypto/tls"
	"net/http"
	"testing"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/auth"
	"github.com/infrabay/provider-clickhouse/internal/reason"
)

func TestConfigValidate(t *testing.T) {
	ts := auth.Static{Value: "a.b.c"}
	cases := map[string]struct {
		cfg  Config
		want bool
	}{
		"Password":        {Config{Host: "h", Port: 9000, Protocol: ProtocolNative, Username: "u", Password: "p"}, true},
		"Token":           {Config{Host: "h", Port: 9440, Protocol: ProtocolNative, TokenSource: ts, TLS: &tls.Config{}}, true},
		"EmptyUser":       {Config{Host: "h", Port: 9000, Protocol: ProtocolNative, Password: "p"}, false},
		"NoHost":          {Config{Port: 9000, Protocol: ProtocolNative, Username: "u"}, false},
		"BadPort":         {Config{Host: "h", Port: 0, Protocol: ProtocolNative, Username: "u"}, false},
		"Both":            {Config{Host: "h", Port: 9000, Protocol: ProtocolNative, Username: "u", TokenSource: ts}, false},
		"SkipVerifyToken": {Config{Host: "h", Port: 9440, Protocol: ProtocolNative, TokenSource: ts, TLS: &tls.Config{InsecureSkipVerify: true}}, false}, //nolint:gosec // Test input.
		"BadProtocol":     {Config{Host: "h", Port: 9000, Protocol: "gRPC", Username: "u"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err == nil) != tc.want {
				t.Fatalf("Validate() = %v, want ok=%t", err, tc.want)
			}
			if err != nil && !reason.Is(err, reason.InvalidConfig) {
				t.Errorf("Validate() reason = %v, want InvalidConfig", err)
			}
		})
	}
}

func TestOptionsNativeToken(t *testing.T) {
	cfg := Config{Host: "ch.example.com", Port: 9440, Protocol: ProtocolNative, TLS: &tls.Config{}, TokenSource: auth.Static{Value: "h.p.s"}, MaxOpenConns: 6}
	opt, err := cfg.Options()
	if err != nil {
		t.Fatal(err)
	}
	if opt.Protocol != ch.Native || opt.GetJWT == nil || opt.Auth.Username != "" || opt.Auth.Password != "" {
		t.Fatalf("native token options: protocol %v, GetJWT set %t, user %q", opt.Protocol, opt.GetJWT != nil, opt.Auth.Username)
	}
	if tok, _ := opt.GetJWT(context.Background()); tok != "h.p.s" {
		t.Errorf("GetJWT() = %q", tok)
	}
	if opt.MaxOpenConns != 6 || opt.MaxIdleConns != 3 || opt.ConnMaxLifetime != DefaultConnMaxLifetime {
		t.Errorf("pool = %d/%d/%s", opt.MaxOpenConns, opt.MaxIdleConns, opt.ConnMaxLifetime)
	}
	if opt.Addr[0] != "ch.example.com:9440" || opt.Auth.Database != "" || opt.DialTimeout != dialTimeout {
		t.Errorf("addr/db/dial = %v/%q/%s", opt.Addr, opt.Auth.Database, opt.DialTimeout)
	}
	if len(opt.ClientInfo.Products) != 1 || opt.ClientInfo.Products[0].Name != "provider-clickhouse" {
		t.Errorf("client info = %+v", opt.ClientInfo)
	}
}

func TestOptionsNativePassword(t *testing.T) {
	cfg := Config{Host: "h", Port: 9000, Protocol: ProtocolNative, Username: "u", Password: "p", DefaultDatabase: "default"}
	opt, err := cfg.Options()
	if err != nil {
		t.Fatal(err)
	}
	if opt.GetJWT != nil || opt.Auth.Username != "u" || opt.Auth.Password != "p" || opt.Auth.Database != "default" {
		t.Fatalf("password options wrong: %+v", opt.Auth)
	}
	if opt.MaxOpenConns != DefaultMaxOpenConns || opt.MaxIdleConns != DefaultMaxOpenConns/2 {
		t.Errorf("default pool = %d/%d", opt.MaxOpenConns, opt.MaxIdleConns)
	}
}

func TestOptionsHTTPTokenNeverUsesGetJWT(t *testing.T) {
	cfg := Config{Host: "h", Port: 8123, Protocol: ProtocolHTTP, TokenSource: auth.Static{Value: "h.p.s"}}
	opt, err := cfg.Options()
	if err != nil {
		t.Fatal(err)
	}
	if opt.Protocol != ch.HTTP || opt.GetJWT != nil || opt.TransportFunc == nil || opt.Auth.Username != "" {
		t.Fatalf("HTTP token options: GetJWT set %t, TransportFunc set %t, user %q", opt.GetJWT != nil, opt.TransportFunc != nil, opt.Auth.Username)
	}
	rt, err := opt.TransportFunc(&http.Transport{})
	if err != nil {
		t.Fatal(err)
	}
	bt, ok := rt.(*BearerTransport)
	if !ok || bt.scheme != "http" || bt.host != "h:8123" {
		t.Errorf("transport = %#v", rt)
	}
}

func TestNewTLSConfig(t *testing.T) {
	c, err := NewTLSConfig(TLSOptions{ServerName: "ch"})
	if err != nil || c.MinVersion != tls.VersionTLS12 || c.ServerName != "ch" || c.RootCAs != nil {
		t.Fatalf("NewTLSConfig() = %+v, %v", c, err)
	}
	if _, err := NewTLSConfig(TLSOptions{CAPEM: []byte("not pem")}); !reason.Is(err, reason.InvalidConfig) {
		t.Errorf("invalid CA: %v", err)
	}
}
