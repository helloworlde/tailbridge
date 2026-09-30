// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package app

import "testing"

func TestStateFromUserMessage(t *testing.T) {
	tests := []struct {
		message string
		want    string
	}{
		{"To start this tsnet server, restart with TS_AUTHKEY set, or go to: https://login.tailscale.com/a/example", "needs_login"},
		{"AuthLoop: state is NeedsMachineAuth; done", "needs_machine_approval"},
		{"AuthLoop: state is Running; done", ""},
	}
	for _, tt := range tests {
		if got := stateFromUserMessage(tt.message); got != tt.want {
			t.Errorf("stateFromUserMessage(%q)=%q, want %q", tt.message, got, tt.want)
		}
	}
}
