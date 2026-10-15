// Copyright 2026 The provider-clickhouse Authors
// SPDX-License-Identifier: Apache-2.0

package fakegcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ControlPrefix is the root of the test control API.
const ControlPrefix = "/fakegcp/v1/"

// ControlHandler serves the test-only control API. Serve it on its own
// listener, never on the metadata port: it is unauthenticated and mints
// tokens for any identity.
//
//	GET  /healthz                 200 "ok"
//	GET  /fakegcp/v1/state        State
//	GET  /fakegcp/v1/jwks         the published keys
//	GET  /fakegcp/v1/requests     served requests
//	GET  /fakegcp/v1/issued       summaries of issued tokens (no tokens)
//	POST /fakegcp/v1/identity     {"email": "...", "uniqueId": "..."}
//	POST /fakegcp/v1/settings     {"ttl": "5m", "clockOffset": "-90s"} (both optional)
//	POST /fakegcp/v1/fault        {"fault": "expired", "count": 1}  (count <= 0: until cleared)
//	POST /fakegcp/v1/outage       {"target": "metadata|identity|jwks", "mode": "500|503|403|timeout|", "count": 0}
//	POST /fakegcp/v1/keys         {"action": "add|activate|rotate|retire", "kid": "..."} -> {"kid": "..."}
//	POST /fakegcp/v1/mint         {"audience": "...", "email": "...", "uniqueId": "...", "format": "full", "fault": "", "kid": ""} -> {"token": "..."}
//	POST /fakegcp/v1/reset        clear faults and outages, restore identity, ttl and clock offset
//
// Errors are 400 {"error": "..."}.
func (s *Server) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		text(w, http.StatusOK, "ok\n")
	})
	mux.HandleFunc("GET "+ControlPrefix+"state", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, s.State())
	})
	mux.HandleFunc("GET "+ControlPrefix+"jwks", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, s.JWKS())
	})
	mux.HandleFunc("GET "+ControlPrefix+"requests", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, s.Requests())
	})
	mux.HandleFunc("GET "+ControlPrefix+"issued", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, s.Issued())
	})
	mux.HandleFunc("POST "+ControlPrefix+"identity", func(w http.ResponseWriter, r *http.Request) {
		var id Identity
		if !decode(w, r, &id) {
			return
		}
		if err := s.SetIdentity(id); err != nil {
			badRequest(w, err)
			return
		}
		reply(w, s.State())
	})
	mux.HandleFunc("POST "+ControlPrefix+"settings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TTL         string `json:"ttl"`
			ClockOffset string `json:"clockOffset"`
		}
		if !decode(w, r, &req) {
			return
		}
		var ttl, offset time.Duration
		var err error
		if req.TTL != "" {
			if ttl, err = time.ParseDuration(req.TTL); err != nil {
				badRequest(w, fmt.Errorf("ttl: %w", err))
				return
			}
		}
		if req.ClockOffset != "" {
			if offset, err = time.ParseDuration(req.ClockOffset); err != nil {
				badRequest(w, fmt.Errorf("clockOffset: %w", err))
				return
			}
		}
		if req.TTL != "" {
			if err := s.SetTTL(ttl); err != nil {
				badRequest(w, err)
				return
			}
		}
		if req.ClockOffset != "" {
			s.SetClockOffset(offset)
		}
		reply(w, s.State())
	})
	mux.HandleFunc("POST "+ControlPrefix+"fault", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Fault Fault `json:"fault"`
			Count int   `json:"count"`
		}
		if !decode(w, r, &req) {
			return
		}
		if err := s.SetFault(req.Fault, req.Count); err != nil {
			badRequest(w, err)
			return
		}
		reply(w, s.State())
	})
	mux.HandleFunc("POST "+ControlPrefix+"outage", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Target Target     `json:"target"`
			Mode   OutageMode `json:"mode"`
			Count  int        `json:"count"`
		}
		if !decode(w, r, &req) {
			return
		}
		if err := s.SetOutage(req.Target, req.Mode, req.Count); err != nil {
			badRequest(w, err)
			return
		}
		reply(w, s.State())
	})
	mux.HandleFunc("POST "+ControlPrefix+"keys", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action string `json:"action"`
			KID    string `json:"kid"`
		}
		if !decode(w, r, &req) {
			return
		}
		kid := req.KID
		var err error
		switch req.Action {
		case "add":
			kid, err = s.AddKey()
		case "rotate":
			kid, err = s.Rotate()
		case "activate":
			err = s.Activate(req.KID)
		case "retire":
			err = s.Retire(req.KID)
		default:
			err = fmt.Errorf("unknown action %q (want add, activate, rotate or retire)", req.Action)
		}
		if err != nil {
			badRequest(w, err)
			return
		}
		reply(w, map[string]any{"kid": kid, "state": s.State()})
	})
	mux.HandleFunc("POST "+ControlPrefix+"mint", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Audience string `json:"audience"`
			Email    string `json:"email"`
			UniqueID string `json:"uniqueId"`
			Format   string `json:"format"`
			Fault    Fault  `json:"fault"`
			KID      string `json:"kid"`
		}
		if !decode(w, r, &req) {
			return
		}
		o := MintOptions{Audience: req.Audience, Format: req.Format, Fault: req.Fault, KID: req.KID}
		if req.Email != "" || req.UniqueID != "" {
			id := s.Identity()
			if req.Email != "" {
				id.Email = req.Email
			}
			if req.UniqueID != "" {
				id.UniqueID = req.UniqueID
			}
			o.Identity = &id
		}
		tok, err := s.Mint(o)
		if err != nil {
			badRequest(w, err)
			return
		}
		reply(w, map[string]string{"token": tok})
	})
	mux.HandleFunc("POST "+ControlPrefix+"reset", func(w http.ResponseWriter, _ *http.Request) {
		s.Reset()
		reply(w, s.State())
	})
	return mux
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func badRequest(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		badRequest(w, fmt.Errorf("invalid request body: %w", err))
		return false
	}
	return true
}
