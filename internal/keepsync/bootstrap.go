package keepsync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// First-run identity belongs in the writable state volume, never in /data.
// Persist it before the S3 claim so retries after a crash use the same identity.
func (s *Service) ensureSetup(ctx context.Context) error {
	var existing Registration
	if err := readJSON(s.statePath("registration.json"), &existing); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read existing source state: %w", err)
	}
	source, err := filepath.EvalSymlinks(s.cfg.Source)
	if err != nil {
		return err
	}
	if err := s.safePaths(source); err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("source must be a directory")
	}
	_, fileID := fileMetadata(info)
	store, err := s.live(ctx)
	if err != nil {
		return err
	}
	var reg Registration
	pending := s.statePath("setup-pending.json")
	if err := readJSON(pending, &reg); err == nil {
		if reg.Version != 2 || reg.Dataset != s.cfg.Dataset || reg.Repository != s.cfg.Repository || reg.Source != source {
			return errors.New("pending setup belongs to a different source or target")
		}
		if reg.IdentityMode != "state" && reg.IdentityMode != "marker" {
			return errors.New("invalid pending identity mode")
		}
		if reg.IdentityMode == "state" && (reg.SourceFileID == "" || reg.SourceFileID != fileID) {
			return errors.New("source volume changed during setup")
		}
		if reg.IdentityMode == "marker" {
			if err := checkSourceMarker(source, reg.SourceID); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read pending source setup: %w", err)
	} else {
		reg = Registration{Version: 2, Dataset: s.cfg.Dataset, Source: source, Repository: s.cfg.Repository, IdentityMode: "state", SourceFileID: fileID}
		marker := filepath.Join(source, markerName)
		if _, err := os.Lstat(marker); err == nil {
			id, err := readSourceIDMarker(source)
			if err != nil {
				return err
			}
			reg.SourceID = id
			reg.IdentityMode = "marker"
			reg.SourceFileID = ""
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else {
			if fileID == "" {
				return errors.New("source filesystem does not provide a stable volume identity")
			}
			empty, err := store.Empty(ctx)
			if err != nil {
				return fmt.Errorf("check initial S3 target: %w", err)
			}
			if !empty {
				return errStateVolumeRequired
			}
			var id [32]byte
			if _, err := rand.Read(id[:]); err != nil {
				return err
			}
			reg.SourceID = hex.EncodeToString(id[:])
		}
		if !validSourceID(reg.SourceID) {
			return errors.New("invalid source identity")
		}
		if err := writeJSON(pending, reg); err != nil {
			return err
		}
	}
	if !validSourceID(reg.SourceID) {
		return errors.New("invalid pending source identity")
	}
	if err := store.Bind(ctx, reg.SourceID); err != nil {
		return fmt.Errorf("bind backup target: %w", err)
	}
	if err := writeJSON(s.statePath("registration.json"), reg); err != nil {
		return err
	}
	if err := os.Remove(pending); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.log.Debug("Source initialized automatically", "source", source, "identity", reg.IdentityMode, "dataset", reg.Dataset)
	return nil
}

// reclaim rebuilds local registration from an already-claimed Live target
// after the state volume was lost. Unlike ensureSetup, it trusts the mounted
// source without comparing a previously recorded file ID: running this
// command is an explicit operator decision, never an automatic fallback
// during backup, snapshot, or serve.
//
// force additionally claims a target that has no identity marker at all
// (local state and the remote marker both lost). Nothing on disk or in S3
// can distinguish that from someone else's backup at the wrong prefix, so
// force is never implied: the operator must type it, confirming by hand
// what KeepSync cannot verify on its own.
func (s *Service) reclaim(ctx context.Context, force bool) error {
	if _, err := os.Stat(s.statePath("registration.json")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("registration already exists; reclaim only applies when local state is missing")
	}
	source, err := filepath.EvalSymlinks(s.cfg.Source)
	if err != nil {
		return err
	}
	if err := s.safePaths(source); err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("source must be a directory")
	}
	if err := s.hasSourceFiles(ctx, source); err != nil {
		return err
	}
	_, fileID := fileMetadata(info)
	if fileID == "" {
		return errors.New("source filesystem does not provide a stable volume identity")
	}
	store, err := s.live(ctx)
	if err != nil {
		return err
	}
	id, err := store.Identity(ctx)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && !missingObject(err) {
			return err
		}
		if !force {
			return errors.New("backup target has no existing identity to reclaim; rerun with --force to claim it as yours, or start normally on an empty target")
		}
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		id = hex.EncodeToString(random[:])
		if err := store.Repair(ctx, id); err != nil {
			return fmt.Errorf("claim backup target: %w", err)
		}
		s.log.Warn("Claimed backup target without an existing identity", "source", source, "dataset", s.cfg.Dataset)
	}
	if !validSourceID(id) {
		return errors.New("existing backup target has an invalid source identity")
	}
	reg := Registration{Version: 2, Dataset: s.cfg.Dataset, Source: source, Repository: s.cfg.Repository, SourceID: id, IdentityMode: "state", SourceFileID: fileID}
	if err := writeJSON(s.statePath("registration.json"), reg); err != nil {
		return err
	}
	s.log.Info("Backup target reclaimed", "source", source, "dataset", s.cfg.Dataset)
	return nil
}

func validSourceID(id string) bool {
	if len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
func checkSourceMarker(source, id string) error {
	value, err := readSourceIDMarker(source)
	if err != nil {
		return err
	}
	if value != id {
		return errors.New("source identity changed; backup blocked")
	}
	return nil
}

func readSourceIDMarker(source string) (string, error) {
	root, err := os.OpenRoot(source)
	if err != nil {
		return "", err
	}
	defer root.Close()
	info, err := root.Lstat(markerName)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() != 64 {
		return "", errors.New("source identity marker must contain exactly 64 hexadecimal characters")
	}
	f, err := root.Open(markerName)
	if err != nil {
		return "", err
	}
	defer f.Close()
	value, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil {
		return "", err
	}
	if !validSourceID(string(value)) {
		return "", errors.New("invalid source identity marker")
	}
	return string(value), nil
}
