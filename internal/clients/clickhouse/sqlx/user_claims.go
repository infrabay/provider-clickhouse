// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// CanonicalClaims returns the canonical form of the JSON object a jwt user's
// token must contain (CLAIMS): object keys sorted at every level, no
// insignificant whitespace, numbers exactly as written (decoded with
// UseNumber), no HTML escaping. The provider sends claims in this form and
// compares desired and observed claims in it, because the server keeps the
// literal as sent. An empty object yields "": it constrains nothing, so no
// CLAIMS clause is sent. Anything but a single JSON object is an error.
func CanonicalClaims(raw []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", errors.New("claims are not valid JSON")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("claims must be a single JSON object")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return "", errors.New("claims must be a JSON object")
	}
	if len(obj) == 0 {
		return "", nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return "", errors.New("claims cannot be encoded")
	}
	return string(bytes.TrimSuffix(b.Bytes(), []byte("\n"))), nil
}
