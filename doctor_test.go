package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helloworlde/tailbridge/internal/app"
	"github.com/helloworlde/tailbridge/internal/config"
	"github.com/helloworlde/tailbridge/internal/forward"
	"github.com/helloworlde/tailbridge/internal/webui"
)

type doctorDialer struct{ net.Dialer }

func (d *doctorDialer) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	return d.DialContext(ctx, network, address)
}

func TestDoctorReportsHTTPServerErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer upstream.Close()
	cfg := &config.Config{Routes: []config.Route{{ID: "web", Kind: "http", Upstream: upstream.URL, HostHeader: "upstream", ConnectTimeout: config.Duration{Duration: time.Second}}}}
	handler, err := webui.New(webui.Options{
		Config: cfg,
		Status: func() any {
			return app.Status{Version: 1, State: "running", Alive: true, Routes: []forward.Snapshot{{ID: "web", Kind: "http"}, {ID: "other", Kind: "tcp"}}}
		},
		Probe: func(ctx context.Context, route config.Route) forward.ProbeResult {
			return forward.Probe(ctx, route, &doctorDialer{})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(handler)
	defer api.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := fmt.Sprintf("version: 1\nprofiles: [{id: home, hostname: test}]\nweb_ui: {listen: %q}\n", strings.TrimPrefix(api.URL, "http://"))
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	err = run([]string{"doctor", "-config", path, "-route", "web", "-json"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("doctor reported success for HTTP 503")
	}
	var report struct {
		Routes []forward.ProbeResult `json:"routes"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &report); err != nil {
		t.Fatalf("doctor did not produce JSON: %v, output=%q", err, stdout.String())
	}
	if len(report.Routes) != 1 || report.Routes[0].HTTPStatus != 503 || !report.Routes[0].Reachable {
		t.Fatalf("unexpected report: %+v", report)
	}
	if strings.Contains(stdout.String(), upstream.URL) {
		t.Fatal("doctor leaked upstream")
	}
}
