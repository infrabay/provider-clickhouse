// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package fakegcp

import (
	"fmt"
	"time"
)

// GoogleProcessorName is the name of the ClickHouse token processor for
// Google ID tokens used in the docs, tests and manifests.
const GoogleProcessorName = "google_jwt"

// GoogleProcessor is the ClickHouse (Antalya) token processor that validates
// Google service account ID tokens. Production and tests render it with
// YAML; only JWKSURI and the cache lifetimes differ.
//
// Never pair it with user_directories/token: any Google service account can
// mint a token for any audience, so the list of jwt users (each pinned to
// this processor) is the only allow-list.
type GoogleProcessor struct {
	// Name is the processor's name, referenced by jwt users.
	Name string
	// JWKSURI is GoogleJWKSURI in production.
	JWKSURI string
	// Audience is expected_audience: the GoogleIDToken audience of the
	// provider configuration, unique per ClickHouse deployment.
	Audience string
	// JWKSCacheLifetime is how long fetched keys (and a failed fetch) are
	// cached. A key Google starts signing with is accepted only after the
	// cached set expires; Google publishes keys well ahead of using them.
	JWKSCacheLifetime time.Duration
	// TokenCacheLifetime is how long a validated token is accepted without
	// validating it again. ClickHouse 26.6 does not bound the cache entry by
	// the token's exp, so this also bounds how long an expired token keeps
	// working once it was accepted.
	TokenCacheLifetime time.Duration
}

// ProductionGoogleProcessor returns the processor for Google's real JWKS.
func ProductionGoogleProcessor(audience string) GoogleProcessor {
	return GoogleProcessor{
		Name:               GoogleProcessorName,
		JWKSURI:            GoogleJWKSURI,
		Audience:           audience,
		JWKSCacheLifetime:  time.Hour,
		TokenCacheLifetime: 5 * time.Minute,
	}
}

// YAML renders the processor as config.d/token_processors.yaml.
func (p GoogleProcessor) YAML() string {
	return fmt.Sprintf(`token_processors:
  %s:
    type: jwt_dynamic_jwks
    jwks_uri: %q
    expected_issuer: %q
    expected_audience: %q
    expected_typ: JWT
    claims: '{"email_verified": true}'
    username_claim: email
    jwks_cache_lifetime: %d
    token_cache_lifetime: %d
`, p.Name, p.JWKSURI, DefaultIssuer, p.Audience, int(p.JWKSCacheLifetime.Seconds()), int(p.TokenCacheLifetime.Seconds()))
}
