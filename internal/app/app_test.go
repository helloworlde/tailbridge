package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helloworlde/tailbridge/internal/config"
	"github.com/helloworlde/tailbridge/internal/forward"
	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// Only the external Tailscale SDK is substituted. Listeners, HTTP forwarding,
// diagnostics, lifecycle, locks and status files use the production code.
type testTailnetServer struct {
	closed  atomic.Bool
	upError error
}

func (s *testTailnetServer) Up(context.Context) (*ipnstate.Status, error) {
	return &ipnstate.Status{TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.99")}}, s.upError
}
func (s *testTailnetServer) Close() error { s.closed.Store(true); return nil }
func (s *testTailnetServer) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, address)
}
func (s *testTailnetServer) LocalClient() (*local.Client, error) {
	return nil, errors.New("diagnostics not available in test SDK")
}
func (s *testTailnetServer) Loopback() (string, string, string, error) {
	return "127.0.0.1:12345", "test-socks-secret", "test-api-secret", nil
}

func runtimeConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	return &config.Config{Version: 1, RuntimeDir: filepath.Join(dir, "run"), StartupTimeout: config.Duration{Duration: time.Second}, DrainTimeout: config.Duration{Duration: time.Second}, WebUI: config.WebUI{Listen: "127.0.0.1:0"}, Profiles: []config.Profile{{ID: "home", Hostname: "private-node", StateDir: filepath.Join(dir, "state")}}}
}

func awaitRunning(t *testing.T, runtime *Runtime) Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status := runtime.Snapshot()
		if status.State == "running" {
			return status
		}
		if status.State == "failed" {
			t.Fatal("runtime failed to start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("runtime did not start")
	return Status{}
}

func TestRuntimeForwardsProbesAndStops(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer upstream.Close()
	cfg := runtimeConfig(t)
	cfg.Routes = []config.Route{{ID: "web", Profile: "home", Kind: "http", Listen: "127.0.0.1:0", Upstream: upstream.URL, HostHeader: "upstream", ConnectTimeout: config.Duration{Duration: time.Second}}}
	runtime := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), io.Discard)
	server := &testTailnetServer{}
	runtime.newServer = func(*tsnet.Server) tailnetServer { return server }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	status := awaitRunning(t, runtime)
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + status.Routes[0].Listen)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("forwarding HTTP=%d", resp.StatusCode)
	}
	resp, err = client.Post(status.WebUI+"/api/v1/routes/probe", "application/json", strings.NewReader(`{"id":"web"}`))
	if err != nil {
		t.Fatal(err)
	}
	var probe forward.ProbeResult
	err = json.NewDecoder(resp.Body).Decode(&probe)
	resp.Body.Close()
	if err != nil || !probe.Reachable || probe.HTTPStatus != 204 {
		t.Fatalf("probe=%+v err=%v", probe, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	stored, err := ReadStatus(cfg)
	if err != nil || stored.State != "stopped" || !server.closed.Load() {
		t.Fatalf("stopped status=%+v err=%v", stored, err)
	}
	for _, address := range []string{status.Routes[0].Listen, strings.TrimPrefix(status.WebUI, "http://")} {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Errorf("listener remained open: %s", address)
		}
	}
}

func TestSecondRuntimeDoesNotOverwriteRunningStatus(t *testing.T) {
	cfg := runtimeConfig(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	first := New(cfg, logger, io.Discard)
	first.newServer = func(*tsnet.Server) tailnetServer { return &testTailnetServer{} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- first.Run(ctx) }()
	defer func() { cancel(); <-done }()
	awaitRunning(t, first)
	second := New(cfg, logger, io.Discard)
	second.newServer = first.newServer
	if err := second.Run(context.Background()); err == nil {
		t.Fatal("second runtime unexpectedly started")
	}
	stored, err := ReadStatus(cfg)
	if err != nil || stored.State != "running" {
		t.Fatalf("second runtime overwrote first runtime status: state=%s err=%v", stored.State, err)
	}
}

func TestFailedProfileStartupClosesPreviousProfilesAndReleasesLocks(t *testing.T) {
	cfg := runtimeConfig(t)
	cfg.Profiles = append(cfg.Profiles, config.Profile{ID: "second", StateDir: filepath.Join(t.TempDir(), "state")})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtime := New(cfg, logger, io.Discard)
	first := &testTailnetServer{}
	second := &testTailnetServer{upError: context.DeadlineExceeded}
	runtime.newServer = func(options *tsnet.Server) tailnetServer {
		if options.Dir == cfg.Profiles[0].StateDir {
			return first
		}
		return second
	}
	if err := runtime.Run(context.Background()); err == nil {
		t.Fatal("startup should fail")
	}
	stored, err := ReadStatus(cfg)
	if err != nil || stored.State != "failed" || !first.closed.Load() || !second.closed.Load() {
		t.Fatalf("failed startup did not clean up: state=%s err=%v", stored.State, err)
	}
	retry := New(cfg, logger, io.Discard)
	retry.newServer = func(*tsnet.Server) tailnetServer { return &testTailnetServer{} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- retry.Run(ctx) }()
	defer func() { cancel(); <-done }()
	awaitRunning(t, retry)
}
