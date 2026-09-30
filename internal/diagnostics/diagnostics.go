// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package diagnostics provides conservative error classification and log
// redaction. It deliberately avoids claiming ACL or routing causes that cannot
// be proven from a local dial error alone.
package diagnostics

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"regexp"
	"strings"
)

var (
	keyPattern    = regexp.MustCompile(`(?i)tskey-[a-z0-9?&=._-]+`)
	headerPattern = regexp.MustCompile(`(?i)(authorization|proxy-authorization|cookie|set-cookie)(\s*[:=]\s*)[^\r\n]+`)
)

// Redact removes credential-shaped values from a diagnostic string.
func Redact(s string) string {
	s = keyPattern.ReplaceAllString(s, "[REDACTED_KEY]")
	return headerPattern.ReplaceAllString(s, "$1$2[REDACTED]")
}

// Classify returns a stable, evidence-based error category.
func Classify(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "target_timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "magicdns_failed"
	}
	var tlsErr tls.RecordHeaderError
	if errors.As(err, &tlsErr) || strings.Contains(strings.ToLower(err.Error()), "certificate") || strings.Contains(strings.ToLower(err.Error()), "tls") {
		return "tls_failed"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "target_timeout"
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "connection refused") {
		return "target_refused"
	}
	if strings.Contains(msg, "address already in use") || strings.Contains(msg, "permission denied") {
		return "local_bind_failed"
	}
	return "unknown"
}
