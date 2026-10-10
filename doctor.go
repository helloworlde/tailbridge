package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/helloworlde/tailbridge/internal/app"
	"github.com/helloworlde/tailbridge/internal/config"
	"github.com/helloworlde/tailbridge/internal/forward"
)

func runDoctor(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", defaultConfigPath(), "YAML configuration path")
	routeID := fs.String("route", "", "probe only this running route")
	jsonOutput := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("doctor accepts no positional arguments")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	endpoint, err := doctorEndpoint(cfg)
	if err != nil {
		return err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: forward.ProbeTimeout + time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(endpoint + "/api/v1/status")
	if err != nil {
		return errors.New("cannot reach TailBridge Web UI; start TailBridge before running doctor")
	}
	var status app.Status
	err = decodeDoctorResponse(resp, &status)
	if err != nil {
		return err
	}
	if status.Version != 1 || status.State != "running" || !status.Alive {
		return errors.New("TailBridge is not running; start it before probing routes")
	}
	routes := status.Routes
	if *routeID != "" {
		routes = nil
		for _, route := range status.Routes {
			if route.ID == *routeID {
				routes = append(routes, route)
				break
			}
		}
		if len(routes) == 0 {
			return fmt.Errorf("route %q is not configured in the running process", *routeID)
		}
	}
	report := struct {
		State  string                `json:"state"`
		Routes []forward.ProbeResult `json:"routes"`
	}{State: status.State, Routes: make([]forward.ProbeResult, 0, len(routes))}
	failed := false
	for _, route := range routes {
		body, _ := json.Marshal(struct {
			ID string `json:"id"`
		}{route.ID})
		resp, err := client.Post(endpoint+"/api/v1/routes/probe", "application/json", bytes.NewReader(body))
		result := forward.ProbeResult{ID: route.ID, Kind: route.Kind}
		if err == nil {
			err = decodeDoctorResponse(resp, &result)
		}
		if err != nil {
			result = forward.ProbeResult{ID: route.ID, Kind: route.Kind, ErrorClass: "diagnostics_unavailable"}
		}
		if !result.Reachable || result.HTTPStatus >= 500 {
			failed = true
		}
		report.Routes = append(report.Routes, result)
	}
	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(stdout, "state: running")
		for _, result := range report.Routes {
			state := "reachable"
			if !result.Reachable {
				state = result.ErrorClass
			}
			fmt.Fprintf(stdout, "route %s (%s): %s", result.ID, result.Kind, state)
			if result.HTTPStatus != 0 {
				fmt.Fprintf(stdout, " HTTP=%d", result.HTTPStatus)
			}
			fmt.Fprintf(stdout, " duration=%dms\n", result.DurationMS)
		}
		if len(report.Routes) == 0 {
			fmt.Fprintln(stdout, "no routes are configured in the running process")
		}
	}
	if failed {
		return errors.New("one or more route checks failed; see the reported error class or HTTP status")
	}
	return nil
}

func decodeDoctorResponse(resp *http.Response, target any) error {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("diagnostic API returned HTTP %d; check the running TailBridge version", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target); err != nil {
		return errors.New("invalid response from TailBridge diagnostic API")
	}
	return nil
}

func doctorEndpoint(cfg *config.Config) (string, error) {
	if cfg.WebUI.Listen == "" {
		return "", errors.New("doctor requires web_ui.listen; enable the loopback Web UI and restart TailBridge")
	}
	_, port, _ := net.SplitHostPort(cfg.WebUI.Listen)
	if port != "0" {
		return "http://" + cfg.WebUI.Listen, nil
	}
	status, err := app.ReadStatus(cfg)
	if err != nil || !status.Alive || status.State != "running" {
		return "", errors.New("cannot resolve the dynamic Web UI port; start TailBridge first")
	}
	u, err := url.Parse(status.WebUI)
	if err != nil || u.Scheme != "http" || u.User != nil {
		return "", errors.New("invalid Web UI address in runtime status")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !ip.IsLoopback() || strings.TrimSpace(u.Port()) == "" {
		return "", errors.New("Web UI address must be loopback")
	}
	return "http://" + u.Host, nil
}
