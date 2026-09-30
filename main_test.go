// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helloworlde/tailbridge/internal/config"
)

func TestWriteInitialConfigIsValidPrivateAndNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	if err := writeInitialConfig(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("config mode=%#o, want 0600", got)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatalf("generated config is invalid: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeInitialConfig(path); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second init error=%v, want already exists", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("second init modified the existing configuration")
	}
}

func TestRunVersionReportsDependency(t *testing.T) {
	var stdout, stderr strings.Builder
	if err := run([]string{"version"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	got := stdout.String()
	if !strings.Contains(got, "tailbridge ") || !strings.Contains(got, "tailscale.com v1.102.5") {
		t.Fatalf("version output=%q", got)
	}
}
