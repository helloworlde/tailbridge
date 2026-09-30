// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package forward

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/helloworlde/tailbridge/internal/config"
)

func TestHTTPForwardsHostAndPath(t *testing.T) {
	seen := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seen <- req.Clone(req.Context())
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Route{
		ID: "web", Profile: "test", Kind: "http", Listen: local.Addr().String(), Upstream: upstream.URL + "/base",
		HostHeader: "upstream", ConnectTimeout: config.Duration{Duration: time.Second},
	}
	route, err := NewHTTP(cfg, &directDialer{}, local, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- route.Serve() }()

	resp, err := http.Get("http://" + local.Addr().String() + "/child")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != "ok" {
		t.Fatalf("body=%q err=%v", body, err)
	}
	req := <-seen
	if req.URL.Path != "/base/child" {
		t.Fatalf("path=%q", req.URL.Path)
	}
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	if req.Host != upstreamURL.Host {
		t.Fatalf("Host=%q, want %q", req.Host, upstreamURL.Host)
	}
	if req.Header.Get("X-Forwarded-Proto") != "http" {
		t.Fatalf("X-Forwarded-Proto=%q", req.Header.Get("X-Forwarded-Proto"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := route.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestHTTPUpgradeRemainsWritable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.EqualFold(req.Header.Get("Connection"), "upgrade") || req.Header.Get("Upgrade") != "test" {
			http.Error(w, "upgrade required", http.StatusBadRequest)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream response writer cannot hijack")
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
		rw.Flush()
		payload := make([]byte, 4)
		if _, err := io.ReadFull(rw, payload); err != nil {
			t.Error(err)
			return
		}
		if _, err := rw.Write(payload); err != nil {
			t.Error(err)
			return
		}
		rw.Flush()
	}))
	defer upstream.Close()

	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Route{
		ID: "upgrade", Profile: "test", Kind: "http", Listen: local.Addr().String(), Upstream: upstream.URL,
		HostHeader: "upstream", ConnectTimeout: config.Duration{Duration: time.Second},
	}
	route, err := NewHTTP(cfg, &directDialer{}, local, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- route.Serve() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := route.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	conn, err := net.Dial("tcp", local.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: local\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status=%d, want 101", resp.StatusCode)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo=%q, want ping", got)
	}
}
