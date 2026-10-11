// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package auth provides the token sources the provider authenticates to
// ClickHouse with: Google ID tokens, a token file and a token stored in a
// Secret. Tokens from a file or a Secret are checked before they are sent:
// ClickHouse sees a token before it validates anything, so a token minted for
// another audience (in particular the provider's own Kubernetes API token)
// must never leave the provider.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// A TokenSource returns JWTs for the native protocol's JWT login.
type TokenSource interface {
	// Token returns a token. It is called once per new connection.
	Token(ctx context.Context) (string, error)
	// Invalidate drops any cached token; the next Token call fetches a new one.
	Invalidate()
}

// An Error is returned by token sources. It always carries a reason:
// InvalidConfig for a token that fails the pre-send checks or a source that is
// misconfigured, AuthenticationFailed for a failed token fetch.
type Error struct {
	Source string
	Err    error
}

func (e *Error) Error() string { return e.Source + ": " + e.Err.Error() }

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

func invalid(source, format string, args ...any) error {
	return &Error{Source: source, Err: reason.Errorf(reason.InvalidConfig, format, args...)}
}

// KubernetesServiceAccountDir is the directory of the automounted Kubernetes
// API credential. Token files under it are refused.
const KubernetesServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount/"

// KubernetesTokenPath is the automounted Kubernetes API token.
const KubernetesTokenPath = KubernetesServiceAccountDir + "token"

// maxTokenBytes bounds the size of a token file or Secret value.
const maxTokenBytes = 64 << 10

// defaultLeeway is the clock skew tolerated for exp, matching the server.
const defaultLeeway = 60 * time.Second

// A Checker validates a token before it is sent:
//  1. it must be a JWT (three base64url segments); the payload is decoded
//     without verifying the signature;
//  2. aud must contain Audience;
//  3. aud must not contain any of Forbidden (the audiences of the provider's
//     own Kubernetes API token);
//  4. exp, if present, must not be in the past (with a 60 s leeway).
type Checker struct {
	Audience  string
	Forbidden []string
	Now       func() time.Time
	Leeway    time.Duration
}

type claims struct {
	Aud audience `json:"aud"`
	Exp *float64 `json:"exp"`
}

// audience decodes the aud claim, which is either a string or an array.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*a = audience{s}
		return nil
	}
	var l []string
	if err := json.Unmarshal(b, &l); err != nil {
		return errors.New("aud is neither a string nor an array of strings")
	}
	*a = l
	return nil
}

// Audiences decodes the aud claim of a JWT without verifying it.
func Audiences(token string) ([]string, error) {
	c, err := decode(token)
	if err != nil {
		return nil, err
	}
	return c.Aud, nil
}

func decode(token string) (*claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, errors.New("not a JWT (expected three dot-separated segments)")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, errors.New("not a JWT (payload is not base64url)")
	}
	c := &claims{}
	if err := json.Unmarshal(payload, c); err != nil {
		return nil, fmt.Errorf("not a JWT (payload is not a JSON object: %w)", err)
	}
	return c, nil
}

// Check validates token. Its errors never contain the token.
func (c Checker) Check(token string) error {
	cl, err := decode(token)
	if err != nil {
		return err
	}
	if c.Audience == "" {
		return errors.New("no audience configured")
	}
	if !slices.Contains(cl.Aud, c.Audience) {
		return fmt.Errorf("token audience %q does not contain the configured audience %q", []string(cl.Aud), c.Audience)
	}
	for _, f := range c.Forbidden {
		if f != "" && slices.Contains(cl.Aud, f) {
			return fmt.Errorf("token audience %q contains %q, an audience of the provider's Kubernetes API token; refusing to send it", []string(cl.Aud), f)
		}
	}
	if cl.Exp != nil {
		now := time.Now
		if c.Now != nil {
			now = c.Now
		}
		leeway := c.Leeway
		if leeway == 0 {
			leeway = defaultLeeway
		}
		exp := time.Unix(int64(*cl.Exp), 0)
		if now().After(exp.Add(leeway)) {
			return fmt.Errorf("token expired at %s", exp.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

// KubernetesAPIAudiences returns the audiences of the provider's automounted
// Kubernetes API token at path, or nil if there is none. The provider reads it
// once at start; tokens carrying any of these audiences are never sent.
func KubernetesAPIAudiences(path string) []string {
	b, err := os.ReadFile(path) //nolint:gosec // Fixed, well-known path.
	if err != nil {
		return nil
	}
	aud, err := Audiences(strings.TrimSpace(string(b)))
	if err != nil {
		return nil
	}
	return aud
}

func validToken(source string, raw []byte, c Checker) (string, error) {
	if len(raw) > maxTokenBytes {
		return "", invalid(source, "token is larger than %d bytes", maxTokenBytes)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", invalid(source, "token is empty")
	}
	if err := c.Check(tok); err != nil {
		return "", invalid(source, "%s", err.Error())
	}
	return tok, nil
}
