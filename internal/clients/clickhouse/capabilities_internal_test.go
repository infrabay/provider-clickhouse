// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"testing"
	"time"
)

// TestTableDenied: a refused read of a system table is remembered for
// CapabilityTTL, like a probe result, so that a grant given later is picked
// up.
func TestTableDenied(t *testing.T) {
	now := time.Now()
	c := NewCapabilities()
	c.now = func() time.Time { return now }
	if c.TableDenied(TableSettingsProfileElements) {
		t.Fatal("nothing was denied yet")
	}
	c.DenyTable(TableSettingsProfileElements)
	if !c.TableDenied(TableSettingsProfileElements) || c.TableDenied(TableGrants) {
		t.Fatal("only the refused table is denied")
	}
	now = now.Add(CapabilityTTL - time.Second)
	if !c.TableDenied(TableSettingsProfileElements) {
		t.Fatal("the refusal is remembered for CapabilityTTL")
	}
	now = now.Add(2 * time.Second)
	if c.TableDenied(TableSettingsProfileElements) {
		t.Fatal("the refusal expires after CapabilityTTL")
	}
}
