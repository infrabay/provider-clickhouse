// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
)

// Keys of the connection Secrets that User publishes. Compositions may rely on
// them. The crossplane-runtime constants are used where they exist.
const (
	// ConnectionKeyUsername is the ClickHouse user name.
	ConnectionKeyUsername = xpv2.CredentialsSecretUserKey
	// ConnectionKeyPassword is the password of a password user.
	ConnectionKeyPassword = xpv2.CredentialsSecretPasswordKey
	// ConnectionKeyEndpoint is the host of the ProviderConfig endpoint.
	ConnectionKeyEndpoint = xpv2.CredentialsSecretEndpointKey
	// ConnectionKeyPort is the port of the ProviderConfig endpoint.
	ConnectionKeyPort = xpv2.CredentialsSecretPortKey
	// ConnectionKeyProtocol is the protocol, in API casing ("Native").
	ConnectionKeyProtocol = "protocol"
	// ConnectionKeyTLS is "true" or "false".
	ConnectionKeyTLS = "tls"
	// ConnectionKeyJWTProcessor is the token processor of a jwt user.
	ConnectionKeyJWTProcessor = "jwtProcessor"
	// ConnectionKeyRotationToken is the rotation token that was last applied
	// to a generated password.
	ConnectionKeyRotationToken = "rotationToken"
)
