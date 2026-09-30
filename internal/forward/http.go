// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package forward

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"github.com/helloworlde/tailbridge/internal/config"
	"github.com/helloworlde/tailbridge/internal/diagnostics"
)

type HTTP struct {
	cfg       config.Route
	ln        net.Listener
	server    *http.Server
	transport *http.Transport
	log       *slog.Logger

	accepting atomic.Bool
	active    atomic.Int64
	total     atomic.Uint64
	sent      atomic.Uint64
	received  atomic.Uint64
	lastError atomic.Value // string
}

func NewHTTP(cfg config.Route, dialer Dialer, ln net.Listener, logger *slog.Logger) (*HTTP, error) {
	target, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout.Duration)
		defer cancel()
		return dialer.Dial(ctx, network, address)
	}
	transport.ForceAttemptHTTP2 = true
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.TLSServerName}
	if cfg.CAFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("ca_file contains no valid PEM certificates")
		}
		transport.TLSClientConfig.RootCAs = pool
	}

	h := &HTTP{cfg: cfg, ln: ln, transport: transport, log: logger}
	h.accepting.Store(true)
	h.lastError.Store("")
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalHost := req.Host
		originalDirector(req)
		if cfg.HostHeader == "upstream" {
			req.Host = target.Host
		} else {
			req.Host = originalHost
		}
		req.Header.Set("X-Forwarded-Proto", forwardedProto(req))
		if req.Body != nil {
			req.Body = &countingReadCloser{ReadCloser: req.Body, count: &h.sent}
		}
	}
	proxy.Transport = transport
	proxy.ModifyResponse = func(resp *http.Response) error {
		if body, ok := resp.Body.(io.ReadWriteCloser); ok {
			resp.Body = &countingReadWriteCloser{ReadWriteCloser: body, readCount: &h.received, writeCount: &h.sent}
		} else if resp.Body != nil {
			resp.Body = &countingReadCloser{ReadCloser: resp.Body, count: &h.received}
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		class := diagnostics.Classify(err)
		h.lastError.Store(class)
		h.log.Warn("HTTP upstream request failed", "route", cfg.ID, "profile", cfg.Profile, "error_class", class)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}
	var handler http.Handler = proxy
	if cfg.RequestTimeout.Duration > 0 {
		handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx, cancel := context.WithTimeout(req.Context(), cfg.RequestTimeout.Duration)
			defer cancel()
			proxy.ServeHTTP(w, req.WithContext(ctx))
		})
	}
	h.server = &http.Server{
		Handler:           h.instrument(handler),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       cfg.IdleTimeout.Duration,
	}
	return h, nil
}

func forwardedProto(req *http.Request) string {
	if req.TLS != nil {
		return "https"
	}
	return "http"
}

type countingReadCloser struct {
	io.ReadCloser
	count *atomic.Uint64
}

type countingReadWriteCloser struct {
	io.ReadWriteCloser
	readCount  *atomic.Uint64
	writeCount *atomic.Uint64
}

func (rw *countingReadWriteCloser) Read(p []byte) (int, error) {
	n, err := rw.ReadWriteCloser.Read(p)
	rw.readCount.Add(uint64(n))
	return n, err
}

func (rw *countingReadWriteCloser) Write(p []byte) (int, error) {
	n, err := rw.ReadWriteCloser.Write(p)
	rw.writeCount.Add(uint64(n))
	return n, err
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.count.Add(uint64(n))
	return n, err
}

func (h *HTTP) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h.active.Add(1)
		h.total.Add(1)
		defer h.active.Add(-1)
		next.ServeHTTP(w, req)
	})
}

func (h *HTTP) Serve() error {
	err := h.server.Serve(h.ln)
	if errors.Is(err, http.ErrServerClosed) || (!h.accepting.Load() && errors.Is(err, net.ErrClosed)) {
		return nil
	}
	if err != nil {
		h.lastError.Store(diagnostics.Classify(err))
	}
	return err
}

func (h *HTTP) Shutdown(ctx context.Context) error {
	h.accepting.Store(false)
	err := h.server.Shutdown(ctx)
	h.transport.CloseIdleConnections()
	return err
}

func (h *HTTP) Snapshot() Snapshot {
	return Snapshot{
		ID: h.cfg.ID, Kind: h.cfg.Kind, Profile: h.cfg.Profile,
		Listen:    h.ln.Addr().String(),
		Accepting: h.accepting.Load(), Active: h.active.Load(), Connections: h.total.Load(),
		SentBytes: h.sent.Load(), RecvBytes: h.received.Load(), LastError: h.lastError.Load().(string),
	}
}
