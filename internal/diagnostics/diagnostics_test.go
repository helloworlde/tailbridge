// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package diagnostics

import (
	"errors"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	in := "TS_AUTHKEY=tskey-auth-abc123\nAuthorization: Bearer secret-value\nCookie=sessionvalue"
	got := Redact(in)
	for _, secret := range []string{"tskey-auth-abc123", "Bearer", "secret-value", "sessionvalue"} {
		if strings.Contains(got, secret) {
			t.Fatalf("Redact(%q) leaked %q: %q", in, secret, got)
		}
	}
}

func TestClassify(t *testing.T) {
	if got := Classify(errors.New("dial tcp: connection refused")); got != "target_refused" {
		t.Fatalf("got %q", got)
	}
	if got := Classify(errors.New("x509: certificate signed by unknown authority")); got != "tls_failed" {
		t.Fatalf("got %q", got)
	}
}
