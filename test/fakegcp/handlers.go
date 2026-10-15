// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package fakegcp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Paths.
const (
	// MetadataPrefix is the root of metadata values.
	MetadataPrefix = "/computeMetadata/v1/"
	// IdentityPath is the ID token endpoint of the default service account.
	IdentityPath = MetadataPrefix + "instance/service-accounts/default/identity"
	// JWKSPath is the path of Google's JWKS (https://www.googleapis.com/oauth2/v3/certs).
	JWKSPath = "/oauth2/v3/certs"
	// GoogleJWKSURI is Google's real JWKS URI, for production processors.
	GoogleJWKSURI = "https://www.googleapis.com" + JWKSPath
)

const (
	flavorHeader = "Metadata-Flavor"
	flavor       = "Google"

	defaultScopes = "https://www.googleapis.com/auth/cloud-platform\nhttps://www.googleapis.com/auth/userinfo.email\n"

	// forbiddenBody imitates the GKE metadata server's answer when the
	// Workload Identity binding is missing. It spans several lines, as the
	// real one does, so tests can check that only the first line of a
	// metadata error reaches a managed resource.
	forbiddenBody = "Unable to generate an ID token: the IAM Service Account Credentials API returned 403 Forbidden.\n" +
		"The Kubernetes service account may not be allowed to impersonate the Google service account\n" +
		"(roles/iam.workloadIdentityUser on the Google service account). [fakegcp]\n"
	internalErrorBody = "Internal Server Error\nfakegcp outage: details that must not reach a managed resource\n"
	unavailableBody   = "Service Unavailable\nfakegcp outage: metadata server under maintenance\n"
)

// Handler serves the metadata endpoints and the JWKS on one port, which is
// how the binary and the Kubernetes manifests run it.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(JWKSPath, s.JWKSHandler())
	mux.Handle("/", s.MetadataHandler())
	return mux
}

// statusWriter records the status of a response.
type statusWriter struct {
	http.ResponseWriter
	status int
	note   string
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (s *Server) logged(next func(w *statusWriter, r *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		next(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		req := Request{Time: s.opts.Now(), Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Status: sw.status, Note: sw.note}
		s.record(req)
		s.log.Info("request", "method", req.Method, "path", req.Path, "query", req.RawQuery, "status", req.Status, "note", req.Note)
	})
}

// MetadataHandler serves the metadata server, modelled on the GCE and GKE
// metadata servers:
//
//   - every response carries "Metadata-Flavor: Google" (the Go library's
//     GCE detection, without GCE_METADATA_HOST, checks it on GET /);
//   - requests with an X-Forwarded-For header are refused with 403;
//   - requests below /computeMetadata/v1/ without "Metadata-Flavor: Google"
//     are refused with 403; GET / needs no header;
//   - only GET and HEAD are allowed (405);
//   - a directory without its trailing slash redirects (301); a directory
//     lists its entries; ?recursive=true is supported for service accounts;
//   - ?alt=json encodes a value as JSON; wait_for_change is not supported
//     (400);
//   - unknown paths and service accounts are 404.
func (s *Server) MetadataHandler() http.Handler {
	return s.logged(s.serveMetadata)
}

func (s *Server) serveMetadata(w *statusWriter, r *http.Request) {
	h := w.Header()
	h.Set(flavorHeader, flavor)
	if s.opts.ComputeEngine != nil {
		h.Set("Server", "Metadata Server for VM")
	} else {
		h.Set("Server", "GKE Metadata Server")
	}
	if _, ok := r.Header["X-Forwarded-For"]; ok {
		w.note = "X-Forwarded-For"
		text(w, http.StatusForbidden, "Request had an X-Forwarded-For header and was rejected.\n")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		text(w, http.StatusMethodNotAllowed, "Method Not Allowed\n")
		return
	}
	switch r.URL.Path {
	case "/":
		text(w, http.StatusOK, "computeMetadata/\n")
		return
	case "/computeMetadata", "/computeMetadata/v1":
		http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
		return
	case "/computeMetadata/":
		text(w, http.StatusOK, "v1/\n")
		return
	}
	if !strings.HasPrefix(r.URL.Path, MetadataPrefix) {
		text(w, http.StatusNotFound, "Not Found\n")
		return
	}
	if r.Header.Get(flavorHeader) != flavor {
		w.note = "missing Metadata-Flavor"
		text(w, http.StatusForbidden, "Missing required header \"Metadata-Flavor\": \"Google\"\n")
		return
	}
	q := r.URL.Query()
	if q.Get("wait_for_change") != "" {
		text(w, http.StatusBadRequest, "wait_for_change is not supported by fakegcp\n")
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, MetadataPrefix)
	identity := strings.HasPrefix(suffix, "instance/service-accounts/") &&
		(strings.HasSuffix(suffix, "/identity") || strings.HasSuffix(suffix, "/token"))
	targets := []Target{TargetMetadata}
	if identity {
		targets = append(targets, TargetIdentity)
	}
	if mode := s.takeOutage(targets...); mode != OutageNone {
		w.note = "outage " + string(mode)
		s.fail(w, r, mode)
		return
	}
	s.serveValue(w, r, suffix)
}

// fail answers a request during an outage.
func (s *Server) fail(w *statusWriter, r *http.Request, mode OutageMode) {
	switch mode {
	case Outage500:
		text(w, http.StatusInternalServerError, internalErrorBody)
	case Outage503:
		text(w, http.StatusServiceUnavailable, unavailableBody)
	case OutageForbidden:
		text(w, http.StatusForbidden, forbiddenBody)
	case OutageTimeout:
		t := time.NewTimer(s.opts.Hang)
		defer t.Stop()
		select {
		case <-r.Context().Done():
			w.note += " (client gave up)"
		case <-s.done:
		case <-t.C:
		}
		text(w, http.StatusGatewayTimeout, "Gateway Timeout\n")
	}
}

func text(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/text")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

func value(w http.ResponseWriter, r *http.Request, v string) {
	if r.URL.Query().Get("alt") == "json" {
		var b []byte
		if _, err := strconv.ParseInt(v, 10, 64); err == nil {
			b = []byte(v)
		} else {
			b, _ = json.Marshal(v)
		}
		w.Header().Set("Content-Type", "application/json")
		etag(w, b)
		_, _ = w.Write(b)
		return
	}
	w.Header().Set("Content-Type", "application/text")
	etag(w, []byte(v))
	_, _ = w.Write([]byte(v))
}

func etag(w http.ResponseWriter, b []byte) {
	sum := sha256.Sum256(b)
	w.Header().Set("ETag", hex.EncodeToString(sum[:8]))
}

func jsonBody(w http.ResponseWriter, code int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	etag(w, b)
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

// directories maps directory suffixes (with trailing slash) to their
// entries. Service accounts are handled separately.
var directories = map[string]string{
	"":          "instance/\nproject/\nuniverse/\n",
	"instance/": "hostname\nid\nname\nservice-accounts/\nzone\n",
	"project/":  "numeric-project-id\nproject-id\n",
	"universe/": "universe-domain\n",
}

func (s *Server) serveValue(w *statusWriter, r *http.Request, suffix string) {
	id := s.Identity()
	zone, instanceID, instanceName := DefaultZone, DefaultInstanceID, DefaultInstanceName
	if ce := s.opts.ComputeEngine; ce != nil {
		zone, instanceID, instanceName = ce.Zone, ce.InstanceID, ce.InstanceName
	}
	if d, ok := directories[suffix]; ok {
		text(w, http.StatusOK, d)
		return
	}
	if _, ok := directories[suffix+"/"]; ok {
		http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
		return
	}
	switch suffix {
	case "project/project-id":
		value(w, r, s.opts.ProjectID)
		return
	case "project/numeric-project-id":
		value(w, r, s.opts.NumericProjectID)
		return
	case "instance/id":
		value(w, r, instanceID)
		return
	case "instance/name":
		value(w, r, instanceName)
		return
	case "instance/hostname":
		value(w, r, instanceName+"."+zone+".c."+s.opts.ProjectID+".internal")
		return
	case "instance/zone":
		value(w, r, "projects/"+s.opts.NumericProjectID+"/zones/"+zone)
		return
	case "universe/universe-domain":
		value(w, r, "googleapis.com")
		return
	case "instance/service-accounts":
		http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
		return
	case "instance/service-accounts/":
		if r.URL.Query().Get("recursive") == "true" {
			sa := serviceAccountJSON(id)
			jsonBody(w, http.StatusOK, map[string]any{"default": sa, id.Email: sa})
			return
		}
		text(w, http.StatusOK, "default/\n"+id.Email+"/\n")
		return
	}
	rest, ok := strings.CutPrefix(suffix, "instance/service-accounts/")
	if !ok {
		text(w, http.StatusNotFound, "Not Found\n")
		return
	}
	account, leaf, hasSlash := strings.Cut(rest, "/")
	if account != "default" && account != id.Email {
		text(w, http.StatusNotFound, "Not Found\n")
		return
	}
	switch {
	case !hasSlash:
		http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
	case leaf == "":
		if r.URL.Query().Get("recursive") == "true" {
			jsonBody(w, http.StatusOK, serviceAccountJSON(id))
			return
		}
		text(w, http.StatusOK, "aliases\nemail\nidentity\nscopes\ntoken\n")
	case leaf == "aliases":
		value(w, r, "default\n")
	case leaf == "email":
		value(w, r, id.Email)
	case leaf == "scopes":
		value(w, r, defaultScopes)
	case leaf == "token":
		s.serveAccessToken(w)
	case leaf == "identity":
		s.serveIDToken(w, r)
	default:
		text(w, http.StatusNotFound, "Not Found\n")
	}
}

func serviceAccountJSON(id Identity) map[string]any {
	return map[string]any{
		"aliases": []string{"default"},
		"email":   id.Email,
		"scopes":  strings.Split(strings.TrimSpace(defaultScopes), "\n"),
	}
}

// serveAccessToken returns an opaque, useless access token in the shape of
// the real endpoint. Nothing accepts it.
func (s *Server) serveAccessToken(w *statusWriter) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	s.mu.Lock()
	ttl := s.ttl
	s.mu.Unlock()
	jsonBody(w, http.StatusOK, map[string]any{
		"access_token": "ya29.fakegcp-" + hex.EncodeToString(b),
		"expires_in":   int(ttl.Seconds()) - 1,
		"token_type":   "Bearer",
	})
}

// serveIDToken serves instance/service-accounts/<sa>/identity: a raw JWT
// for ?audience= (required), ?format=standard|full (default standard) and
// ?licenses=TRUE|FALSE.
func (s *Server) serveIDToken(w *statusWriter, r *http.Request) {
	q := r.URL.Query()
	aud := q.Get("audience")
	if aud == "" {
		text(w, http.StatusBadRequest, "non-empty audience parameter required\n")
		return
	}
	format := q.Get("format")
	if format == "" {
		format = FormatStandard
	}
	if format != FormatStandard && format != FormatFull {
		text(w, http.StatusBadRequest, fmt.Sprintf("invalid format %q: want standard or full\n", format))
		return
	}
	var licenses bool
	switch strings.ToUpper(q.Get("licenses")) {
	case "", "FALSE":
	case "TRUE":
		licenses = true
	default:
		text(w, http.StatusBadRequest, "invalid licenses value: want TRUE or FALSE\n")
		return
	}
	fault := s.takeFault()
	tok, _, err := s.mint(MintOptions{Audience: aud, Format: format, Licenses: licenses, Fault: fault}, "metadata")
	if err != nil {
		text(w, http.StatusInternalServerError, "cannot sign token\n")
		return
	}
	if fault != FaultNone {
		w.note = "fault " + string(fault)
	}
	// The body is the raw JWT, without a trailing newline.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(tok))
}

// JWKSHandler serves the published keys at JWKSPath in the shape of
// https://www.googleapis.com/oauth2/v3/certs, with a Cache-Control max-age.
func (s *Server) JWKSHandler() http.Handler {
	return s.logged(func(w *statusWriter, r *http.Request) {
		if r.URL.Path != JWKSPath {
			text(w, http.StatusNotFound, "Not Found\n")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			text(w, http.StatusMethodNotAllowed, "Method Not Allowed\n")
			return
		}
		if mode := s.takeOutage(TargetJWKS); mode != OutageNone {
			w.note = "outage " + string(mode)
			s.fail(w, r, mode)
			return
		}
		set := s.JWKS()
		kids := make([]string, 0, len(set.Keys))
		for _, k := range set.Keys {
			kids = append(kids, k.Kid)
		}
		w.note = "kids " + strings.Join(kids, ",")
		maxAge := int(s.opts.JWKSMaxAge.Seconds())
		h := w.Header()
		h.Set("Cache-Control", fmt.Sprintf("public, max-age=%d, must-revalidate, no-transform", maxAge))
		h.Set("Expires", s.opts.Now().Add(s.opts.JWKSMaxAge).UTC().Format(http.TimeFormat))
		b, _ := json.MarshalIndent(set, "", "  ")
		h.Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = w.Write(append(b, '\n'))
	})
}
