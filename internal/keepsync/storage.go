package keepsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const markerName = ".keepsync-source-id"

type Registration struct {
	Version      int    `json:"version"`
	Dataset      string `json:"dataset"`
	Source       string `json:"source"`
	SourceID     string `json:"source_id"`
	Repository   string `json:"repository"`
	RepositoryID string `json:"repository_id"`
	IdentityMode string `json:"identity_mode,omitempty"`
	SourceFileID string `json:"source_file_id,omitempty"`
}

type Status struct {
	LastAttempt     time.Time `json:"last_attempt,omitempty"`
	LastSuccess     time.Time `json:"last_success,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	SnapshotAttempt time.Time `json:"snapshot_attempt,omitempty"`
	LastSnapshot    time.Time `json:"last_snapshot,omitempty"`
	SnapshotID      string    `json:"snapshot_id,omitempty"`
	SnapshotFormat  string    `json:"snapshot_format,omitempty"`
	SnapshotError   string    `json:"snapshot_error,omitempty"`
}

func readJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".keepsync-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(b, '\n')); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Kernel locks are released automatically on exit, including an unclean restart.
// State must be on a local filesystem that supports flock, not an object mount.
func lockState(dir string) (func(), error) {
	return lockNamed(dir, "operation.lock")
}

func lockStateContext(ctx context.Context, dir string) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		unlock, err := lockState(dir)
		if err == nil {
			return unlock, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func lockNamed(dir, name string) (func(), error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another operation is using this state directory: %w", err)
	}
	return func() { _ = f.Close() }, nil
}
