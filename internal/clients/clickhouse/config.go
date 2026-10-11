// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/auth"
	"github.com/infrabay/provider-clickhouse/internal/reason"
	"github.com/infrabay/provider-clickhouse/internal/version"
)

// Protocol is the transport protocol.
type Protocol string

// Protocols. Only Native can be selected through the v1alpha1 API. HTTP is
// implemented for the deferred HTTP transport: it is never sent a JWT through
// clickhouse-go's GetJWT (which silently falls back to user "default" without
// TLS); a dedicated round tripper sets the bearer token instead.
const (
	ProtocolNative Protocol = "Native"
	ProtocolHTTP   Protocol = "HTTP"
)

// Defaults.
const (
	DefaultNativePort    = 9000
	DefaultNativeTLSPort = 9440
	DefaultHTTPPort      = 8123
	DefaultHTTPSPort     = 8443

	DefaultMaxOpenConns    = 4
	DefaultConnMaxLifetime = 30 * time.Minute

	dialTimeout = 5 * time.Second
	readTimeout = 60 * time.Second
)

// Config is a resolved connection configuration.
type Config struct {
	Host            string
	Port            int
	Protocol        Protocol
	TLS             *tls.Config // nil means no TLS
	DefaultDatabase string
	MaxOpenConns    int
	ConnMaxLifetime time.Duration

	// Password authentication.
	Username string
	Password string

	// Token authentication. Mutually exclusive with Username/Password.
	TokenSource auth.TokenSource
}

// Addr returns host:port.
func (c Config) Addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// TokenAuth reports whether the config authenticates with tokens.
func (c Config) TokenAuth() bool { return c.TokenSource != nil }

// Validate rejects configurations the provider never uses.
func (c Config) Validate() error {
	switch {
	case c.Host == "":
		return reason.Errorf(reason.InvalidConfig, "endpoint host is empty")
	case c.Port <= 0 || c.Port > 65535:
		return reason.Errorf(reason.InvalidConfig, "endpoint port %d is out of range", c.Port)
	case c.Protocol != ProtocolNative && c.Protocol != ProtocolHTTP:
		return reason.Errorf(reason.InvalidConfig, "unsupported protocol %q", c.Protocol)
	case c.TokenSource != nil && (c.Username != "" || c.Password != ""):
		return reason.Errorf(reason.InvalidConfig, "token and password authentication are mutually exclusive")
	case c.TokenSource == nil && c.Username == "":
		return reason.Errorf(reason.InvalidConfig, "username is empty")
	case c.TokenSource != nil && c.TLS != nil && c.TLS.InsecureSkipVerify:
		return reason.Errorf(reason.InvalidConfig, "insecureSkipVerify is not allowed with token credentials")
	}
	return nil
}

// Options builds clickhouse-go options.
func (c Config) Options() (*ch.Options, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	maxOpen := c.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = DefaultMaxOpenConns
	}
	lifetime := c.ConnMaxLifetime
	if lifetime <= 0 {
		lifetime = DefaultConnMaxLifetime
	}
	opt := &ch.Options{
		Protocol:        ch.Native,
		Addr:            []string{c.Addr()},
		TLS:             c.TLS,
		Auth:            ch.Auth{Database: c.DefaultDatabase},
		DialTimeout:     dialTimeout,
		ReadTimeout:     readTimeout,
		MaxOpenConns:    maxOpen,
		MaxIdleConns:    max(1, maxOpen/2),
		ConnMaxLifetime: lifetime,
		ClientInfo: ch.ClientInfo{Products: []struct {
			Name    string
			Version string
		}{{Name: "provider-clickhouse", Version: version.Version}}},
	}
	switch c.Protocol {
	case ProtocolNative:
		if c.TokenSource != nil {
			// Sent as user " JWT AUTHENTICATION " with the token as password,
			// once per new connection.
			opt.GetJWT = c.TokenSource.Token
		} else {
			opt.Auth.Username, opt.Auth.Password = c.Username, c.Password
		}
	case ProtocolHTTP:
		opt.Protocol = ch.HTTP
		if c.TokenSource != nil {
			scheme := "http"
			if c.TLS != nil {
				scheme = "https"
			}
			ts, addr := c.TokenSource, c.Addr()
			opt.TransportFunc = func(t *http.Transport) (http.RoundTripper, error) {
				return NewBearerTransport(t, ts, scheme, addr), nil
			}
		} else {
			opt.Auth.Username, opt.Auth.Password = c.Username, c.Password
		}
	}
	return opt, nil
}

// TLSOptions are the inputs to NewTLSConfig.
type TLSOptions struct {
	ServerName         string
	InsecureSkipVerify bool
	CAPEM              []byte
}

// NewTLSConfig builds a TLS configuration (TLS 1.2 or later). An empty CA
// bundle means the system roots.
func NewTLSConfig(o TLSOptions) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         o.ServerName,
		InsecureSkipVerify: o.InsecureSkipVerify, //nolint:gosec // Explicit, documented opt-in; refused with tokens.
	}
	if len(o.CAPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(o.CAPEM) {
			return nil, reason.Wrap(reason.InvalidConfig, errors.New("CA bundle contains no PEM certificates"))
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}
