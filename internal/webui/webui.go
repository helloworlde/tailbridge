// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package webui serves TailBridge's embedded, loopback-only dashboard.
package webui

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/helloworlde/tailbridge/internal/config"
)

//go:embed assets/*
var assets embed.FS

type Options struct {
	Config *config.Config
	Status func() any
}

type handler struct {
	config *config.Config
	status func() any
	static http.Handler
}

func New(options Options) (http.Handler, error) {
	if options.Config == nil || options.Status == nil {
		return nil, errors.New("web UI requires config and status provider")
	}
	staticFS, err := fs.Sub(assets, "assets")
	if err != nil {
		return nil, err
	}
	h := &handler{config: options.Config, status: options.Status, static: http.FileServer(http.FS(staticFS))}
	return h.security(http.HandlerFunc(h.serveHTTP)), nil
}

func (h *handler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/v1/status":
		h.serveStatus(w, r)
	case r.URL.Path == "/api/v1/meta":
		h.serveMeta(w, r)
	case r.URL.Path == "/api/v1/routes/validate":
		h.validateRoute(w, r)
	case r.URL.Path == "/favicon.ico":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/assets/"):
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/assets")
		}
		h.static.ServeHTTP(w, r)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (h *handler) serveStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, h.status())
}

func (h *handler) serveMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	profiles := make([]string, 0, len(h.config.Profiles))
	for _, profile := range h.config.Profiles {
		profiles = append(profiles, profile.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": profiles})
}

func (h *handler) validateRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "origin not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var input config.RouteInput
	if err := dec.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid route input")
		return
	}
	if err := ensureJSONEOF(dec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid route input")
		return
	}
	if _, err := h.config.ValidateRouteInput(input); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true})
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func (h *handler) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			writeError(w, http.StatusMisdirectedRequest, "loopback host required")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self'; style-src 'self'; script-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host := hostport
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Scheme == "http" && strings.EqualFold(u.Host, r.Host)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func NewServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}
