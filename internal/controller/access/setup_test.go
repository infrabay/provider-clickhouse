// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/feature"
	"github.com/crossplane/crossplane-runtime/v2/pkg/gate"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
)

func TestOptions(t *testing.T) {
	o := controller.Options{Logger: logging.NewNopLogger(), PollInterval: time.Minute, Features: &feature.Flags{}}
	var got event.Recorder
	c := Controller[*v1alpha1.Grant]{
		GroupKind: v1alpha1.GrantGroupKind,
		GVK:       v1alpha1.GrantGroupVersionKind,
		Object:    &v1alpha1.Grant{},
		List:      &v1alpha1.GrantList{},
		NewConnector: func(r event.Recorder) managed.TypedExternalConnector[*v1alpha1.Grant] {
			got = r
			return &Connector[*v1alpha1.Grant]{}
		},
	}
	rec := &recorder{}
	base, err := Options(nil, o, c, rec)
	if err != nil {
		t.Fatal(err)
	}
	if got != rec {
		t.Error("the connector must get the controller's recorder")
	}
	// Connector, initializers (always, even empty), deterministic external
	// names, management policies (always), logger, poll interval, recorder.
	if len(base) != 7 {
		t.Errorf("%d base options, want 7", len(base))
	}
	c.References = managed.NewAPISimpleReferenceResolver(nil)
	c.Timeout = time.Minute
	more, err := Options(nil, o, c, rec)
	if err != nil || len(more) != 9 {
		t.Errorf("with references and a timeout: %d options, %v", len(more), err)
	}
}

func TestSetupGated(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: v1alpha1.Group, Version: v1alpha1.Version, Kind: "Grant"}
	o := controller.Options{Gate: new(gate.Gate[schema.GroupVersionKind])}
	ran := make(chan struct{})
	SetupGated(o, gvk, func() error { close(ran); return nil })
	select {
	case <-ran:
		t.Fatal("setup must wait for the CRD")
	case <-time.After(50 * time.Millisecond):
	}
	o.Gate.Set(gvk, true)
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("setup must run once the CRD is installed")
	}
}
