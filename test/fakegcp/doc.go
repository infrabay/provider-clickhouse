// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Package fakegcp is a test-only fake of the two Google endpoints that the
// GoogleIDToken credentials source depends on:
//
//   - the GCE/GKE metadata server, which the Google client libraries reach
//     through GCE_METADATA_HOST (Application Default Credentials on GKE with
//     Workload Identity): detection root, project and instance values, the
//     service account's email, scopes, access token and ID token
//     (instance/service-accounts/default/identity);
//   - Google's public signing keys (https://www.googleapis.com/oauth2/v3/certs),
//     which ClickHouse's jwt_dynamic_jwks token processor fetches.
//
// ID tokens are shaped like Google service account ID tokens (RS256, kid and
// typ JWT in the header; iss https://accounts.google.com, aud, azp, sub,
// email, email_verified, iat, exp in the payload) and signed with throwaway
// RSA keys generated in memory. A control surface, as Go methods and as a
// separate HTTP handler (ControlHandler, never on the metadata port), rotates
// keys, switches the next tokens to faulty variants and simulates outages.
//
// Nothing here talks to Google, and nothing here is fit for production: the
// control API is unauthenticated and mints tokens for any identity.
package fakegcp
