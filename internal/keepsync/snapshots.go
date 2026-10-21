package keepsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

func snapshotFormat(status Status) string {
	if status.SnapshotFormat == "" {
		return "restic"
	}
	return status.SnapshotFormat
}

func (s *Service) snapshotDue(now time.Time) bool {
	if !s.cfg.Snapshots.Enabled {
		return false
	}
	var status Status
	if err := readJSON(s.statePath("status.json"), &status); err != nil {
		return true
	}
	return snapshotFormat(status) != s.cfg.Snapshots.Format || status.SnapshotError != "" || status.LastSnapshot.IsZero() || now.Sub(status.LastSnapshot) >= duration(s.cfg.Snapshots.Interval)
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
	if err := s.ensureSnapshotRepository(ctx); err != nil {
		return err
	}
	reg, err := s.snapshotRegistration(ctx)
	if err != nil {
		return err
	}
	if s.cfg.Snapshots.Format == "tar" {
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
	// Preserve the source identity in encrypted history so an explicit recovery
	// can resume against the same Live target after replacing a lost disk.
	pendingTag := s.tag() + ":pending"
	args := []string{"backup", "--json", "--read-concurrency", "1", "--tag", pendingTag, "--tag", "keepsync-source:" + reg.SourceID, "--host", s.cfg.Dataset}
	for _, exclude := range s.cfg.Excludes {
		args = append(args, "--exclude", exclude)
	}
	args = append(args, "--", source)
	s.log.Info("Snapshot started")
	var out snapshotOutput
	if err := s.restic(ctx, &out, args...); err != nil {
		return err
	}
	if out.err != nil {
		return out.err
	}
	summary := out.summary
	if summary.SnapshotID == "" {
		return errors.New("Restic completed without a confirmed snapshot ID")
	}
	// Restic can save a partial snapshot and still exit with an error. Publish
	// into the retained history only after a successful backup, so failed or
	// interrupted attempts cannot displace healthy recovery points.
	var promoted snapshotOutput
	if err := s.restic(ctx, &promoted, "tag", "--json", "--set", s.tag()+",keepsync-source:"+reg.SourceID, summary.SnapshotID); err != nil {
		return err
	}
	if promoted.err != nil {
		return promoted.err
	}
	if promoted.changed.OldSnapshotID != summary.SnapshotID || promoted.changed.NewSnapshotID == "" {
		return errors.New("snapshot publication was not confirmed")
	}
	summary.SnapshotID = promoted.changed.NewSnapshotID
	status.LastSnapshot = time.Now().UTC()
	status.SnapshotID = summary.SnapshotID
	status.SnapshotFormat = "restic"
	status.SnapshotError = ""
	if err := writeJSON(s.statePath("status.json"), status); err != nil {
		return err
	}
	s.log.Info("Snapshot saved", "snapshot", summary.SnapshotID, "new_data_bytes", summary.DataAdded)
	if err := s.retain(ctx); err != nil {
		return fmt.Errorf("snapshot saved but retention failed: %w", err)
	}
	return nil
}

func (s *Service) retain(ctx context.Context) error {
	if s.cfg.Snapshots.Format == "tar" {
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
	if err := s.restic(ctx, io.Discard, "forget", "--prune", "--tag", s.tag(), "--group-by", "", "--keep-last", fmt.Sprint(s.cfg.Snapshots.KeepLast)); err != nil {
		return err
	}
	s.log.Info("Snapshot retention complete", "keep_last", s.cfg.Snapshots.KeepLast)
	return nil
}
