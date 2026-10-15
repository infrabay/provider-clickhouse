// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

// Command fakegcp runs the test-only fake GCE metadata server and Google JWKS
// of package fakegcp, for end-to-end tests in Kubernetes. Never run it where
// real workloads could reach it: it mints tokens for any identity.
//
//	fakegcp --listen :8080 --control-listen :8081 \
//	  --email provider@example-project.iam.gserviceaccount.com --unique-id 100000000000000000001
//
// Point the provider at it with GCE_METADATA_HOST=<service>:<port> and
// ClickHouse with jwks_uri http://<service>:<port>/oauth2/v3/certs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/infrabay/provider-clickhouse/test/fakegcp"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fakegcp:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("fakegcp", flag.ContinueOnError)
	listen := fs.String("listen", ":8080", "address of the metadata server and the JWKS ("+fakegcp.JWKSPath+")")
	control := fs.String("control-listen", ":8081", "address of the test control API (empty disables it); never expose it beyond the test")
	email := fs.String("email", fakegcp.DefaultEmail, "service account email (email claim)")
	uniqueID := fs.String("unique-id", fakegcp.DefaultUniqueID, "service account unique ID (sub and azp claims)")
	projectID := fs.String("project-id", fakegcp.DefaultProjectID, "project ID")
	projectNumber := fs.String("project-number", fakegcp.DefaultNumericProjectID, "numeric project ID")
	ttl := fs.Duration("ttl", fakegcp.DefaultTTL, "token lifetime (exp - iat)")
	offset := fs.Duration("clock-offset", 0, "shift of iat and exp against the real clock")
	computeEngine := fs.Bool("compute-engine", false, "behave like a Compute Engine VM (google.compute_engine claim) instead of GKE Workload Identity")
	jwksMaxAge := fs.Duration("jwks-max-age", fakegcp.DefaultJWKSMaxAge, "Cache-Control max-age of the JWKS")
	keyFile := fs.String("signing-key-file", "", "PEM RSA private key (PKCS #1 or #8, at least 2048 bits) to sign with, e.g. from a Secret generated at test time; a key is generated if empty")
	kid := fs.String("kid", "", "key ID of --signing-key-file (derived from the public key if empty)")
	jsonLogs := fs.Bool("json-logs", false, "log as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *kid != "" && *keyFile == "" {
		return errors.New("--kid needs --signing-key-file")
	}

	var h slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *jsonLogs {
		h = slog.NewJSONHandler(os.Stderr, nil)
	}
	log := slog.New(h)

	opts := fakegcp.Options{
		Identity:         fakegcp.Identity{Email: *email, UniqueID: *uniqueID},
		ProjectID:        *projectID,
		NumericProjectID: *projectNumber,
		TTL:              *ttl,
		ClockOffset:      *offset,
		JWKSMaxAge:       *jwksMaxAge,
		Logger:           log,
	}
	if *computeEngine {
		opts.ComputeEngine = &fakegcp.ComputeEngine{CreationTimestamp: time.Now().Add(-24 * time.Hour).Unix()}
	}
	if *keyFile != "" {
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			return err
		}
		k, err := fakegcp.ParseRSAPrivateKeyPEM(b)
		if err != nil {
			return fmt.Errorf("%s: %w", *keyFile, err)
		}
		opts.SigningKeys = []fakegcp.SigningKey{{KID: *kid, Key: k}}
	}
	srv, err := fakegcp.New(opts)
	if err != nil {
		return err
	}
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	servers := []*http.Server{{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}}
	if *control != "" {
		servers = append(servers, &http.Server{Addr: *control, Handler: srv.ControlHandler(), ReadHeaderTimeout: 10 * time.Second})
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		ln, err := net.Listen("tcp", s.Addr)
		if err != nil {
			return err
		}
		log.Info("listening", "addr", ln.Addr().String(), "control", s.Addr == *control && *control != "")
		go func() {
			if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	log.Info("fakegcp ready", "email", *email, "kid", srv.ActiveKID(), "ttl", *ttl, "computeEngine", *computeEngine)

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	srv.Close()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdown)
	}
	return err
}
