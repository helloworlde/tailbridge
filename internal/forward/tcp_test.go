// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package forward

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/helloworlde/tailbridge/internal/config"
)

type directDialer struct{ d net.Dialer }

func (d *directDialer) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	return d.d.DialContext(ctx, network, address)
}

func TestTCPForwardsAndPreservesHalfClose(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		body, _ := io.ReadAll(conn)
		conn.Write(append([]byte("reply:"), body...))
	}()

	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Route{
		ID: "echo", Profile: "test", Kind: "tcp", Listen: local.Addr().String(), Upstream: upstream.Addr().String(),
		ConnectTimeout: config.Duration{Duration: time.Second},
	}
	route := NewTCP(cfg, &directDialer{}, local, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan error, 1)
	go func() { done <- route.Serve() }()

	conn, err := net.Dial("tcp", local.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tcpConn := conn.(*net.TCPConn)
	if _, err := tcpConn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := tcpConn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(tcpConn)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "reply:hello" {
		t.Fatalf("response=%q", got)
	}
	tcpConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := route.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	snapshot := route.Snapshot()
	if snapshot.Connections != 1 || snapshot.SentBytes != 5 || snapshot.RecvBytes != uint64(len("reply:hello")) {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
}
