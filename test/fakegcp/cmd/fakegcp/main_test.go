// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunRejectsBadArguments covers the errors run returns before it serves.
func TestRunRejectsBadArguments(t *testing.T) {
	dir := t.TempDir()
	weak, err := rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // Deliberately too small: must be refused.
	if err != nil {
		t.Fatal(err)
	}
	weakFile := filepath.Join(dir, "weak.pem")
	if err := os.WriteFile(weakFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(weak)}), 0o600); err != nil {
		t.Fatal(err)
	}
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"unknown flag":       {[]string{"--nope"}, "flag provided but not defined"},
		"kid without key":    {[]string{"--kid", "k1"}, "--kid needs --signing-key-file"},
		"missing key file":   {[]string{"--signing-key-file", filepath.Join(dir, "absent.pem")}, "no such file"},
		"not a PEM key":      {[]string{"--signing-key-file", garbage}, "no PEM block"},
		"key too small":      {[]string{"--signing-key-file", weakFile}, "at least 2048 bits"},
		"bad listen address": {[]string{"--listen", "127.0.0.1:99999", "--control-listen", ""}, "invalid port"},
	} {
		t.Run(name, func(t *testing.T) {
			err := run(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run(%q) = %v, want an error containing %q", tc.args, err, tc.want)
			}
		})
	}
}
