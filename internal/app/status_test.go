// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/helloworlde/tailbridge/internal/config"
	"github.com/helloworlde/tailbridge/internal/forward"
)

func TestStatusJSONOmitsPrivateNetworkData(t *testing.T) {
	profile := &profileRuntime{
		cfg:     config.Profile{ID: "home", Hostname: "private-hostname"},
		state:   "running",
		ipCount: 2,
		network: networkSnapshot{
			State: "available", PeerCount: 4, PeerRelayPathCount: 1,
		},
	}
	status := Status{
		State:    "running",
		Profiles: []profileSnapshot{profile.snapshot()},
		Routes: []forward.Snapshot{{
			ID: "web",
		}},
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, secret := range []string{"private-hostname", "100.64.0.1", "fd7a:115c:a1e0::1", "private.internal.example"} {
		if strings.Contains(got, secret) {
			t.Fatalf("status JSON leaked %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, `"tailscale_ip_count":2`) {
		t.Fatalf("status JSON omitted anonymous IP count: %s", got)
	}
	if !strings.Contains(got, `"peer_relay_path_count":1`) {
		t.Fatalf("status JSON omitted peer relay count: %s", got)
	}
}
