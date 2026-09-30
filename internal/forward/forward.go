// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package forward implements local TCP and HTTP listeners whose outbound
// connections are supplied by an injected Tailnet dialer.
package forward

import (
	"context"
	"net"
)

type Dialer interface {
	Dial(context.Context, string, string) (net.Conn, error)
}

type Snapshot struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Profile     string `json:"profile"`
	Listen      string `json:"listen"`
	Accepting   bool   `json:"accepting"`
	Active      int64  `json:"active_connections"`
	Connections uint64 `json:"connections_total"`
	SentBytes   uint64 `json:"sent_bytes"`
	RecvBytes   uint64 `json:"received_bytes"`
	LastError   string `json:"last_error_class,omitempty"`
}

type Route interface {
	Serve() error
	Shutdown(context.Context) error
	Snapshot() Snapshot
}
