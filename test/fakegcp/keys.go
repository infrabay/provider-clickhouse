// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package fakegcp

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // Key IDs only, like Google's 40-hex-digit kids.
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
)

// DefaultKeyBits is the RSA modulus size of generated keys (Google uses 2048).
const DefaultKeyBits = 2048

// key is a signing key. Published keys are in the JWKS; the active key signs
// new tokens.
type key struct {
	kid       string
	priv      *rsa.PrivateKey
	published bool
}

// SigningKey is an RSA signing key and its key ID.
type SigningKey struct {
	// KID is the key ID; derived from the public key (40 hex digits, like
	// Google's) if empty.
	KID string
	Key *rsa.PrivateKey
}

func newKey(bits int) (*key, error) {
	if bits == 0 {
		bits = DefaultKeyBits
	}
	if bits < 2048 {
		return nil, fmt.Errorf("RSA keys must have at least 2048 bits, got %d", bits)
	}
	priv, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return nil, err
	}
	return keyFrom(SigningKey{Key: priv})
}

func keyFrom(sk SigningKey) (*key, error) {
	if sk.Key == nil {
		return nil, errors.New("signing key is nil")
	}
	if n := sk.Key.N.BitLen(); n < 2048 {
		return nil, fmt.Errorf("RSA keys must have at least 2048 bits, got %d", n)
	}
	if err := sk.Key.Validate(); err != nil {
		return nil, fmt.Errorf("invalid RSA key: %w", err)
	}
	kid := sk.KID
	if kid == "" {
		der, err := x509.MarshalPKIXPublicKey(&sk.Key.PublicKey)
		if err != nil {
			return nil, err
		}
		sum := sha1.Sum(der) //nolint:gosec // Key ID, not a security boundary.
		kid = hex.EncodeToString(sum[:])
	}
	return &key{kid: kid, priv: sk.Key}, nil
}

// ParseRSAPrivateKeyPEM parses a PEM "RSA PRIVATE KEY" (PKCS #1) or
// "PRIVATE KEY" (PKCS #8) block, as written by
// openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048.
func ParseRSAPrivateKeyPEM(b []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS #8 key is a %T, not an RSA key", k)
		}
		return rk, nil
	}
	return nil, fmt.Errorf("unsupported PEM block %q (want RSA PRIVATE KEY or PRIVATE KEY)", block.Type)
}

// JWK is one entry of a JSON Web Key Set, in the shape Google publishes.
type JWK struct {
	Alg string `json:"alg"`
	E   string `json:"e"`
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	N   string `json:"n"`
	Use string `json:"use"`
}

// JWKS is a JSON Web Key Set.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

func (k *key) jwk() JWK {
	return JWK{
		Alg: "RS256",
		E:   b64(big.NewInt(int64(k.priv.E)).Bytes()),
		Kid: k.kid,
		Kty: "RSA",
		N:   b64(k.priv.N.Bytes()),
		Use: "sig",
	}
}

// publicPEM is the PKIX PEM of the public key: the "secret" of the HS256
// key-confusion attack, where a verifier uses the RSA public key as an HMAC
// key.
func (k *key) publicPEM() []byte {
	der, err := x509.MarshalPKIXPublicKey(&k.priv.PublicKey)
	if err != nil {
		panic(err) // An RSA public key always marshals.
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// header is a JOSE header. Google's ID tokens carry alg, kid and typ.
type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid,omitempty"`
	Typ string `json:"typ,omitempty"`
}

// sign returns the compact serialization of payload with header h, signed
// according to h.Alg: RS256 with rsaKey, HS256 with hmacKey, or nothing for
// "none".
func sign(h header, payload any, rsaKey *rsa.PrivateKey, hmacKey []byte) (string, error) {
	hb, err := json.Marshal(h)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	input := b64(hb) + "." + b64(pb)
	switch h.Alg {
	case "RS256":
		sum := sha256.Sum256([]byte(input))
		sig, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, sum[:])
		if err != nil {
			return "", err
		}
		return input + "." + b64(sig), nil
	case "HS256":
		m := hmac.New(sha256.New, hmacKey)
		m.Write([]byte(input))
		return input + "." + b64(m.Sum(nil)), nil
	case "none":
		return input + ".", nil
	}
	return "", fmt.Errorf("unsupported alg %q", h.Alg)
}
