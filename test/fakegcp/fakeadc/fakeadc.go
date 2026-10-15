// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package fakeadc points the Google client libraries of a test process at a
// fake metadata server (package fakegcp) and tells which library they use.
// It is separate from fakegcp so that the fake, and the image of its binary,
// depend on the standard library only.
package fakeadc

import (
	"reflect"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// UsesNewAuthLibrary reports whether ts, a token source returned by
// google.golang.org/api/idtoken.NewTokenSource, comes from
// cloud.google.com/go/auth (wrapped by its oauth2adapt package: with
// GOOGLE_API_GO_EXPERIMENTAL_ENABLE_NEW_AUTH_LIB=true, and always with a key)
// rather than from golang.org/x/oauth2. Both send the same metadata request,
// so tests check this to know which library they exercised.
func UsesNewAuthLibrary(ts oauth2.TokenSource) bool {
	t := reflect.TypeOf(ts)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t != nil && strings.HasPrefix(t.PkgPath(), "cloud.google.com/go/auth/")
}

// UseFake points the Google client libraries in this process at the fake
// metadata server at host ("host:port") for the rest of the test, and hides
// every other Application Default Credentials source, so that a developer's
// own credentials are never picked up:
//
//   - GCE_METADATA_HOST=host. cloud.google.com/go/compute/metadata then
//     reports OnGCE without probing 169.254.169.254 or DNS, and sends every
//     request to host;
//   - GOOGLE_APPLICATION_CREDENTIALS is cleared;
//   - HOME, CLOUDSDK_CONFIG and APPDATA point at empty directories, which
//     hides gcloud's application_default_credentials.json;
//   - the new-auth-library switches are cleared (tests set them explicitly).
//
// GCE detection is memoized per process, and the project ID is cached after
// the first success; both are harmless across fakes because the host is read
// on every request. Call UseFake before anything in the test binary asks for
// Application Default Credentials without a key: a detection made without
// GCE_METADATA_HOST would probe 169.254.169.254 and metadata.google.internal
// and memoize the answer (false off Google Cloud) for the whole process.
func UseFake(t testing.TB, host string) {
	t.Helper()
	t.Setenv("GCE_METADATA_HOST", host)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("GOOGLE_API_GO_EXPERIMENTAL_ENABLE_NEW_AUTH_LIB", "")
	t.Setenv("GOOGLE_API_GO_EXPERIMENTAL_DISABLE_NEW_AUTH_LIB", "")
}
