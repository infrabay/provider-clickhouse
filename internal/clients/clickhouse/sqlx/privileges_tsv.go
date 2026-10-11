// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package sqlx

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	ch "github.com/infrabay/provider-clickhouse/internal/clients/clickhouse"
)

// The privilege catalogues of the supported server versions are recorded in
// testdata/privileges/<version()>.tsv, read through the driver with
// clickhouse.QueryPrivileges (the integration tests record and check them).
// Each line holds one row of system.privileges: the privilege, its aliases as
// a JSON array, its level and its parent group, separated by tabs, with ""
// where the server has NULL. Lines that start with '#' are comments.

// WritePrivilegesTSV writes a catalogue in the recorded format. header is
// written as comment lines.
func WritePrivilegesTSV(w io.Writer, header string, privs []ch.Privilege) error {
	bw := bufio.NewWriter(w)
	for _, line := range strings.Split(strings.TrimSpace(header), "\n") {
		if _, err := fmt.Fprintf(bw, "# %s\n", line); err != nil {
			return err
		}
	}
	for _, p := range privs {
		aliases := p.Aliases
		if aliases == nil {
			aliases = []string{}
		}
		a, err := json.Marshal(aliases)
		if err != nil {
			return err
		}
		for _, f := range []string{p.Name, p.Level, p.Parent, string(a)} {
			if strings.ContainsAny(f, "\t\n\r") {
				return fmt.Errorf("privilege %q: field %q contains a tab or a line break", p.Name, f)
			}
		}
		if _, err := fmt.Fprintf(bw, "%s\t%s\t%s\t%s\n", p.Name, a, p.Level, p.Parent); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// ReadPrivilegesTSV reads a catalogue in the recorded format.
func ReadPrivilegesTSV(r io.Reader) ([]ch.Privilege, error) {
	var out []ch.Privilege
	s := bufio.NewScanner(r)
	n := 0
	for s.Scan() {
		n++
		line := s.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			return nil, fmt.Errorf("line %d: %d fields, want 4", n, len(f))
		}
		p := ch.Privilege{Name: f[0], Level: f[2], Parent: f[3]}
		if err := json.Unmarshal([]byte(f[1]), &p.Aliases); err != nil {
			return nil, fmt.Errorf("line %d: aliases: %w", n, err)
		}
		out = append(out, p)
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
