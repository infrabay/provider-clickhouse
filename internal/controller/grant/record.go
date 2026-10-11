// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package grant

import (
	"context"
	"encoding/json"

	"github.com/infrabay/provider-clickhouse/apis/v1alpha1"
	"github.com/infrabay/provider-clickhouse/internal/clients/clickhouse/sqlx"
	"github.com/infrabay/provider-clickhouse/internal/controller/ownership"
)

// The record of the grant option a Grant gave at its target (design 5.6,
// D-35). The server leaves a grant option out of system.grants and SHOW
// GRANTS when the privileges at its object equal those inherited from above
// and a sibling name shares its first character (sqlx.Analyze), so the rows
// alone could neither confirm a grant option the Grant needs, which would
// make it grant again on every reconcile, nor reveal one it must revoke on
// deletion or when withGrantOption is turned off. The Grant therefore
// records the flags it granted WITH GRANT OPTION, together with the
// grantee's UUID, in the annotation clickhouse.m.infrabay.io/grant-option.
//
// Like every annotation, the record is writable by anyone who can edit the
// MR. A forged record only changes what this Grant assumes about, or
// revokes from, its own grantee at its own target; every write is still
// bound to an owned grantee and the name policy.

// grantOptionRecord is the JSON form of the annotation.
type grantOptionRecord struct {
	GranteeID  string   `json:"granteeID"`
	Privileges []string `json:"privileges"`
}

// recordOf returns the flags the Grant recorded for the grantee with UUID
// granteeID. A record of another grantee (one that was dropped and created
// again) or one that does not parse is empty.
func recordOf(cr *v1alpha1.Grant, granteeID string) sqlx.PrivilegeSet {
	v, ok := cr.GetAnnotations()[v1alpha1.AnnotationGrantOption]
	if !ok || granteeID == "" {
		return sqlx.PrivilegeSet{}
	}
	var r grantOptionRecord
	if err := json.Unmarshal([]byte(v), &r); err != nil || r.GranteeID != granteeID {
		return sqlx.PrivilegeSet{}
	}
	return sqlx.NewPrivilegeSet(r.Privileges...)
}

// saveRecord persists the record of the grant option for the grantee with
// UUID granteeID with a JSON merge patch on the MR's annotations, or removes
// the annotation when nothing is recorded. crossplane-runtime does not
// persist annotations that Update or Delete set.
func (e *External) saveRecord(ctx context.Context, cr *v1alpha1.Grant, granteeID string, rec sqlx.PrivilegeSet) error {
	_, held := cr.GetAnnotations()[v1alpha1.AnnotationGrantOption]
	if len(rec) == 0 {
		if !held {
			return nil
		}
		return ownership.Patch(ctx, e.Kube, cr, nil, []string{v1alpha1.AnnotationGrantOption})
	}
	if held && recordOf(cr, granteeID).Equal(rec) {
		return nil
	}
	b, err := json.Marshal(grantOptionRecord{GranteeID: granteeID, Privileges: rec.Sorted()})
	if err != nil {
		return err
	}
	return ownership.Patch(ctx, e.Kube, cr, map[string]string{v1alpha1.AnnotationGrantOption: string(b)}, nil)
}
