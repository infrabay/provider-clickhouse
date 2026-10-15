// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package fakegcp

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Token formats of the identity endpoint.
const (
	// FormatStandard omits the email claims (the endpoint's default).
	FormatStandard = "standard"
	// FormatFull adds email and email_verified, and on Compute Engine the
	// google.compute_engine claim. The Go libraries always request it.
	FormatFull = "full"
)

// Claims is the payload of a Google service account ID token.
type Claims struct {
	Aud           string       `json:"aud"`
	Azp           string       `json:"azp"`
	Email         string       `json:"email,omitempty"`
	EmailVerified *bool        `json:"email_verified,omitempty"`
	Exp           int64        `json:"exp"`
	Google        *GoogleClaim `json:"google,omitempty"`
	Iat           int64        `json:"iat"`
	Iss           string       `json:"iss"`
	Sub           string       `json:"sub"`
}

// GoogleClaim is the google claim of format=full tokens on Compute Engine.
type GoogleClaim struct {
	ComputeEngine ComputeEngineClaim `json:"compute_engine"`
}

// ComputeEngineClaim is google.compute_engine.
type ComputeEngineClaim struct {
	InstanceCreationTimestamp int64    `json:"instance_creation_timestamp"`
	InstanceID                string   `json:"instance_id"`
	InstanceName              string   `json:"instance_name"`
	LicenseID                 []string `json:"license_id,omitempty"`
	ProjectID                 string   `json:"project_id"`
	ProjectNumber             int64    `json:"project_number"`
	Zone                      string   `json:"zone"`
}

// MintOptions describe one token.
type MintOptions struct {
	// Audience is the aud claim. Required.
	Audience string
	// Identity overrides the server's identity for this token.
	Identity *Identity
	// Format is FormatFull (the default) or FormatStandard.
	Format string
	// Licenses adds license_id on Compute Engine (format=full only).
	Licenses bool
	// Fault makes this token defective.
	Fault Fault
	// KID signs with this key, which may be retired, instead of the active
	// key.
	KID string
}

// Mint signs a token directly, without the metadata endpoint and without
// consuming a fault set with SetFault. Tests use it for identities other than
// the provider's, e.g. a workload logging in as a jwt user.
func (s *Server) Mint(o MintOptions) (string, error) {
	if o.Format == "" {
		o.Format = FormatFull
	}
	tok, _, err := s.mint(o, "mint")
	return tok, err
}

// wrongAudience, wrongIssuer and wrongTyp are what the corresponding faults
// put into tokens.
const (
	wrongAudience = "https://other-relying-party.example.com"
	wrongIssuer   = "https://issuer.example.com"
	wrongTyp      = "at+jwt"
)

func (s *Server) mint(o MintOptions, via string) (string, Issued, error) {
	if o.Audience == "" {
		return "", Issued{}, errors.New("audience is required")
	}
	if o.Format != FormatFull && o.Format != FormatStandard {
		return "", Issued{}, fmt.Errorf("unknown format %q", o.Format)
	}
	if !o.Fault.valid() {
		return "", Issued{}, fmt.Errorf("unknown fault %q", o.Fault)
	}

	s.mu.Lock()
	id := s.identity
	if o.Identity != nil {
		id = *o.Identity
	}
	ttl, offset := s.ttl, s.offset
	signer := s.active
	if o.KID != "" {
		signer = s.keyLocked(o.KID)
	}
	active, stranger := s.active, s.stranger
	s.mu.Unlock()
	if signer == nil {
		return "", Issued{}, fmt.Errorf("no key %q", o.KID)
	}

	now := s.opts.Now().Add(offset)
	iat, exp := now, now.Add(ttl)
	c := Claims{Aud: o.Audience, Azp: id.UniqueID, Iss: s.opts.Issuer, Sub: id.UniqueID}
	if o.Format == FormatFull {
		verified := true
		c.Email, c.EmailVerified = id.Email, &verified
		if ce := s.opts.ComputeEngine; ce != nil {
			num, _ := strconv.ParseInt(s.opts.NumericProjectID, 10, 64)
			c.Google = &GoogleClaim{ComputeEngine: ComputeEngineClaim{
				InstanceCreationTimestamp: ce.CreationTimestamp,
				InstanceID:                ce.InstanceID,
				InstanceName:              ce.InstanceName,
				ProjectID:                 s.opts.ProjectID,
				ProjectNumber:             num,
				Zone:                      ce.Zone,
			}}
			if o.Licenses {
				c.Google.ComputeEngine.LicenseID = ce.LicenseIDs
			}
		}
	}
	h := header{Alg: "RS256", Kid: signer.kid, Typ: "JWT"}
	rsaKey := signer.priv
	var hmacKey []byte

	switch o.Fault {
	case FaultNone:
	case FaultWrongAudience:
		c.Aud = wrongAudience
	case FaultWrongIssuer:
		c.Iss = wrongIssuer
	case FaultEmailUnverified:
		verified := false
		c.Email, c.EmailVerified = id.Email, &verified
	case FaultNoEmail:
		c.Email, c.EmailVerified = "", nil
	case FaultWrongTyp:
		h.Typ = wrongTyp
	case FaultNoTyp:
		h.Typ = ""
	case FaultUnknownKid:
		h.Kid, rsaKey = stranger.kid, stranger.priv
	case FaultBadSignature:
		h.Kid, rsaKey = active.kid, stranger.priv
	case FaultAlgNone:
		h.Alg = "none"
	case FaultHS256Confusion:
		h.Alg, h.Kid, hmacKey = "HS256", active.kid, active.publicPEM()
	case FaultExpired:
		iat, exp = now.Add(-2*time.Hour), now.Add(-time.Hour)
	case FaultIssuedInFuture:
		iat, exp = now.Add(time.Hour), now.Add(time.Hour+ttl)
	}
	c.Iat, c.Exp = iat.Unix(), exp.Unix()

	tok, err := sign(h, c, rsaKey, hmacKey)
	if err != nil {
		return "", Issued{}, err
	}
	is := Issued{
		Time: s.opts.Now(), Kid: h.Kid, Alg: h.Alg, Typ: h.Typ, Audience: c.Aud, Issuer: c.Iss,
		Email: c.Email, Subject: c.Sub, IssuedAt: time.Unix(c.Iat, 0).UTC(), Expiry: time.Unix(c.Exp, 0).UTC(),
		Fault: o.Fault, Via: via,
	}
	s.mu.Lock()
	if len(s.issued) == maxLog {
		s.issued = s.issued[1:]
	}
	s.issued = append(s.issued, is)
	s.mu.Unlock()
	s.log.Info("issued ID token", "via", via, "kid", is.Kid, "alg", is.Alg, "typ", is.Typ, "aud", is.Audience,
		"iss", is.Issuer, "email", is.Email, "sub", is.Subject, "iat", is.IssuedAt, "exp", is.Expiry, "fault", string(is.Fault))
	return tok, is, nil
}
