// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package forward

import (
	"context"
	"errors"
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

type dialFunc func(context.Context, string, string) (net.Conn, error)

func (f dialFunc) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

func TestTCPShutdownCancelsPendingDial(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	dialer := dialFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(entered)
		select {
		case <-ctx.Done():
			close(cancelled)
			return nil, ctx.Err()
		case <-release:
			return nil, errors.New("test cleanup")
		}
	})
	route := NewTCP(config.Route{ID: "pending", Upstream: "unused:22", ConnectTimeout: config.Duration{Duration: 5 * time.Second}}, dialer, listener, slog.New(slog.NewTextHandler(io.Discard, nil)))
	serveDone := make(chan error, 1)
	go func() { serveDone <- route.Serve() }()
	client, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("dial did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- route.Shutdown(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown error=%v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("shutdown exceeded drain deadline")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("pending dial was not cancelled")
	}
	client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("local connection remained open")
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
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
