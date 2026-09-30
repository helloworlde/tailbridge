// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package forward

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/helloworlde/tailbridge/internal/config"
	"github.com/helloworlde/tailbridge/internal/diagnostics"
)

type connPair struct {
	local  net.Conn
	remote net.Conn
}

func (p *connPair) close() {
	p.local.Close()
	p.remote.Close()
}

type TCP struct {
	cfg    config.Route
	dialer Dialer
	ln     net.Listener
	log    *slog.Logger

	accepting atomic.Bool
	active    atomic.Int64
	total     atomic.Uint64
	sent      atomic.Uint64
	received  atomic.Uint64
	lastError atomic.Value // string

	closeOnce    sync.Once
	serveStarted atomic.Bool
	serveDone    chan struct{}
	wg           sync.WaitGroup
	mu           sync.Mutex
	conns        map[*connPair]struct{}
}

func NewTCP(cfg config.Route, dialer Dialer, ln net.Listener, logger *slog.Logger) *TCP {
	t := &TCP{cfg: cfg, dialer: dialer, ln: ln, log: logger, serveDone: make(chan struct{}), conns: make(map[*connPair]struct{})}
	t.accepting.Store(true)
	t.lastError.Store("")
	return t
}

func (t *TCP) Serve() error {
	if !t.serveStarted.CompareAndSwap(false, true) {
		return errors.New("TCP route Serve called more than once")
	}
	defer close(t.serveDone)
	for {
		conn, err := t.ln.Accept()
		if err != nil {
			if !t.accepting.Load() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			t.setError(err)
			return err
		}
		t.wg.Add(1)
		go t.handle(conn)
	}
}

func (t *TCP) handle(local net.Conn) {
	defer t.wg.Done()
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), t.cfg.ConnectTimeout.Duration)
	remote, err := t.dialer.Dial(ctx, "tcp", t.cfg.Upstream)
	cancel()
	if err != nil {
		t.setError(err)
		t.log.Warn("TCP upstream dial failed", "route", t.cfg.ID, "profile", t.cfg.Profile, "error_class", diagnostics.Classify(err))
		local.Close()
		return
	}

	pair := &connPair{local: local, remote: remote}
	t.mu.Lock()
	t.conns[pair] = struct{}{}
	t.mu.Unlock()
	t.active.Add(1)
	t.total.Add(1)
	defer func() {
		pair.close()
		t.mu.Lock()
		delete(t.conns, pair)
		t.mu.Unlock()
		t.active.Add(-1)
		t.log.Info("TCP connection closed", "route", t.cfg.ID, "profile", t.cfg.Profile, "duration", time.Since(started).String())
	}()

	stopIdle := make(chan struct{})
	activity := make(chan struct{}, 1)
	if t.cfg.IdleTimeout.Duration > 0 {
		go watchIdle(pair, t.cfg.IdleTimeout.Duration, activity, stopIdle)
	}

	type result struct {
		n   int64
		err error
	}
	sentResult := make(chan result, 1)
	receivedResult := make(chan result, 1)
	go func() {
		n, err := copyStream(remote, local, activity)
		closeWrite(remote)
		closeRead(local)
		sentResult <- result{n, err}
	}()
	go func() {
		n, err := copyStream(local, remote, activity)
		closeWrite(local)
		closeRead(remote)
		receivedResult <- result{n, err}
	}()
	sent := <-sentResult
	if firstCopyError(sent.err) != nil {
		pair.close()
	}
	received := <-receivedResult
	if t.cfg.IdleTimeout.Duration > 0 {
		close(stopIdle)
	}
	t.sent.Add(uint64(sent.n))
	t.received.Add(uint64(received.n))
	if err := firstCopyError(sent.err, received.err); err != nil {
		t.setError(err)
		t.log.Warn("TCP forwarding ended with error", "route", t.cfg.ID, "profile", t.cfg.Profile, "error_class", diagnostics.Classify(err))
	}
}

func firstCopyError(errs ...error) error {
	for _, err := range errs {
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
			return err
		}
	}
	return nil
}

func copyStream(dst io.Writer, src io.Reader, activity chan<- struct{}) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		nr, readErr := src.Read(buf)
		if nr > 0 {
			nw, writeErr := dst.Write(buf[:nr])
			total += int64(nw)
			select {
			case activity <- struct{}{}:
			default:
			}
			if writeErr != nil {
				return total, writeErr
			}
			if nw != nr {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func watchIdle(pair *connPair, timeout time.Duration, activity <-chan struct{}, stop <-chan struct{}) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)
		case <-timer.C:
			pair.close()
			return
		case <-stop:
			return
		}
	}
}

func closeWrite(conn net.Conn) {
	if c, ok := conn.(interface{ CloseWrite() error }); ok {
		c.CloseWrite()
	}
}

func closeRead(conn net.Conn) {
	if c, ok := conn.(interface{ CloseRead() error }); ok {
		c.CloseRead()
	}
}

func (t *TCP) Shutdown(ctx context.Context) error {
	t.closeOnce.Do(func() {
		t.accepting.Store(false)
		t.ln.Close()
	})
	// A prepared route can be shut down before Serve is launched. Otherwise,
	// wait for the accept loop to stop adding connection goroutines before
	// waiting on their WaitGroup.
	if t.serveStarted.Load() {
		<-t.serveDone
	}
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		t.mu.Lock()
		for pair := range t.conns {
			pair.close()
		}
		t.mu.Unlock()
		<-done
		return ctx.Err()
	}
}

func (t *TCP) setError(err error) {
	t.lastError.Store(diagnostics.Classify(err))
}

func (t *TCP) Snapshot() Snapshot {
	return Snapshot{
		ID: t.cfg.ID, Kind: t.cfg.Kind, Profile: t.cfg.Profile,
		Listen:    t.ln.Addr().String(),
		Accepting: t.accepting.Load(), Active: t.active.Load(), Connections: t.total.Load(),
		SentBytes: t.sent.Load(), RecvBytes: t.received.Load(), LastError: t.lastError.Load().(string),
	}
}
