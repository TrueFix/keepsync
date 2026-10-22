package keepsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

func (s *Service) snapshotDue(now time.Time) bool {
	if !s.cfg.Snapshots.Enabled {
		return false
	}
	var status Status
	if err := readJSON(s.statePath("status.json"), &status); err != nil {
		return true
	}
	return status.SnapshotFormat != "tar" || status.SnapshotError != "" || status.LastSnapshot.IsZero() || now.Sub(status.LastSnapshot) >= duration(s.cfg.Snapshots.Interval)
}

func (s *Service) snapshot(ctx context.Context) (result error) {
	if !s.cfg.Snapshots.Enabled {
		return errors.New("snapshots are disabled")
	}
	var status Status
	if err := readJSON(s.statePath("status.json"), &status); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	status.SnapshotAttempt = time.Now().UTC()
	status.SnapshotError = "snapshot interrupted or still running"
	if err := writeJSON(s.statePath("status.json"), status); err != nil {
		return err
	}
	defer func() {
		if result != nil {
			status.SnapshotError = result.Error()
		}
		result = errors.Join(result, writeJSON(s.statePath("status.json"), status))
	}()
	source, err := s.source(ctx)
	if err != nil {
		return err
	}
	reg, err := s.registration(ctx)
	if err != nil {
		return err
	}
	record, err := s.createTarSnapshot(ctx, source, reg)
	if err != nil {
		return err
	}
	status.LastSnapshot = time.Now().UTC()
	status.SnapshotID = record.ID
	status.SnapshotFormat = "tar"
	status.SnapshotError = ""
	if err := writeJSON(s.statePath("status.json"), status); err != nil {
		return err
	}
	s.log.Info("Snapshot saved", "format", "tar", "snapshot", record.ID, "bytes", record.Size)
	if err := s.retainTar(ctx, reg, record.ID); err != nil {
		return fmt.Errorf("archive saved but retention failed: %w", err)
	}
	return nil
}

func (s *Service) retain(ctx context.Context) error {
	reg, err := s.registration(ctx)
	if err != nil {
		return err
	}
	var status Status
	if err := readJSON(s.statePath("status.json"), &status); err != nil {
		return err
	}
	return s.retainTar(ctx, reg, status.SnapshotID)
}
