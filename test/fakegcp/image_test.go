// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package fakegcp

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestImageBuildsOffline checks that the fakegcp binary depends on the
// standard library only: the Dockerfile builds it with GOPROXY=off from
// go.mod, go.sum and test/fakegcp alone, so any other module (even one in
// go.sum) breaks the image build, which only make e2e runs. Helpers that need
// other modules belong in a subpackage such as fakeadc.
func TestImageBuildsOffline(t *testing.T) {
	const self = "github.com/infrabay/provider-clickhouse/test/fakegcp"
	for _, dir := range []string{".", "cmd/fakegcp"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f) //nolint:gosec // Files of this package.
			if err != nil {
				t.Fatal(err)
			}
			ast, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range ast.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				first, _, _ := strings.Cut(path, "/")
				if strings.Contains(first, ".") && path != self {
					t.Errorf("%s imports %s; the fakegcp image is built without module downloads (standard library only)", f, path)
				}
			}
		}
	}
}
