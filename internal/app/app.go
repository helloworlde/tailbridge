// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package app coordinates tsnet profiles and local forwarding routes.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/helloworlde/tailbridge/internal/config"
	"github.com/helloworlde/tailbridge/internal/diagnostics"
	"github.com/helloworlde/tailbridge/internal/forward"
	"github.com/helloworlde/tailbridge/internal/webui"
	"golang.org/x/sys/unix"
	"tailscale.com/tsnet"
)

type Runtime struct {
	cfg           *config.Config
	log           *slog.Logger
	credentialOut io.Writer
	startedAt     time.Time
	profiles      map[string]*profileRuntime
	mu            sync.RWMutex
	routes        []forward.Route
	webServer     *http.Server
	webListener   net.Listener
	state         atomic.Value // string
	newServer     func(*tsnet.Server) tailnetServer
}

func New(cfg *config.Config, logger *slog.Logger, credentialOut io.Writer) *Runtime {
	r := &Runtime{
		cfg: cfg, log: logger, credentialOut: credentialOut,
		profiles: make(map[string]*profileRuntime),
	}
	r.state.Store("starting")
	return r
}

// Run starts every profile before exposing any local listener, then waits for
// cancellation or an unexpected listener failure.
func (r *Runtime) Run(ctx context.Context) error {
	// Own the status directory before writing anything, including failure status.
	// A second process must not overwrite the live instance's status file.
	if err := os.MkdirAll(r.cfg.RuntimeDir, 0700); err != nil {
		return fmt.Errorf("create runtime directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(r.cfg.RuntimeDir, ".tailbridge.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open runtime lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("runtime directory is already in use by another TailBridge process")
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	r.mu.Lock()
	r.startedAt = time.Now()
	for _, profileCfg := range r.cfg.Profiles {
		profile := newProfile(profileCfg, r.log)
		profile.newServer = r.newServer
		r.profiles[profileCfg.ID] = profile
	}
	r.mu.Unlock()
	if err := r.writeStatus(); err != nil {
		r.log.Warn("cannot write startup status file", "error", diagnostics.Redact(err.Error()))
	}
	statusDone := make(chan struct{})
	statusFinished := make(chan struct{})
	go func() {
		defer close(statusFinished)
		r.statusLoop(statusDone)
	}()
	stopStatus := sync.OnceFunc(func() {
		close(statusDone)
		<-statusFinished
	})
	defer stopStatus()

	for _, profileCfg := range r.cfg.Profiles {
		profile := r.profiles[profileCfg.ID]
		r.log.Info("starting Tailnet profile", "profile", profileCfg.ID)
		if err := profile.start(ctx, r.cfg.StartupTimeout.Duration, r.credentialOut); err != nil {
			r.state.Store("failed")
			r.closeProfiles()
			stopStatus()
			if statusErr := r.writeStatus(); statusErr != nil {
				r.log.Warn("cannot write failed status", "error", diagnostics.Redact(statusErr.Error()))
			}
			return err
		}
		r.log.Info("Tailnet profile is running", "profile", profileCfg.ID, "tailscale_ip_count", profile.snapshot().IPCount)
	}

	if err := r.prepareRoutes(); err != nil {
		r.state.Store("failed")
		r.shutdownRoutes(context.Background())
		r.closeProfiles()
		stopStatus()
		if statusErr := r.writeStatus(); statusErr != nil {
			r.log.Warn("cannot write failed status", "error", diagnostics.Redact(statusErr.Error()))
		}
		return err
	}
	if err := r.prepareWebUI(); err != nil {
		r.state.Store("failed")
		r.shutdownRoutes(context.Background())
		r.closeProfiles()
		stopStatus()
		if statusErr := r.writeStatus(); statusErr != nil {
			r.log.Warn("cannot write failed status", "error", diagnostics.Redact(statusErr.Error()))
		}
		return err
	}

	errCh := make(chan error, len(r.routes)+1)
	for _, route := range r.routes {
		route := route
		r.log.Info("route is listening", "route", route.Snapshot().ID, "kind", route.Snapshot().Kind, "listen", route.Snapshot().Listen)
		go func() {
			if err := route.Serve(); err != nil {
				errCh <- fmt.Errorf("route %q: %w", route.Snapshot().ID, err)
			}
		}()
	}
	if r.webServer != nil {
		r.log.Info("Web UI is listening", "listen", r.webListener.Addr().String())
		go func() {
			if err := r.webServer.Serve(r.webListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("web UI: %w", err)
			}
		}()
	}
	r.state.Store("running")
	if err := r.writeStatus(); err != nil {
		r.log.Warn("cannot write status file", "error", diagnostics.Redact(err.Error()))
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
		r.log.Error("listener failed", "error", diagnostics.Redact(runErr.Error()))
	}
	r.state.Store("stopping")
	drainCtx, cancel := context.WithTimeout(context.Background(), r.cfg.DrainTimeout.Duration)
	r.shutdownWebUI(drainCtx)
	r.shutdownRoutes(drainCtx)
	cancel()
	r.closeProfiles()
	r.state.Store("stopped")
	stopStatus()
	if err := r.writeStatus(); err != nil {
		r.log.Warn("cannot write final status", "error", diagnostics.Redact(err.Error()))
	}
	return runErr
}

func (r *Runtime) prepareWebUI() error {
	if r.cfg.WebUI.Listen == "" {
		return nil
	}
	handler, err := webui.New(webui.Options{
		Config: r.cfg, Status: func() any { return r.Snapshot() },
		Probe: func(ctx context.Context, route config.Route) forward.ProbeResult {
			return forward.Probe(ctx, route, r.profiles[route.Profile])
		},
	})
	if err != nil {
		return fmt.Errorf("prepare Web UI: %w", err)
	}
	listener, err := net.Listen("tcp", r.cfg.WebUI.Listen)
	if err != nil {
		return fmt.Errorf("bind Web UI (%s): %w", diagnostics.Classify(err), err)
	}
	r.mu.Lock()
	r.webListener = listener
	r.webServer = webui.NewServer(listener.Addr().String(), handler)
	r.mu.Unlock()
	return nil
}

func (r *Runtime) shutdownWebUI(ctx context.Context) {
	if r.webServer == nil {
		return
	}
	if err := r.webServer.Shutdown(ctx); err != nil {
		r.webServer.Close()
		if ctx.Err() == nil {
			r.log.Warn("Web UI shutdown failed", "error", diagnostics.Redact(err.Error()))
		}
	}
}

func (r *Runtime) prepareRoutes() error {
	for _, routeCfg := range r.cfg.Routes {
		ln, err := net.Listen("tcp", routeCfg.Listen)
		if err != nil {
			return fmt.Errorf("bind route %q (%s): %w", routeCfg.ID, diagnostics.Classify(err), err)
		}
		profile := r.profiles[routeCfg.Profile]
		var route forward.Route
		switch routeCfg.Kind {
		case "tcp":
			route = forward.NewTCP(routeCfg, profile, ln, r.log)
		case "http":
			route, err = forward.NewHTTP(routeCfg, profile, ln, r.log)
			if err != nil {
				ln.Close()
				return fmt.Errorf("prepare HTTP route %q: %w", routeCfg.ID, err)
			}
		}
		if routeCfg.AllowNonLoopback {
			r.log.Warn("route explicitly listens outside loopback", "route", routeCfg.ID, "listen", routeCfg.Listen)
		}
		r.mu.Lock()
		r.routes = append(r.routes, route)
		r.mu.Unlock()
	}
	return nil
}

func (r *Runtime) shutdownRoutes(ctx context.Context) {
	var wg sync.WaitGroup
	for _, route := range r.routes {
		wg.Add(1)
		go func(route forward.Route) {
			defer wg.Done()
			if err := route.Shutdown(ctx); err != nil && ctx.Err() == nil {
				r.log.Warn("route shutdown failed", "route", route.Snapshot().ID, "error", diagnostics.Redact(err.Error()))
			}
		}(route)
	}
	wg.Wait()
}

func (r *Runtime) closeProfiles() {
	for i := len(r.cfg.Profiles) - 1; i >= 0; i-- {
		profile := r.profiles[r.cfg.Profiles[i].ID]
		if profile == nil {
			continue
		}
		if err := profile.close(); err != nil {
			r.log.Warn("profile shutdown failed", "profile", r.cfg.Profiles[i].ID, "error", diagnostics.Redact(err.Error()))
		}
	}
}

func (r *Runtime) statusLoop(done <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := r.writeStatus(); err != nil {
				r.log.Debug("cannot refresh status file", "error", diagnostics.Redact(err.Error()))
			}
		case <-done:
			return
		}
	}
}

func (r *Runtime) writeStatus() error {
	return writeStatus(statusPath(r.cfg), r.Snapshot())
}

// Snapshot is the single privacy-safe runtime view shared by the status file,
// CLI and embedded Web UI.
func (r *Runtime) Snapshot() Status {
	r.mu.RLock()
	status := Status{
		Version: 1, PID: os.Getpid(), Alive: true, State: r.state.Load().(string),
		StartedAt: r.startedAt, UpdatedAt: time.Now(),
	}
	webListener := r.webListener
	routes := append([]forward.Route(nil), r.routes...)
	profiles := make([]*profileRuntime, 0, len(r.cfg.Profiles))
	for _, profileCfg := range r.cfg.Profiles {
		if profile := r.profiles[profileCfg.ID]; profile != nil {
			profiles = append(profiles, profile)
		}
	}
	r.mu.RUnlock()
	if webListener != nil {
		status.WebUI = "http://" + webListener.Addr().String()
	}
	for _, profile := range profiles {
		status.Profiles = append(status.Profiles, profile.snapshot())
	}
	for _, route := range routes {
		status.Routes = append(status.Routes, route.Snapshot())
	}
	return status
}
