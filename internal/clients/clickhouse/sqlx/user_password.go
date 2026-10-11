// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// Password generation limits, matching the User CRD (generate.length).
const (
	MinPasswordLength     = 16
	MaxPasswordLength     = 128
	DefaultPasswordLength = 32
)

// saltBytes is the number of random bytes in a salt. The salt is sent as
// their hex text, 32 characters.
const saltBytes = 16

// passwordAlphabet is what generated passwords are drawn from: [A-Za-z0-9].
const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// HashPassword returns hex(sha256(password + salt)), the BY value of
// IDENTIFIED WITH sha256_hash ... SALT salt. The server appends the SALT
// literal exactly as sent, so the salt is appended as its hex text, never as
// the bytes it encodes: hashing the decoded bytes creates a user that can
// never log in (516). Test vector: HashPassword("Pa55word",
// "00112233445566778899aabbccddeeff") is
// "6257310c34f98d6e96c672db8cee44e0077f4e03eb3581a7260ccd6861848188".
func HashPassword(password, salt string) string {
	sum := sha256.Sum256([]byte(password + salt))
	return hex.EncodeToString(sum[:])
}

// NewPasswordHash hashes password with a fresh random salt. Every
// application of a password gets a new salt.
func NewPasswordHash(password string) (PasswordHash, error) {
	return newPasswordHash(rand.Reader, password)
}

func newPasswordHash(r io.Reader, password string) (PasswordHash, error) {
	b := make([]byte, saltBytes)
	if _, err := io.ReadFull(r, b); err != nil {
		return PasswordHash{}, fmt.Errorf("cannot generate a password salt: %w", err)
	}
	salt := hex.EncodeToString(b)
	return PasswordHash{Hash: HashPassword(password, salt), Salt: salt}, nil
}

// GeneratePassword returns a random password of length characters drawn
// uniformly from [A-Za-z0-9] with crypto/rand.
func GeneratePassword(length int) (string, error) {
	return generatePassword(rand.Reader, length)
}

func generatePassword(r io.Reader, length int) (string, error) {
	if length < MinPasswordLength || length > MaxPasswordLength {
		return "", fmt.Errorf("password length %d is outside [%d, %d]", length, MinPasswordLength, MaxPasswordLength)
	}
	// Rejection sampling: bytes from 248 up would make the first 248 mod 62
	// characters more likely.
	const limit = 256 - 256%len(passwordAlphabet)
	out := make([]byte, 0, length)
	buf := make([]byte, length)
	for len(out) < length {
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", fmt.Errorf("cannot generate a password: %w", err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, passwordAlphabet[int(b)%len(passwordAlphabet)])
			if len(out) == length {
				break
			}
		}
	}
	return string(out), nil
}
