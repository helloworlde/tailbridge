// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/helloworlde/tailbridge/internal/config"
	"github.com/helloworlde/tailbridge/internal/diagnostics"
	"golang.org/x/sys/unix"
	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// tailnetServer is the boundary to the embedded Tailscale SDK.
type tailnetServer interface {
	Up(context.Context) (*ipnstate.Status, error)
	Close() error
	Dial(context.Context, string, string) (net.Conn, error)
	LocalClient() (*local.Client, error)
	Loopback() (string, string, string, error)
}

type profileRuntime struct {
	cfg       config.Profile
	server    tailnetServer
	newServer func(*tsnet.Server) tailnetServer
	lock      *os.File
	log       *slog.Logger

	mu        sync.RWMutex
	state     string
	ipCount   int
	socksAddr string
	lastError string

	diagnosticsCancel context.CancelFunc
	diagnosticsWG     sync.WaitGroup
	network           networkSnapshot
	lastDERPCheck     time.Time
}

type profileSnapshot struct {
	ID         string          `json:"id"`
	State      string          `json:"state"`
	IPCount    int             `json:"tailscale_ip_count,omitempty"`
	SOCKS5Addr string          `json:"socks5_address,omitempty"`
	LastError  string          `json:"last_error_class,omitempty"`
	Network    networkSnapshot `json:"network"`
}

// networkSnapshot contains counts and booleans only. Peer names, addresses,
// DERP region names, routes and health messages deliberately never leave tsnet.
type networkSnapshot struct {
	State               string `json:"state"`
	PeerCount           int    `json:"peer_count,omitempty"`
	OnlinePeerCount     int    `json:"online_peer_count,omitempty"`
	ActivePeerCount     int    `json:"active_peer_count,omitempty"`
	DirectPathCount     int    `json:"direct_path_count,omitempty"`
	PeerRelayPathCount  int    `json:"peer_relay_path_count,omitempty"`
	DERPPathCount       int    `json:"derp_path_count,omitempty"`
	PrimaryRouterCount  int    `json:"primary_subnet_router_count,omitempty"`
	PrimaryRouteCount   int    `json:"primary_subnet_route_count,omitempty"`
	HealthIssueCount    int    `json:"health_issue_count,omitempty"`
	MagicDNSEnabled     bool   `json:"magic_dns_enabled"`
	DERPCheckConfigured bool   `json:"derp_check_configured,omitempty"`
	DERPCheckState      string `json:"derp_check_state,omitempty"`
}

type socksCredential struct {
	Profile  string `json:"profile"`
	Address  string `json:"address"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func newProfile(cfg config.Profile, logger *slog.Logger) *profileRuntime {
	return &profileRuntime{cfg: cfg, log: logger, state: "stopped", network: networkSnapshot{State: "pending"}}
}

func (p *profileRuntime) start(parent context.Context, timeout time.Duration, credentialOut io.Writer) error {
	p.setState("starting", "")
	if err := os.MkdirAll(p.cfg.StateDir, 0700); err != nil {
		p.setState("failed", diagnostics.Classify(err))
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(p.cfg.StateDir, 0700); err != nil {
		p.setState("failed", diagnostics.Classify(err))
		return fmt.Errorf("secure state directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(p.cfg.StateDir, ".tailbridge.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		p.setState("failed", diagnostics.Classify(err))
		return fmt.Errorf("open state lock: %w", err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		p.setState("failed", "state_in_use")
		return errors.New("state directory is already in use by another TailBridge process")
	}
	p.lock = lock

	userLogf := func(format string, args ...any) {
		message := diagnostics.Redact(fmt.Sprintf(format, args...))
		if state := stateFromUserMessage(message); state != "" {
			p.mu.Lock()
			p.state = state
			p.mu.Unlock()
			if state == "needs_login" {
				// The short-lived login URL is required for interactive enrollment.
				p.log.Info(message, "profile", p.cfg.ID, "component", "tsnet")
			}
		}
		if strings.Contains(message, "NeedsMachineAuth") {
			p.log.Warn("Tailscale device approval is required", "profile", p.cfg.ID, "action", "approve this device in the Tailscale Machines admin page")
		}
	}
	options := &tsnet.Server{
		Dir: p.cfg.StateDir, Hostname: p.cfg.Hostname, Ephemeral: p.cfg.Ephemeral,
		ControlURL: p.cfg.ControlURL, UserLogf: userLogf, Port: p.cfg.UDPPort,
		AdvertiseTags: append([]string(nil), p.cfg.AdvertiseTags...),
	}
	var server tailnetServer = options
	if p.newServer != nil {
		server = p.newServer(options)
	}
	p.mu.Lock()
	p.server = server
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, timeout)
	status, err := server.Up(ctx)
	cancel()
	if err != nil {
		class := diagnostics.Classify(err)
		p.mu.RLock()
		state := p.state
		p.mu.RUnlock()
		if state == "needs_login" {
			class = "auth_required"
		} else if state == "needs_machine_approval" {
			class = "machine_approval_required"
		}
		p.setState("failed", class)
		server.Close()
		p.mu.Lock()
		p.server = nil
		p.mu.Unlock()
		p.unlock()
		return fmt.Errorf("connect profile %q (%s): %w", p.cfg.ID, class, err)
	}
	p.mu.Lock()
	p.ipCount = len(status.TailscaleIPs)
	p.state = "running"
	p.lastError = ""
	p.mu.Unlock()
	if p.cfg.SOCKS5 {
		addr, proxyCred, _, err := server.Loopback()
		if err != nil {
			p.setState("degraded", diagnostics.Classify(err))
			return fmt.Errorf("start SOCKS5 for profile %q: %w", p.cfg.ID, err)
		}
		p.mu.Lock()
		p.socksAddr = addr
		p.mu.Unlock()
		credential := socksCredential{Profile: p.cfg.ID, Address: addr, Username: "tsnet", Password: proxyCred}
		encoded, _ := json.Marshal(credential)
		if _, err := fmt.Fprintf(credentialOut, "%s\n", encoded); err != nil {
			p.setState("degraded", "credential_output_failed")
			return fmt.Errorf("write SOCKS5 credentials for profile %q: %w", p.cfg.ID, err)
		}
	}
	diagnosticsCtx, diagnosticsCancel := context.WithCancel(parent)
	p.mu.Lock()
	p.diagnosticsCancel = diagnosticsCancel
	p.mu.Unlock()
	p.refreshNetwork(diagnosticsCtx)
	p.diagnosticsWG.Add(1)
	go p.diagnosticsLoop(diagnosticsCtx)
	return nil
}

func (p *profileRuntime) diagnosticsLoop(ctx context.Context) {
	defer p.diagnosticsWG.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.refreshNetwork(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (p *profileRuntime) refreshNetwork(parent context.Context) {
	p.mu.RLock()
	server := p.server
	p.mu.RUnlock()
	if server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	lc, err := server.LocalClient()
	if err != nil {
		p.setNetworkUnavailable()
		return
	}
	status, err := lc.Status(ctx)
	if err != nil {
		p.setNetworkUnavailable()
		return
	}
	network := summarizeNetwork(status)
	if p.cfg.DERPCheckRegion != "" {
		network.DERPCheckConfigured = true
		p.mu.RLock()
		lastCheck := p.lastDERPCheck
		network.DERPCheckState = p.network.DERPCheckState
		p.mu.RUnlock()
		checkDue := lastCheck.IsZero() || network.DERPCheckState == "unavailable" || time.Since(lastCheck) >= 5*time.Minute
		if checkDue {
			report, err := lc.DebugDERPRegion(ctx, p.cfg.DERPCheckRegion)
			switch {
			case err != nil || report == nil || len(report.Errors) != 0:
				network.DERPCheckState = "failed"
			case len(report.Warnings) != 0:
				network.DERPCheckState = "warning"
			default:
				network.DERPCheckState = "ok"
			}
			lastCheck = time.Now()
		}
		p.mu.Lock()
		p.lastDERPCheck = lastCheck
		p.mu.Unlock()
	}
	p.mu.Lock()
	p.network = network
	p.mu.Unlock()
}

func summarizeNetwork(status *ipnstate.Status) networkSnapshot {
	n := networkSnapshot{State: "available"}
	if status.CurrentTailnet != nil {
		n.MagicDNSEnabled = status.CurrentTailnet.MagicDNSEnabled
	}
	n.HealthIssueCount = len(status.Health)
	for _, peer := range status.Peer {
		n.PeerCount++
		if peer.Online {
			n.OnlinePeerCount++
		}
		if !peer.Active {
			if peer.PrimaryRoutes != nil && peer.PrimaryRoutes.Len() != 0 {
				n.PrimaryRouterCount++
				n.PrimaryRouteCount += peer.PrimaryRoutes.Len()
			}
			continue
		}
		n.ActivePeerCount++
		switch {
		case peer.CurAddr != "":
			n.DirectPathCount++
		case peer.PeerRelay != "":
			n.PeerRelayPathCount++
		case peer.Relay != "":
			n.DERPPathCount++
		}
		if peer.PrimaryRoutes != nil && peer.PrimaryRoutes.Len() != 0 {
			n.PrimaryRouterCount++
			n.PrimaryRouteCount += peer.PrimaryRoutes.Len()
		}
	}
	return n
}

func (p *profileRuntime) setNetworkUnavailable() {
	p.mu.Lock()
	p.network.State = "unavailable"
	p.network.DERPCheckConfigured = p.cfg.DERPCheckRegion != ""
	if p.network.DERPCheckConfigured {
		p.network.DERPCheckState = "unavailable"
	}
	p.mu.Unlock()
}

func (p *profileRuntime) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	p.mu.RLock()
	server := p.server
	p.mu.RUnlock()
	if server == nil {
		return nil, errors.New("profile is not running")
	}
	return server.Dial(ctx, network, address)
}

func (p *profileRuntime) close() error {
	p.mu.RLock()
	preserveFailure := p.state == "failed"
	p.mu.RUnlock()
	if !preserveFailure {
		p.setState("stopping", "")
	}
	p.mu.Lock()
	cancelDiagnostics := p.diagnosticsCancel
	p.diagnosticsCancel = nil
	p.mu.Unlock()
	if cancelDiagnostics != nil {
		cancelDiagnostics()
		p.diagnosticsWG.Wait()
	}
	p.mu.Lock()
	server := p.server
	p.server = nil
	p.mu.Unlock()
	var err error
	if server != nil {
		err = server.Close()
	}
	p.unlock()
	if !preserveFailure {
		p.setState("stopped", "")
	}
	return err
}

func stateFromUserMessage(message string) string {
	switch {
	case strings.Contains(message, "NeedsMachineAuth"):
		return "needs_machine_approval"
	case strings.Contains(message, "go to: http"):
		return "needs_login"
	default:
		return ""
	}
}

func (p *profileRuntime) unlock() {
	if p.lock != nil {
		unix.Flock(int(p.lock.Fd()), unix.LOCK_UN)
		p.lock.Close()
		p.lock = nil
	}
}

func (p *profileRuntime) setState(state, lastError string) {
	p.mu.Lock()
	p.state = state
	p.lastError = lastError
	p.mu.Unlock()
}

func (p *profileRuntime) snapshot() profileSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return profileSnapshot{
		ID: p.cfg.ID, State: p.state, IPCount: p.ipCount,
		SOCKS5Addr: p.socksAddr, LastError: p.lastError, Network: p.network,
	}
}
