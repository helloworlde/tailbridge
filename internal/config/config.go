// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package config loads and validates TailBridge configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"tailscale.com/tailcfg"
)

const CurrentVersion = 1

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Duration is a YAML duration such as "10s" or "2m".
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return errors.New("duration must be a string")
	}
	v, err := time.ParseDuration(node.Value)
	if err != nil {
		return err
	}
	if v < 0 {
		return errors.New("duration must not be negative")
	}
	d.Duration = v
	return nil
}

type Config struct {
	Version        int       `yaml:"version"`
	StartupTimeout Duration  `yaml:"startup_timeout,omitempty"`
	DrainTimeout   Duration  `yaml:"drain_timeout,omitempty"`
	RuntimeDir     string    `yaml:"runtime_dir,omitempty"`
	WebUI          WebUI     `yaml:"web_ui,omitempty"`
	Profiles       []Profile `yaml:"profiles"`
	Routes         []Route   `yaml:"routes,omitempty"`
}

// WebUI configures the optional embedded, read-only local dashboard.
// An empty listen address disables it.
type WebUI struct {
	Listen string `yaml:"listen,omitempty"`
}

type Profile struct {
	ID              string   `yaml:"id"`
	Hostname        string   `yaml:"hostname"`
	StateDir        string   `yaml:"state_dir,omitempty"`
	Ephemeral       bool     `yaml:"ephemeral,omitempty"`
	ControlURL      string   `yaml:"control_url,omitempty"`
	SOCKS5          bool     `yaml:"socks5,omitempty"`
	UDPPort         uint16   `yaml:"udp_port,omitempty"`
	AdvertiseTags   []string `yaml:"advertise_tags,omitempty"`
	DERPCheckRegion string   `yaml:"derp_check_region,omitempty"`
}

type Route struct {
	ID               string   `yaml:"id"`
	Profile          string   `yaml:"profile"`
	Kind             string   `yaml:"kind"`
	Listen           string   `yaml:"listen"`
	LocalPort        uint16   `yaml:"local_port,omitempty"`
	Upstream         string   `yaml:"upstream"`
	AllowNonLoopback bool     `yaml:"allow_non_loopback,omitempty"`
	ConnectTimeout   Duration `yaml:"connect_timeout,omitempty"`
	IdleTimeout      Duration `yaml:"idle_timeout,omitempty"`
	RequestTimeout   Duration `yaml:"request_timeout,omitempty"`
	HostHeader       string   `yaml:"host_header,omitempty"`
	TLSServerName    string   `yaml:"tls_server_name,omitempty"`
	CAFile           string   `yaml:"ca_file,omitempty"`
}

// RouteInput is the intentionally small route interface exposed by the Web UI.
// Advanced TLS and timeout settings remain YAML-only.
type RouteInput struct {
	ID        string `json:"id"`
	Profile   string `json:"profile"`
	Kind      string `json:"kind"`
	LocalPort uint16 `json:"local_port"`
	Upstream  string `json:"upstream"`
}

// DefaultPath returns config.yaml next to the running executable. Keeping the
// user-maintained configuration beside TailBridge makes the tool relocatable;
// node state remains in Application Support unless explicitly overridden.
func DefaultPath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return defaultPathForExecutable(executable), nil
}

func defaultPathForExecutable(executable string) string {
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return filepath.Join(filepath.Dir(executable), "config.yaml")
}

// Load parses path strictly, applies defaults, normalizes paths, and validates
// the complete configuration before returning it.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode YAML: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("configuration must contain exactly one YAML document")
		}
		return nil, fmt.Errorf("decode trailing YAML: %w", err)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	if err := cfg.normalize(filepath.Dir(absPath)); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) normalize(configDir string) error {
	base, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("resolve user config directory: %w", err)
	}
	appDir := filepath.Join(base, "TailBridge")
	if c.StartupTimeout.Duration == 0 {
		c.StartupTimeout.Duration = 2 * time.Minute
	}
	if c.DrainTimeout.Duration == 0 {
		c.DrainTimeout.Duration = 15 * time.Second
	}
	if c.RuntimeDir == "" {
		c.RuntimeDir = filepath.Join(appDir, "run")
	}
	if c.RuntimeDir, err = absolutePath(configDir, c.RuntimeDir); err != nil {
		return fmt.Errorf("runtime_dir: %w", err)
	}
	if c.WebUI.Listen != "" {
		if err := validateListen(c.WebUI.Listen, false); err != nil {
			return fmt.Errorf("web_ui: %w", err)
		}
	}
	for i := range c.Profiles {
		p := &c.Profiles[i]
		if p.StateDir == "" {
			p.StateDir = filepath.Join(appDir, "profiles", p.ID, "tsnet")
		}
		if p.StateDir, err = absolutePath(configDir, p.StateDir); err != nil {
			return fmt.Errorf("profile %q state_dir: %w", p.ID, err)
		}
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if err := c.normalizeRoute(configDir, r); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) normalizeRoute(configDir string, r *Route) error {
	r.Kind = strings.ToLower(r.Kind)
	if r.Kind == "" {
		u, err := url.Parse(r.Upstream)
		if err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
			r.Kind = "http"
		} else {
			r.Kind = "tcp"
		}
	}
	if r.Profile == "" && len(c.Profiles) == 1 {
		r.Profile = c.Profiles[0].ID
	}
	if r.Listen != "" && r.LocalPort != 0 {
		return fmt.Errorf("route %q: use either listen or local_port, not both", r.ID)
	}
	if r.Listen == "" && r.LocalPort != 0 {
		r.Listen = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(r.LocalPort)))
		r.LocalPort = 0
	}
	if r.ConnectTimeout.Duration == 0 {
		r.ConnectTimeout.Duration = 10 * time.Second
	}
	if r.HostHeader == "" {
		r.HostHeader = "upstream"
	}
	if r.CAFile != "" {
		var err error
		if r.CAFile, err = absolutePath(configDir, r.CAFile); err != nil {
			return fmt.Errorf("route %q ca_file: %w", r.ID, err)
		}
	}
	return nil
}

// ValidateRouteInput applies the same defaults and validation as a YAML route
// without mutating the active configuration.
func (c *Config) ValidateRouteInput(input RouteInput) (Route, error) {
	route := Route{
		ID: input.ID, Profile: input.Profile, Kind: input.Kind,
		LocalPort: input.LocalPort, Upstream: input.Upstream,
	}
	if err := c.normalizeRoute(".", &route); err != nil {
		return Route{}, err
	}
	candidate := *c
	candidate.Routes = append(append([]Route(nil), c.Routes...), route)
	if err := candidate.Validate(); err != nil {
		return Route{}, err
	}
	return route, nil
}

func absolutePath(base, value string) (string, error) {
	if strings.HasPrefix(value, "~") {
		return "", errors.New("~ is not supported; use an absolute path")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	return filepath.Clean(value), nil
}

// Validate checks all cross references and security-sensitive defaults.
func (c *Config) Validate() error {
	if c.Version != CurrentVersion {
		return fmt.Errorf("version must be %d", CurrentVersion)
	}
	if len(c.Profiles) == 0 {
		return errors.New("at least one profile is required")
	}
	if c.StartupTimeout.Duration <= 0 {
		return errors.New("startup_timeout must be greater than zero")
	}
	if c.DrainTimeout.Duration <= 0 {
		return errors.New("drain_timeout must be greater than zero")
	}
	if !filepath.IsAbs(c.RuntimeDir) {
		return errors.New("runtime_dir must be absolute")
	}
	if c.WebUI.Listen != "" {
		if err := validateListen(c.WebUI.Listen, false); err != nil {
			return fmt.Errorf("web_ui: %w", err)
		}
	}

	profiles := make(map[string]Profile, len(c.Profiles))
	stateDirs := make(map[string]string, len(c.Profiles))
	for _, p := range c.Profiles {
		if !idPattern.MatchString(p.ID) {
			return fmt.Errorf("profile id %q is invalid", p.ID)
		}
		if _, ok := profiles[p.ID]; ok {
			return fmt.Errorf("duplicate profile id %q", p.ID)
		}
		if p.Hostname == "" {
			return fmt.Errorf("profile %q: hostname is required", p.ID)
		}
		if !filepath.IsAbs(p.StateDir) {
			return fmt.Errorf("profile %q: state_dir must be absolute", p.ID)
		}
		if old, ok := stateDirs[p.StateDir]; ok {
			return fmt.Errorf("profiles %q and %q share state_dir %q", old, p.ID, p.StateDir)
		}
		stateDirs[p.StateDir] = p.ID
		if p.ControlURL != "" {
			u, err := url.Parse(p.ControlURL)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("profile %q: control_url must be an http(s) URL", p.ID)
			}
		}
		for _, tag := range p.AdvertiseTags {
			if err := tailcfg.CheckTag(tag); err != nil {
				return fmt.Errorf("profile %q: invalid advertise tag %q: %w", p.ID, tag, err)
			}
		}
		if p.DERPCheckRegion != "" && !idPattern.MatchString(p.DERPCheckRegion) {
			return fmt.Errorf("profile %q: derp_check_region is invalid", p.ID)
		}
		profiles[p.ID] = p
	}

	routeIDs := make(map[string]bool, len(c.Routes))
	listeners := make(map[string]string, len(c.Routes))
	if c.WebUI.Listen != "" {
		listeners[c.WebUI.Listen] = "web_ui"
	}
	for _, r := range c.Routes {
		if !idPattern.MatchString(r.ID) {
			return fmt.Errorf("route id %q is invalid", r.ID)
		}
		if routeIDs[r.ID] {
			return fmt.Errorf("duplicate route id %q", r.ID)
		}
		routeIDs[r.ID] = true
		if _, ok := profiles[r.Profile]; !ok {
			return fmt.Errorf("route %q references unknown profile %q", r.ID, r.Profile)
		}
		if r.Kind != "tcp" && r.Kind != "http" {
			return fmt.Errorf("route %q: kind must be tcp or http", r.ID)
		}
		if err := validateListen(r.Listen, r.AllowNonLoopback); err != nil {
			return fmt.Errorf("route %q: %w", r.ID, err)
		}
		if old, ok := listeners[r.Listen]; ok {
			if old == "web_ui" {
				return fmt.Errorf("route %q shares listen address %q with web_ui", r.ID, r.Listen)
			}
			return fmt.Errorf("routes %q and %q share listen address %q", old, r.ID, r.Listen)
		}
		listeners[r.Listen] = r.ID
		if r.ConnectTimeout.Duration <= 0 {
			return fmt.Errorf("route %q: connect_timeout must be greater than zero", r.ID)
		}
		switch r.Kind {
		case "tcp":
			if _, _, err := net.SplitHostPort(r.Upstream); err != nil {
				return fmt.Errorf("route %q: upstream must be host:port: %w", r.ID, err)
			}
			if r.CAFile != "" || r.TLSServerName != "" || r.RequestTimeout.Duration != 0 {
				return fmt.Errorf("route %q: HTTP/TLS fields are not valid for a TCP route", r.ID)
			}
		case "http":
			u, err := url.Parse(r.Upstream)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("route %q: upstream must be an http(s) URL", r.ID)
			}
			if u.User != nil || u.Fragment != "" {
				return fmt.Errorf("route %q: upstream must not contain credentials or a fragment", r.ID)
			}
			if r.HostHeader != "upstream" && r.HostHeader != "preserve" {
				return fmt.Errorf("route %q: host_header must be upstream or preserve", r.ID)
			}
			if u.Scheme != "https" && (r.CAFile != "" || r.TLSServerName != "") {
				return fmt.Errorf("route %q: tls_server_name and ca_file require an HTTPS upstream", r.ID)
			}
		}
	}
	return nil
}

func validateListen(value string, allowNonLoopback bool) error {
	host, _, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("listen must be IP:port: %w", err)
	}
	if host == "" {
		if allowNonLoopback {
			return nil
		}
		return errors.New("an empty listen host exposes all interfaces; set allow_non_loopback explicitly")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return errors.New("listen host must be an IP literal")
	}
	if !ip.IsLoopback() && !allowNonLoopback {
		return fmt.Errorf("listen address %s is not loopback; set allow_non_loopback explicitly", ip)
	}
	return nil
}
