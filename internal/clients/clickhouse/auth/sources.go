// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/infrabay/provider-clickhouse/internal/reason"
)

// Source names, used in errors.
const (
	SourceTokenFile     = "TokenFile"
	SourceTokenSecret   = "TokenSecret"
	SourceGoogleIDToken = "GoogleIDToken"
)

// FileSource reads a token from a file on every call, so a rotated file (for
// example a kubelet-rotated projected service account token) is picked up by
// the next connection.
type FileSource struct {
	path    string
	checker Checker
}

// NewFileSource returns a token source that reads path. Paths under the
// automounted Kubernetes service account directory are refused: that token is
// the provider's own Kubernetes API credential.
func NewFileSource(path string, c Checker) (*FileSource, error) {
	if !filepath.IsAbs(path) {
		return nil, invalid(SourceTokenFile, "path %q is not absolute", path)
	}
	clean := filepath.Clean(path)
	if strings.HasPrefix(clean+"/", KubernetesServiceAccountDir) {
		return nil, invalid(SourceTokenFile, "path %q is the provider's Kubernetes API credential and is refused", path)
	}
	return &FileSource{path: clean, checker: c}, nil
}

// Token reads and checks the token file.
func (s *FileSource) Token(_ context.Context) (string, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return "", invalid(SourceTokenFile, "cannot read token file %q: %s", s.path, errors.Unwrap(err))
	}
	defer f.Close() //nolint:errcheck // Read-only file.
	b, err := io.ReadAll(io.LimitReader(f, maxTokenBytes+1))
	if err != nil {
		return "", invalid(SourceTokenFile, "cannot read token file %q", s.path)
	}
	return validToken(SourceTokenFile, b, s.checker)
}

// Invalidate is a no-op: the file is read on every call.
func (s *FileSource) Invalidate() {}

// SecretSource reads a token from a Secret key on every call, through an
// uncached API reader. Dials are rare (connections are pooled), and a stale
// cache would defeat the retry on authentication failure.
type SecretSource struct {
	reader  client.Reader
	key     types.NamespacedName
	field   string
	checker Checker
}

// NewSecretSource returns a token source that reads key[field].
func NewSecretSource(r client.Reader, key types.NamespacedName, field string, c Checker) *SecretSource {
	return &SecretSource{reader: r, key: key, field: field, checker: c}
}

// Token reads and checks the token.
func (s *SecretSource) Token(ctx context.Context) (string, error) {
	sec := &corev1.Secret{}
	if err := s.reader.Get(ctx, s.key, sec); err != nil {
		return "", invalid(SourceTokenSecret, "cannot get Secret %s: %s", s.key, err)
	}
	v, ok := sec.Data[s.field]
	if !ok {
		return "", invalid(SourceTokenSecret, "Secret %s has no key %q", s.key, s.field)
	}
	return validToken(SourceTokenSecret, v, s.checker)
}

// Invalidate is a no-op: the Secret is read on every call.
func (s *SecretSource) Invalidate() {}

// NewSourceFunc builds the underlying Google token source.
type NewSourceFunc func() (oauth2.TokenSource, error)

// GoogleSource returns Google ID tokens with the given audience. Tokens are
// cached until shortly before expiry. Invalidate rebuilds the source.
type GoogleSource struct {
	newSource NewSourceFunc

	mu sync.Mutex
	ts oauth2.TokenSource
}

// NewGoogleSource returns a token source for Google ID tokens. With keyJSON
// it uses that service account key, which is validated first; otherwise
// Application Default Credentials (on GKE with Workload Identity, the metadata
// server). The underlying source is built lazily, with
// context.Background(): the library keeps that context for later refreshes,
// and a cancelled reconcile context would break every refresh after it.
func NewGoogleSource(audience string, keyJSON []byte) (*GoogleSource, error) {
	if audience == "" {
		return nil, invalid(SourceGoogleIDToken, "audience is required")
	}
	var opts []option.ClientOption
	if keyJSON != nil {
		if err := ValidateServiceAccountKey(keyJSON); err != nil {
			return nil, err
		}
		opts = append(opts, option.WithAuthCredentialsJSON(option.ServiceAccount, keyJSON))
	}
	return NewGoogleSourceWith(func() (oauth2.TokenSource, error) {
		return idtoken.NewTokenSource(context.Background(), audience, opts...)
	}), nil
}

// NewGoogleSourceWith returns a GoogleSource over a custom underlying source.
// Used by tests.
func NewGoogleSourceWith(fn NewSourceFunc) *GoogleSource {
	return &GoogleSource{newSource: fn}
}

// Token returns a cached or fresh ID token.
func (s *GoogleSource) Token(_ context.Context) (string, error) {
	s.mu.Lock()
	if s.ts == nil {
		src, err := s.newSource()
		if err != nil {
			s.mu.Unlock()
			return "", &Error{Source: SourceGoogleIDToken, Err: reason.Wrap(reason.AuthenticationFailed, scrub(err))}
		}
		s.ts = oauth2.ReuseTokenSource(nil, src)
	}
	ts := s.ts
	s.mu.Unlock()

	t, err := ts.Token()
	if err != nil {
		return "", &Error{Source: SourceGoogleIDToken, Err: reason.Wrap(reason.AuthenticationFailed, scrub(err))}
	}
	// All Google ID token sources put the ID token in AccessToken.
	if t.AccessToken == "" {
		return "", &Error{Source: SourceGoogleIDToken, Err: reason.Errorf(reason.AuthenticationFailed, "empty ID token")}
	}
	return t.AccessToken, nil
}

// Invalidate drops the cached token.
func (s *GoogleSource) Invalidate() {
	s.mu.Lock()
	s.ts = nil
	s.mu.Unlock()
}

// scrub keeps the first line of a token endpoint error and drops response
// bodies, which the Google library includes in its errors.
func scrub(err error) error {
	msg := err.Error()
	if i := strings.IndexAny(msg, "\r\n"); i >= 0 {
		msg = msg[:i]
	}
	for _, marker := range []string{"Response:", "response:", "body:"} {
		if i := strings.Index(msg, marker); i >= 0 {
			msg = strings.TrimSpace(msg[:i])
		}
	}
	if len(msg) > 256 {
		msg = msg[:256] + "..."
	}
	return errors.New(msg)
}

// Allowed values in a service account key.
const (
	googleTokenURI       = "https://oauth2.googleapis.com/token"
	googleUniverseDomain = "googleapis.com"
)

// ValidateServiceAccountKey checks a Google service account key before it is
// used: type must be service_account, token_uri empty or Google's token
// endpoint, and universe_domain empty or googleapis.com. The Google library
// honours token_uri from the key and puts token endpoint response bodies into
// its errors, so an unchecked key would let its author make the provider POST
// a signed assertion to any URL and read the reply back.
func ValidateServiceAccountKey(b []byte) error {
	var k struct {
		Type           string `json:"type"`
		TokenURI       string `json:"token_uri"`
		UniverseDomain string `json:"universe_domain"`
	}
	if err := json.Unmarshal(b, &k); err != nil {
		return invalid(SourceGoogleIDToken, "service account key is not valid JSON")
	}
	if k.Type != "service_account" {
		return invalid(SourceGoogleIDToken, "credentials type %q is not supported; only service_account keys are", k.Type)
	}
	if k.TokenURI != "" && k.TokenURI != googleTokenURI {
		return invalid(SourceGoogleIDToken, "token_uri %q is not allowed (want %q)", k.TokenURI, googleTokenURI)
	}
	if k.UniverseDomain != "" && k.UniverseDomain != googleUniverseDomain {
		return invalid(SourceGoogleIDToken, "universe_domain %q is not allowed (want %q)", k.UniverseDomain, googleUniverseDomain)
	}
	return nil
}

// Static is a token source that returns a fixed token after checking it. Used
// by tests and tools.
type Static struct {
	Value   string
	Checker *Checker
}

// Token returns the token.
func (s Static) Token(_ context.Context) (string, error) {
	if s.Checker != nil {
		return validToken("Static", []byte(s.Value), *s.Checker)
	}
	if s.Value == "" {
		return "", invalid("Static", "token is empty")
	}
	return s.Value, nil
}

// Invalidate is a no-op.
func (Static) Invalidate() {}

// Interface checks.
var (
	_ TokenSource = &FileSource{}
	_ TokenSource = &SecretSource{}
	_ TokenSource = &GoogleSource{}
	_ TokenSource = Static{}
)

// String implements fmt.Stringer without exposing any token.
func (s *FileSource) String() string { return fmt.Sprintf("TokenFile(%s)", s.path) }
