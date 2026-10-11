// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func managedCRDKinds(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // Test fixture path.
		if err != nil {
			t.Fatal(err)
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(b, crd); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if slices.Contains(crd.Spec.Names.Categories, "managed") {
			kinds = append(kinds, crd.Spec.Names.Kind)
		}
	}
	slices.Sort(kinds)
	return kinds
}

// TestOnlyReconciledKindsArePackaged: the package must not install a managed
// resource CRD without a controller, or MRs of that kind would be accepted
// and never reconciled.
func TestOnlyReconciledKindsArePackaged(t *testing.T) {
	root := filepath.Join("..", "..")
	packaged := managedCRDKinds(t, filepath.Join(root, "package", "crds"))
	want := slices.Clone(ManagedKinds)
	slices.Sort(want)
	if !slices.Equal(packaged, want) {
		t.Errorf("package/crds has managed kinds %v, controllers exist for %v; update ManagedKinds and SetupGated together", packaged, want)
	}
}
