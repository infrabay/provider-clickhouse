// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

//go:build envtest

// Package envtest installs every CRD into a real kube-apiserver (envtest) and
// checks the CEL validation rules with positive and negative cases. CEL rules
// fail their compile or cost checks only there. It also applies every file in
// examples/ with a server-side dry run.
//
// Run with "make envtest" (KUBEBUILDER_ASSETS must point at the envtest
// binaries; the Makefile downloads them with setup-envtest).
package envtest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/infrabay/provider-clickhouse/apis"
)

var kube client.Client

func TestMain(m *testing.M) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "package", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		panic("cannot start envtest (is KUBEBUILDER_ASSETS set?): " + err.Error())
	}
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = apis.AddToScheme(s)
	kube, err = client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func obj(t *testing.T, doc string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return u
}

func ensureNamespace(t *testing.T, name string) {
	t.Helper()
	ns := obj(t, "apiVersion: v1\nkind: Namespace\nmetadata: {name: "+name+"}")
	if err := kube.Create(context.Background(), ns); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
}

type celCase struct {
	doc     string
	wantErr string // substring of the expected error; "" means valid
}

func runCases(t *testing.T, cases map[string]celCase) {
	t.Helper()
	ensureNamespace(t, "team-a")
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := kube.Create(context.Background(), obj(t, tc.doc), client.DryRunAll)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want valid, got: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want error containing %q, got none", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("want error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

const cpcHeader = "apiVersion: clickhouse.m.infrabay.io/v1alpha1\nkind: ClusterProviderConfig\nmetadata: {name: cpc}\n"

func TestClusterProviderConfigValidation(t *testing.T) {
	pw := "  credentials:\n    source: PasswordSecret\n    passwordSecret: {username: p, passwordSecretRef: {namespace: crossplane-system, name: s, key: password}}\n"
	runCases(t, map[string]celCase{
		"ValidPasswordTLS": {doc: cpcHeader + "spec:\n  endpoint: {host: h}\n  tls: {enabled: true}\n" + pw + "  policy: {unrestricted: true}\n"},
		"ValidInsecure":    {doc: cpcHeader + "spec:\n  endpoint: {host: h}\n  allowInsecureTransport: true\n" + pw + "  policy: {}\n"},
		"TLSRequired":      {doc: cpcHeader + "spec:\n  endpoint: {host: h}\n" + pw + "  policy: {}\n", wantErr: "tls.enabled must be true"},
		"PolicyRequired":   {doc: cpcHeader + "spec:\n  endpoint: {host: h}\n  allowInsecureTransport: true\n" + pw, wantErr: "policy"},
		"UnrestrictedWithLists": {
			doc:     cpcHeader + "spec:\n  endpoint: {host: h}\n  allowInsecureTransport: true\n" + pw + "  policy: {unrestricted: true, allowedNamespaces: [a]}\n",
			wantErr: "unrestricted cannot be combined",
		},
		"UnrestrictedWithZooKeeperPaths": {
			doc:     cpcHeader + "spec:\n  endpoint: {host: h}\n  allowInsecureTransport: true\n" + pw + "  policy: {unrestricted: true, zooKeeperPathPatterns: [/x]}\n",
			wantErr: "unrestricted cannot be combined",
		},
		"ZooKeeperPathPatterns": {
			doc: cpcHeader + "spec:\n  endpoint: {host: h}\n  allowInsecureTransport: true\n" + pw + "  policy: {allowedNamespaces: [team-a], zooKeeperPathPatterns: [\"/clickhouse/databases/{namespace}/.+\"]}\n",
		},
		"NamespacePlaceholderInAllowedNamespaces": {
			doc:     cpcHeader + "spec:\n  endpoint: {host: h}\n  allowInsecureTransport: true\n" + pw + "  policy: {allowedNamespaces: [\"team-a\", \"{namespace}\"]}\n",
			wantErr: "{namespace} is not allowed in allowedNamespaces",
		},
		"SkipVerifyWithToken": {
			doc:     cpcHeader + "spec:\n  endpoint: {host: h}\n  tls: {enabled: true, insecureSkipVerify: true}\n  credentials:\n    source: TokenFile\n    tokenFile: {path: /var/run/secrets/clickhouse/token, audience: clickhouse}\n  policy: {}\n",
			wantErr: "insecureSkipVerify is not allowed",
		},
		"SkipVerifyWithPassword": {doc: cpcHeader + "spec:\n  endpoint: {host: h}\n  tls: {enabled: true, insecureSkipVerify: true}\n" + pw + "  policy: {}\n"},
		"BlockMismatch": {
			doc:     cpcHeader + "spec:\n  endpoint: {host: h}\n  allowInsecureTransport: true\n  credentials:\n    source: TokenFile\n    passwordSecret: {username: p, passwordSecretRef: {namespace: ns1, name: s, key: k}}\n  policy: {}\n",
			wantErr: "set exactly the block that matches source",
		},
		"RelativeTokenPath": {
			doc:     cpcHeader + "spec:\n  endpoint: {host: h}\n  allowInsecureTransport: true\n  credentials:\n    source: TokenFile\n    tokenFile: {path: token, audience: clickhouse}\n  policy: {}\n",
			wantErr: "path",
		},
		"GoogleADC": {doc: cpcHeader + "spec:\n  endpoint: {host: h}\n  tls: {enabled: true}\n  credentials:\n    source: GoogleIDToken\n    googleIDToken: {audience: https://clickhouse.example.com}\n  policy: {allowedNamespaces: [team-a]}\n"},
		"HTTPNotYet": {
			doc:     cpcHeader + "spec:\n  endpoint: {host: h}\n  protocol: HTTP\n  tls: {enabled: true}\n" + pw + "  policy: {}\n",
			wantErr: "protocol",
		},
	})
}

func TestProviderConfigValidation(t *testing.T) {
	h := "apiVersion: clickhouse.m.infrabay.io/v1alpha1\nkind: ProviderConfig\nmetadata: {name: pc, namespace: team-a}\n"
	runCases(t, map[string]celCase{
		"ValidPassword": {doc: h + "spec:\n  endpoint: {host: h}\n  tls: {enabled: true}\n  credentials:\n    source: PasswordSecret\n    passwordSecret: {username: p, passwordSecretRef: {name: s, key: password}}\n"},
		"TokenFileRefused": {
			doc:     h + "spec:\n  endpoint: {host: h}\n  tls: {enabled: true}\n  credentials:\n    source: TokenFile\n",
			wantErr: "TokenFile is only allowed on ClusterProviderConfig",
		},
		"GoogleNeedsKey": {
			doc:     h + "spec:\n  endpoint: {host: h}\n  tls: {enabled: true}\n  credentials:\n    source: GoogleIDToken\n    googleIDToken: {audience: a}\n",
			wantErr: "credentialsSecretRef",
		},
	})
}

const dbHeader = "apiVersion: clickhouse.m.infrabay.io/v1alpha1\nkind: Database\nmetadata: {name: db, namespace: team-a}\n"

func TestDatabaseValidation(t *testing.T) {
	runCases(t, map[string]celCase{
		"Valid":            {doc: dbHeader + "spec:\n  forProvider: {name: team-a_db, comment: hi, engine: {name: Atomic}}\n"},
		"ValidEmail":       {doc: dbHeader + "spec:\n  forProvider: {name: \"svc@my-project.iam.gserviceaccount.com\"}\n"},
		"Replicated":       {doc: dbHeader + "spec:\n  forProvider: {engine: {name: Replicated, replicated: {}}}\n"},
		"StarName":         {doc: dbHeader + "spec:\n  forProvider: {name: \"*\"}\n", wantErr: "not a valid database name"},
		"ControlChar":      {doc: dbHeader + "spec:\n  forProvider: {name: \"a\\tb\"}\n", wantErr: "forProvider.name"},
		"ReplicatedAtomic": {doc: dbHeader + "spec:\n  forProvider: {engine: {name: Atomic, replicated: {}}}\n", wantErr: "replicated is only valid"},
		"UnknownEngine":    {doc: dbHeader + "spec:\n  forProvider: {engine: {name: Memory}}\n", wantErr: "Unsupported value"},
	})

	// Transition rules: create, then update.
	ctx := context.Background()
	d := obj(t, strings.Replace(dbHeader, "name: db", "name: immutable", 1)+"spec:\n  forProvider: {name: team-a_x, engine: {name: Atomic}}\n")
	if err := kube.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(u *unstructured.Unstructured){
		"NameImmutable": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "team-a_y", "spec", "forProvider", "name")
		},
		"NameNotRemoved": func(u *unstructured.Unstructured) {
			unstructured.RemoveNestedField(u.Object, "spec", "forProvider", "name")
		},
		"EngineImmutable": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "Replicated", "spec", "forProvider", "engine", "name")
		},
		"EngineNotRemoved": func(u *unstructured.Unstructured) {
			unstructured.RemoveNestedField(u.Object, "spec", "forProvider", "engine")
		},
	} {
		t.Run(name, func(t *testing.T) {
			cur := &unstructured.Unstructured{}
			cur.SetGroupVersionKind(d.GroupVersionKind())
			if err := kube.Get(ctx, client.ObjectKeyFromObject(d), cur); err != nil {
				t.Fatal(err)
			}
			mutate(cur)
			if err := kube.Update(ctx, cur, client.DryRunAll); err == nil {
				t.Fatal("update must be rejected")
			}
		})
	}
	t.Run("CommentMutable", func(t *testing.T) {
		cur := &unstructured.Unstructured{}
		cur.SetGroupVersionKind(d.GroupVersionKind())
		_ = kube.Get(ctx, client.ObjectKeyFromObject(d), cur)
		_ = unstructured.SetNestedField(cur.Object, "new", "spec", "forProvider", "comment")
		if err := kube.Update(ctx, cur, client.DryRunAll); err != nil {
			t.Fatal(err)
		}
	})
}

const userHeader = "apiVersion: clickhouse.m.infrabay.io/v1alpha1\nkind: User\nmetadata: {name: u, namespace: team-a}\n"

func TestUserValidation(t *testing.T) {
	runCases(t, map[string]celCase{
		"ValidJWT": {doc: userHeader + "spec:\n  forProvider:\n    name: alice@example.com\n    authentication: {jwt: {processor: keycloak, claims: {groups: [ch-analysts]}}}\n    hostIP: [10.0.0.0/8]\n"},
		"ValidPassword": {
			doc: userHeader + "spec:\n  forProvider:\n    authentication: {password: {generate: {rotationToken: \"2026-10\"}}}\n    defaultRoles: {policy: Listed, roles: [r1]}\n  writeConnectionSecretToRef: {name: etl}\n",
		},
		"PasswordNeedsSecret": {
			doc:     userHeader + "spec:\n  forProvider:\n    authentication: {password: {generate: {}}}\n",
			wantErr: "password users require writeConnectionSecretToRef",
		},
		"BothMethods": {
			doc:     userHeader + "spec:\n  forProvider:\n    authentication: {password: {generate: {}}, jwt: {processor: p}}\n  writeConnectionSecretToRef: {name: s}\n",
			wantErr: "set exactly one of password or jwt",
		},
		"ProcessorRequired": {
			doc:     userHeader + "spec:\n  forProvider:\n    authentication: {jwt: {}}\n",
			wantErr: "processor",
		},
		"ClaimsMustBeObject": {
			doc:     userHeader + "spec:\n  forProvider:\n    authentication: {jwt: {processor: p, claims: [a]}}\n",
			wantErr: "claims",
		},
		"SecretOrGenerate": {
			doc:     userHeader + "spec:\n  forProvider:\n    authentication: {password: {generate: {}, passwordSecretRef: {name: s, key: k}}}\n  writeConnectionSecretToRef: {name: s}\n",
			wantErr: "set exactly one of passwordSecretRef or generate",
		},
		"ListedNeedsRoles": {
			doc:     userHeader + "spec:\n  forProvider:\n    authentication: {jwt: {processor: p}}\n    defaultRoles: {policy: Listed}\n",
			wantErr: "roles is required for Listed and AllExcept",
		},
		"AllForbidsRoles": {
			doc:     userHeader + "spec:\n  forProvider:\n    authentication: {jwt: {processor: p}}\n    defaultRoles: {policy: All, roles: [r]}\n",
			wantErr: "roles is required for Listed and AllExcept",
		},
		"ShortPassword": {
			doc:     userHeader + "spec:\n  forProvider:\n    authentication: {password: {generate: {length: 8}}}\n  writeConnectionSecretToRef: {name: s}\n",
			wantErr: "length",
		},
	})
}

const grantHeader = "apiVersion: clickhouse.m.infrabay.io/v1alpha1\nkind: Grant\nmetadata: {name: g, namespace: team-a}\n"

func TestGrantValidation(t *testing.T) {
	runCases(t, map[string]celCase{
		"Valid":          {doc: grantHeader + "spec:\n  forProvider: {privileges: [SELECT, SHOW TABLES], databaseRef: {name: analytics}, table: events, roleRef: {name: reader}}\n"},
		"ValidColumns":   {doc: grantHeader + "spec:\n  forProvider: {privileges: [SELECT], database: app, table: t, columns: [a, b], user: alice@example.com}\n"},
		"ValidGlobal":    {doc: grantHeader + "spec:\n  forProvider: {privileges: [SELECT], database: \"*\", role: reader}\n"},
		"NoDatabase":     {doc: grantHeader + "spec:\n  forProvider: {privileges: [SELECT], role: r}\n", wantErr: "database is required"},
		"NoGrantee":      {doc: grantHeader + "spec:\n  forProvider: {privileges: [SELECT], database: app}\n", wantErr: "set exactly one grantee"},
		"TwoGrantees":    {doc: grantHeader + "spec:\n  forProvider: {privileges: [SELECT], database: app, user: u, roleRef: {name: r}}\n", wantErr: "set exactly one grantee"},
		"ColumnsNoTable": {doc: grantHeader + "spec:\n  forProvider: {privileges: [SELECT], database: app, columns: [a], role: r}\n", wantErr: "columns require a specific table"},
		"TableOnGlobal":  {doc: grantHeader + "spec:\n  forProvider: {privileges: [SELECT], database: \"*\", table: t, role: r}\n", wantErr: "a specific table requires a specific database"},
		"BadPrivilege":   {doc: grantHeader + "spec:\n  forProvider: {privileges: [\"SELECT; DROP\"], database: app, role: r}\n", wantErr: "privileges"},
		"CrossNamespaceRef": {
			doc:     grantHeader + "spec:\n  forProvider: {privileges: [SELECT], databaseRef: {name: d, namespace: team-b}, role: r}\n",
			wantErr: "cross-namespace references are not allowed",
		},
		"CrossNamespaceSelector": {
			doc:     grantHeader + "spec:\n  forProvider: {privileges: [SELECT], database: app, userSelector: {matchLabels: {a: b}, namespace: team-b}}\n",
			wantErr: "cross-namespace selectors are not allowed",
		},
		"SameNamespaceRef": {doc: grantHeader + "spec:\n  forProvider: {privileges: [SELECT], databaseRef: {name: d, namespace: \"\"}, role: r}\n"},
	})
}

const roleGrantHeader = "apiVersion: clickhouse.m.infrabay.io/v1alpha1\nkind: RoleGrant\nmetadata: {name: rg, namespace: team-a}\n"

func TestRoleAndRoleGrantValidation(t *testing.T) {
	runCases(t, map[string]celCase{
		"ValidRole":      {doc: "apiVersion: clickhouse.m.infrabay.io/v1alpha1\nkind: Role\nmetadata: {name: r, namespace: team-a}\nspec:\n  forProvider: {name: team-a_reader}\n"},
		"ValidRoleGrant": {doc: roleGrantHeader + "spec:\n  forProvider: {memberOf: analytics_reader, userRef: {name: api}}\n"},
		"NoMemberOf":     {doc: roleGrantHeader + "spec:\n  forProvider: {user: u}\n", wantErr: "memberOf is required"},
		"NoGrantee":      {doc: roleGrantHeader + "spec:\n  forProvider: {memberOf: r}\n", wantErr: "set exactly one grantee"},
		"CrossNamespace": {
			doc:     roleGrantHeader + "spec:\n  forProvider: {memberOfRef: {name: r, namespace: team-b}, user: u}\n",
			wantErr: "cross-namespace references are not allowed",
		},
	})
}

// TestExamples applies every manifest under examples/ with a server-side dry
// run. Namespaces are created first; kinds that are not installed in envtest
// (Crossplane's own types) are skipped.
func TestExamples(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	var files []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")) {
			files = append(files, p)
		}
		return nil
	})
	if len(files) == 0 {
		t.Fatal("no examples found")
	}
	ctx := context.Background()
	for _, f := range files {
		t.Run(strings.TrimPrefix(f, root+string(filepath.Separator)), func(t *testing.T) {
			b, err := os.ReadFile(f) //nolint:gosec // Test fixture path.
			if err != nil {
				t.Fatal(err)
			}
			r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(b)))
			for {
				doc, err := r.Read()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				u := &unstructured.Unstructured{}
				if err := yaml.Unmarshal(doc, &u.Object); err != nil {
					t.Fatal(err)
				}
				if len(u.Object) == 0 {
					continue
				}
				if !strings.HasSuffix(u.GroupVersionKind().Group, "infrabay.io") && u.GetKind() != "Secret" && u.GetKind() != "Namespace" {
					continue
				}
				if ns := u.GetNamespace(); ns != "" {
					ensureNamespace(t, ns)
				}
				if err := kube.Create(ctx, u, client.DryRunAll); err != nil {
					t.Errorf("%s %s: %v", u.GetKind(), u.GetName(), err)
				}
			}
		})
	}
}
