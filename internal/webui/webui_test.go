// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/helloworlde/tailbridge/internal/config"
	"github.com/helloworlde/tailbridge/internal/forward"
)

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	cfg := &config.Config{
		Version: 1, RuntimeDir: "/private/tmp/tailbridge-webui-test",
		StartupTimeout: config.Duration{Duration: 2 * time.Minute},
		DrainTimeout:   config.Duration{Duration: 15 * time.Second},
		Profiles:       []config.Profile{{ID: "home", Hostname: "must-not-appear", StateDir: "/private/tmp/tailbridge-webui-test/state"}},
		Routes: []config.Route{{
			ID: "existing", Profile: "home", Kind: "tcp",
			Listen: "127.0.0.1:10001", Upstream: "private-target:22",
			ConnectTimeout: config.Duration{Duration: 10 * time.Second},
		}},
	}
	h, err := New(Options{Config: cfg, Status: func() any {
		return map[string]any{"state": "running", "profiles": []any{}, "routes": []any{}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestProbeAcceptsOnlyConfiguredRouteIDs(t *testing.T) {
	h, err := New(Options{
		Config: &config.Config{Routes: []config.Route{{ID: "web", Kind: "http", Upstream: "http://private-target"}}},
		Status: func() any { return nil },
		Probe: func(ctx context.Context, route config.Route) forward.ProbeResult {
			return forward.ProbeResult{ID: route.ID, Kind: route.Kind, Reachable: true, HTTPStatus: 204}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	good := request(t, h, http.MethodPost, "/api/v1/routes/probe", `{"id":"web"}`)
	if good.Code != http.StatusOK || !strings.Contains(good.Body.String(), `"reachable":true`) {
		t.Fatalf("probe=%d %s", good.Code, good.Body.String())
	}
	if strings.Contains(good.Body.String(), "private-target") {
		t.Fatal("probe exposed target")
	}
	for _, test := range []struct {
		body   string
		status int
	}{
		{`{"id":"missing"}`, http.StatusNotFound},
		{`{"id":"web","upstream":"http://attacker"}`, http.StatusBadRequest},
		{`{"id":"web"} {}`, http.StatusBadRequest},
	} {
		response := request(t, h, http.MethodPost, "/api/v1/routes/probe", test.body)
		if response.Code != test.status {
			t.Errorf("status=%d want=%d", response.Code, test.status)
		}
	}
	badOrigin := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18180/api/v1/routes/probe", strings.NewReader(`{"id":"web"}`))
	badOrigin.Header.Set("Origin", "http://attacker.test")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, badOrigin)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin probe status=%d", response.Code)
	}
}

func request(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:18180"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func TestDashboardAndStatusSecurity(t *testing.T) {
	h := testHandler(t)
	page := request(t, h, http.MethodGet, "/", "")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "TailBridge") {
		t.Fatalf("dashboard status=%d body=%q", page.Code, page.Body.String())
	}
	for _, header := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Cache-Control"} {
		if page.Header().Get(header) == "" {
			t.Fatalf("dashboard omitted security header %s", header)
		}
	}
	favicon := request(t, h, http.MethodGet, "/favicon.ico", "")
	if favicon.Code != http.StatusNoContent {
		t.Fatalf("favicon status=%d", favicon.Code)
	}
	status := request(t, h, http.MethodGet, "/api/v1/status", "")
	for _, secret := range []string{"must-not-appear", "private-target"} {
		if strings.Contains(status.Body.String(), secret) {
			t.Fatalf("status leaked %q: %s", secret, status.Body.String())
		}
	}
}

func TestRejectsNonLoopbackHostAndCrossOrigin(t *testing.T) {
	h := testHandler(t)
	req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusMisdirectedRequest {
		t.Fatalf("non-loopback host status=%d", recorder.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18180/api/v1/routes/validate", strings.NewReader(`{}`))
	req.Host = "127.0.0.1:18180"
	req.Header.Set("Origin", "http://attacker.test")
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST status=%d", recorder.Code)
	}
}

func TestValidateRoute(t *testing.T) {
	h := testHandler(t)
	valid := request(t, h, http.MethodPost, "/api/v1/routes/validate", `{"id":"ssh","profile":"home","kind":"tcp","local_port":10022,"upstream":"host:22"}`)
	if valid.Code != http.StatusOK || !strings.Contains(valid.Body.String(), `"valid":true`) {
		t.Fatalf("valid route status=%d body=%s", valid.Code, valid.Body.String())
	}
	duplicate := request(t, h, http.MethodPost, "/api/v1/routes/validate", `{"id":"existing","profile":"home","kind":"tcp","local_port":10022,"upstream":"host:22"}`)
	if duplicate.Code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate route status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	unknown := request(t, h, http.MethodPost, "/api/v1/routes/validate", `{"id":"ssh","profile":"home","kind":"tcp","local_port":10022,"upstream":"host:22","secret":"x"}`)
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d body=%s", unknown.Code, unknown.Body.String())
	}
}
