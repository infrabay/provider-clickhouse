// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package clickhouse is the provider's ClickHouse client layer: connection
// options and authentication, a per-ProviderConfig client cache with
// reference counting and a retry on authentication failure, server capability
// detection, error classification and redaction, and the quoting helpers that
// every SQL statement is built with.
//
// Two rules apply everywhere: DDL is built as text from validated input and
// the quoting helpers, and is executed without client-side binding. Reads use
// server-side parameters ({name:String} placeholders).
package clickhouse
