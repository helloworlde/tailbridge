// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
	"github.com/helloworlde/tailbridge/internal/config"
	"github.com/helloworlde/tailbridge/internal/forward"
)

const statusFilename = "status.json"

type Status struct {
	Version   int                `json:"version"`
	PID       int                `json:"pid"`
	Alive     bool               `json:"alive"`
	State     string             `json:"state"`
	StartedAt time.Time          `json:"started_at"`
	UpdatedAt time.Time          `json:"updated_at"`
	WebUI     string             `json:"web_ui,omitempty"`
	Profiles  []profileSnapshot  `json:"profiles"`
	Routes    []forward.Snapshot `json:"routes"`
}

func statusPath(cfg *config.Config) string {
	return filepath.Join(cfg.RuntimeDir, statusFilename)
}

func writeStatus(path string, status Status) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".status-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(status); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func ReadStatus(cfg *config.Config) (*Status, error) {
	data, err := os.ReadFile(statusPath(cfg))
	if err != nil {
		return nil, err
	}
	var status Status
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, fmt.Errorf("decode status: %w", err)
	}
	status.Alive = processAlive(status.PID)
	if status.State == "running" && !status.Alive {
		status.State = "stale"
	}
	return &status, nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}
