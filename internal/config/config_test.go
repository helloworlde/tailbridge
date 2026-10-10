// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadRejectsInvalidEndpoints(t *testing.T) {
	for _, route := range []string{
		`{id: bad, listen: "127.0.0.1:99999", upstream: "server:22"}`,
		`{id: bad, listen: "127.0.0.1:", upstream: "server:22"}`,
		`{id: bad, listen: "127.0.0.1:http", upstream: "server:22"}`,
		`{id: bad, local_port: 18080, upstream: ":22"}`,
		`{id: bad, local_port: 18080, upstream: "server:99999"}`,
		`{id: bad, local_port: 18080, upstream: "server:0"}`,
		`{id: bad, local_port: 18080, upstream: "server:"}`,
		`{id: bad, local_port: 18080, upstream: "server name:22"}`,
		`{id: bad, local_port: 18080, upstream: "https://:443"}`,
		`{id: bad, local_port: 18080, upstream: "https://server:99999"}`,
		`{id: bad, local_port: 18080, upstream: "https://server:"}`,
	} {
		t.Run(route, func(t *testing.T) {
			body := "version: 1\nprofiles: [{id: home, hostname: test}]\nroutes: [" + route + "]\n"
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatal("invalid endpoint accepted")
			}
		})
	}
	for _, address := range []string{"127.0.0.1:65536", "[::1]:-1"} {
		body := fmt.Sprintf("version: 1\nprofiles: [{id: home, hostname: test}]\nweb_ui: {listen: %q}\n", address)
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("invalid Web UI endpoint %q accepted", address)
		}
	}
}

func TestLoadRejectsMissingOrInvalidCA(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(t.TempDir(), "missing.pem"), bad} {
		body := fmt.Sprintf("version: 1\nprofiles: [{id: home, hostname: test}]\nroutes: [{id: web, local_port: 18080, upstream: \"https://service\", ca_file: %q}]\n", path)
		if _, err := Load(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "ca_file") {
			t.Fatalf("invalid CA accepted: %v", err)
		}
	}
}

func TestDefaultPathForExecutable(t *testing.T) {
	got := defaultPathForExecutable(filepath.Join(t.TempDir(), "dist", "tailbridge"))
	if filepath.Base(got) != "config.yaml" || filepath.Base(filepath.Dir(got)) != "dist" {
		t.Fatalf("default path=%q, want config.yaml beside executable", got)
	}
}

func TestLoadDefaultsAndNormalize(t *testing.T) {
	path := writeConfig(t, `
version: 1
runtime_dir: run
profiles:
  - id: home
    hostname: tailbridge-test
    state_dir: state/home
routes:
  - id: db
    profile: home
    kind: TCP
    listen: 127.0.0.1:15432
    upstream: database:5432
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StartupTimeout.Duration != 2*time.Minute || cfg.DrainTimeout.Duration != 15*time.Second {
		t.Fatalf("unexpected defaults: startup=%v drain=%v", cfg.StartupTimeout.Duration, cfg.DrainTimeout.Duration)
	}
	if !filepath.IsAbs(cfg.RuntimeDir) || !filepath.IsAbs(cfg.Profiles[0].StateDir) {
		t.Fatalf("paths were not normalized: %#v", cfg)
	}
	if got := cfg.Routes[0].Kind; got != "tcp" {
		t.Fatalf("kind=%q, want tcp", got)
	}
	if got := cfg.Routes[0].ConnectTimeout.Duration; got != 10*time.Second {
		t.Fatalf("connect timeout=%v, want 10s", got)
	}
}

func TestLoadSimplifiedRoute(t *testing.T) {
	path := writeConfig(t, `
version: 1
web_ui: {listen: "127.0.0.1:18180"}
profiles:
  - {id: home, hostname: test}
routes:
  - {id: ssh, local_port: 10022, upstream: "host:22"}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	route := cfg.Routes[0]
	if route.Profile != "home" || route.Kind != "tcp" || route.Listen != "127.0.0.1:10022" || route.LocalPort != 0 {
		t.Fatalf("simplified route not normalized: %#v", route)
	}
}

func TestLoadInfersHTTPRoute(t *testing.T) {
	path := writeConfig(t, `
version: 1
profiles: [{id: home, hostname: test}]
routes: [{id: web, local_port: 18080, upstream: "https://service:443"}]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Routes[0].Kind; got != "http" {
		t.Fatalf("kind=%q, want http", got)
	}
}

func TestValidateRouteInputDoesNotMutateConfig(t *testing.T) {
	path := writeConfig(t, `
version: 1
profiles: [{id: home, hostname: test}]
routes: []
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	route, err := cfg.ValidateRouteInput(RouteInput{ID: "web", Kind: "http", LocalPort: 18080, Upstream: "http://service:3000"})
	if err != nil {
		t.Fatal(err)
	}
	if route.Profile != "home" || route.Listen != "127.0.0.1:18080" || len(cfg.Routes) != 0 {
		t.Fatalf("unexpected validation result route=%#v configured=%d", route, len(cfg.Routes))
	}
}

func TestLoadRejectsUnknownOrSecretFields(t *testing.T) {
	for _, field := range []string{"auth_key: tskey-secret", "mystery: value"} {
		path := writeConfig(t, `
version: 1
profiles:
  - id: home
    hostname: tailbridge-test
    `+field+`
`)
		if _, err := Load(path); err == nil {
			t.Fatalf("Load accepted field %q", field)
		}
	}
}

func TestValidationFailures(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "route conflicts with web UI",
			body: `
version: 1
web_ui: {listen: "127.0.0.1:18180"}
profiles: [{id: home, hostname: test}]
routes: [{id: web, kind: http, local_port: 18180, upstream: "http://service:80"}]
`,
			want: "with web_ui",
		},
		{
			name: "listen and local port",
			body: `
version: 1
profiles: [{id: home, hostname: test}]
routes: [{id: web, kind: http, listen: "127.0.0.1:18080", local_port: 18081, upstream: "http://service:80"}]
`,
			want: "either listen or local_port",
		},
		{
			name: "invalid advertise tag",
			body: `
version: 1
profiles: [{id: home, hostname: test, advertise_tags: [not-a-tag]}]
`,
			want: "invalid advertise tag",
		},
		{
			name: "invalid DERP region",
			body: `
version: 1
profiles: [{id: home, hostname: test, derp_check_region: "bad region"}]
`,
			want: "derp_check_region is invalid",
		},
		{
			name: "non-loopback",
			body: `
version: 1
profiles: [{id: home, hostname: test}]
routes: [{id: db, profile: home, kind: tcp, listen: "0.0.0.0:1234", upstream: "db:1234"}]
`,
			want: "not loopback",
		},
		{
			name: "duplicate listener",
			body: `
version: 1
profiles: [{id: home, hostname: test}]
routes:
  - {id: one, profile: home, kind: tcp, listen: "127.0.0.1:1234", upstream: "one:1"}
  - {id: two, profile: home, kind: tcp, listen: "127.0.0.1:1234", upstream: "two:2"}
`,
			want: "share listen address",
		},
		{
			name: "unknown profile",
			body: `
version: 1
profiles: [{id: home, hostname: test}]
routes: [{id: db, profile: missing, kind: tcp, listen: "127.0.0.1:1234", upstream: "db:1234"}]
`,
			want: "unknown profile",
		},
		{
			name: "credentials in URL",
			body: `
version: 1
profiles: [{id: home, hostname: test}]
routes: [{id: web, profile: home, kind: http, listen: "127.0.0.1:1234", upstream: "https://user:pass@example.com"}]
`,
			want: "must not contain credentials",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error=%v, want substring %q", err, tt.want)
			}
		})
	}
}
