// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// TailBridge exposes selected Tailnet services on macOS loopback listeners
// without creating a system VPN or changing the host routing table.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"github.com/helloworlde/tailbridge/internal/app"
	"github.com/helloworlde/tailbridge/internal/config"
)

var buildVersion = "dev"

const initialConfig = `version: 1
startup_timeout: 5m
drain_timeout: 15s

web_ui:
  listen: "127.0.0.1:18180"

profiles:
  - id: home
    hostname: tailbridge-macos
    ephemeral: false
    socks5: false

routes: []
`

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "tailbridge: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errors.New("missing command")
	}
	switch args[0] {
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "start":
		return runStart(args[1:], stdout, stderr)
	case "check":
		return runCheck(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "tailbridge %s\ntailscale.com %s\n", tailbridgeVersion(), tailscaleDependencyVersion())
		return nil
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runInit(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath(), "YAML configuration path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("init accepts no positional arguments")
	}
	if err := writeInitialConfig(*configPath); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "created configuration: %s\n", *configPath)
	fmt.Fprintln(stdout, "next: edit routes, then run `tailbridge check`")
	return nil
}

func writeInitialConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("configuration already exists: %s", path)
		}
		return fmt.Errorf("create configuration: %w", err)
	}
	if _, err := io.WriteString(f, initialConfig); err != nil {
		f.Close()
		return fmt.Errorf("write configuration: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close configuration: %w", err)
	}
	return nil
}

func tailbridgeVersion() string {
	if buildVersion != "dev" {
		return buildVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func tailscaleDependencyVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, dep := range info.Deps {
		if dep.Path == "tailscale.com" {
			if dep.Replace != nil && dep.Replace.Version != "" {
				return dep.Replace.Version
			}
			if dep.Version != "" {
				return dep.Version
			}
		}
	}
	return "unknown"
}

func runStart(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath(), "YAML configuration path")
	debug := fs.Bool("debug", false, "enable privacy-safe TailBridge debug diagnostics")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("start accepts no positional arguments")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runtime := app.New(cfg, logger, stdout)
	return runtime.Run(ctx)
}

func runCheck(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath(), "YAML configuration path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("check accepts no positional arguments")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	fmt.Fprintf(stdout, "configuration valid: %d profile(s), %d route(s)\n", len(cfg.Profiles), len(cfg.Routes))
	if cfg.WebUI.Listen != "" {
		fmt.Fprintf(stdout, "web UI: http://%s\n", cfg.WebUI.Listen)
	}
	return nil
}

func runStatus(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath(), "YAML configuration path")
	jsonOutput := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("status accepts no positional arguments")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	status, err := app.ReadStatus(cfg)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("no status file; TailBridge has not been started with this configuration")
		}
		return err
	}
	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}
	fmt.Fprintf(stdout, "state: %s (pid %d, alive %t)\n", status.State, status.PID, status.Alive)
	if status.WebUI != "" {
		fmt.Fprintf(stdout, "web UI: %s\n", status.WebUI)
	}
	for _, profile := range status.Profiles {
		fmt.Fprintf(stdout, "profile %s: %s", profile.ID, profile.State)
		if profile.IPCount != 0 {
			fmt.Fprintf(stdout, " ip_count=%d", profile.IPCount)
		}
		if profile.SOCKS5Addr != "" {
			fmt.Fprintf(stdout, " socks5=%s", profile.SOCKS5Addr)
		}
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "  network: %s peers=%d online=%d active=%d paths(direct=%d peer-relay=%d derp=%d) subnet_routers=%d subnet_routes=%d health=%d magic_dns=%t",
			profile.Network.State, profile.Network.PeerCount, profile.Network.OnlinePeerCount, profile.Network.ActivePeerCount,
			profile.Network.DirectPathCount, profile.Network.PeerRelayPathCount, profile.Network.DERPPathCount,
			profile.Network.PrimaryRouterCount, profile.Network.PrimaryRouteCount,
			profile.Network.HealthIssueCount, profile.Network.MagicDNSEnabled)
		if profile.Network.DERPCheckConfigured {
			fmt.Fprintf(stdout, " derp_check=%s", profile.Network.DERPCheckState)
		}
		fmt.Fprintln(stdout)
	}
	for _, route := range status.Routes {
		fmt.Fprintf(stdout, "route %s: %s active=%d total=%d\n", route.ID, route.Listen, route.Active, route.Connections)
	}
	return nil
}

func defaultConfigPath() string {
	path, err := config.DefaultPath()
	if err != nil {
		return "config.yaml"
	}
	return path
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `Usage: tailbridge <command> [options]

Commands:
  init     Create a safe minimal configuration beside the executable
  start    Connect profiles and serve configured loopback routes
  check    Strictly validate and normalize the YAML configuration
  status   Read the local runtime status file
  version  Print TailBridge and official Tailscale library versions

Run "tailbridge <command> -h" for command options.`)
}
