package keepsync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

type Service struct {
	cfg     Config
	out     io.Writer
	err     io.Writer
	log     *slog.Logger
	pending *changeSet
	store   liveStore
}

var errSourceEmpty = errors.New("source directory has no eligible files; waiting for data")

var errStateVolumeRequired = errors.New("local source state is missing for a non-empty backup target; run `keepsync reclaim` to reattach it, or point KEEPSYNC_TARGET at a new prefix")

func New(c Config, out, stderr io.Writer) *Service {
	if out == nil {
		out = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	var level slog.Level
	_ = level.UnmarshalText([]byte(c.LogLevel))
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler = newCompactHandler(out, c, level)
	if c.LogFormat == "json" {
		h = slog.NewJSONHandler(out, opts)
	}
	return &Service{cfg: c, out: out, err: stderr, log: slog.New(h), pending: &changeSet{}}
}

func (s *Service) Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("a command is required")
	}
	if err := s.cfg.Validate(); err != nil {
		return err
	}
	source, err := resolved(s.cfg.Source)
	if err != nil {
		return err
	}
	if err := s.safePaths(source); err != nil {
		return err
	}
	if args[0] == "restore" {
		return s.restore(ctx, args[1:])
	}
	if args[0] == "reclaim" {
		force := false
		switch len(args) {
		case 1:
		case 2:
			if args[1] != "--force" {
				return fmt.Errorf("unknown reclaim option %q", args[1])
			}
			force = true
		default:
			return errors.New("unexpected command arguments")
		}
		unlock, err := lockState(s.cfg.StateDir)
		if err != nil {
			return err
		}
		defer unlock()
		return s.reclaim(ctx, force)
	}
	if len(args) != 1 {
		return errors.New("unexpected command arguments")
	}
	if args[0] == "serve" {
		return s.serve(ctx)
	}
	if args[0] == "status" {
		var status Status
		if err := readJSON(s.statePath("status.json"), &status); err != nil {
			return err
		}
		return json.NewEncoder(s.out).Encode(status)
	}
	unlock, err := lockState(s.cfg.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
	if args[0] != "backup" {
		if err := s.cleanupTransfers(); err != nil {
			return err
		}
	}
	switch args[0] {
	case "init":
		s.log.Info("Setup runs automatically on startup; no initialization required")
		return nil
	case "register-source":
		return s.register(ctx)
	case "backup":
		if err := s.ensureSetup(ctx); err != nil {
			return err
		}
		return s.backup(ctx)
	case "snapshot":
		if !s.cfg.Snapshots.Enabled {
			return s.snapshot(ctx)
		}
		if err := s.ensureSetup(ctx); err != nil {
			return err
		}
		return s.snapshot(ctx)
	case "snapshots", "check", "prune":
		if _, err := s.registration(ctx); err != nil {
			return err
		}
		switch args[0] {
		case "snapshots":
			return s.listTar(ctx)
		case "check":
			return s.checkTar(ctx)
		default:
			return s.prune(ctx)
		}
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func (s *Service) statePath(name string) string { return filepath.Join(s.cfg.StateDir, name) }

func (s *Service) register(ctx context.Context) error {
	if _, err := os.Stat(s.statePath("registration.json")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("registration already exists or cannot be read; use a new state directory for an explicit rebind")
	}
	source, err := filepath.EvalSymlinks(s.cfg.Source)
	if err != nil {
		return err
	}
	if err := s.safePaths(source); err != nil {
		return err
	}
	if err := s.hasSourceFiles(ctx, source); err != nil {
		return err
	}
	marker := filepath.Join(source, markerName)
	if info, err := os.Lstat(marker); err == nil && !info.Mode().IsRegular() {
		return errors.New("source identity marker must be a regular file")
	}
	id, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		id = []byte(hex.EncodeToString(random[:]))
		f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0444)
		if err != nil {
			return err
		}
		_, writeErr := f.Write(id)
		syncErr := f.Sync()
		if err := errors.Join(writeErr, syncErr, f.Close()); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if len(id) != 64 {
		return errors.New("source identity must be 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(string(id)); err != nil {
		return errors.New("invalid source identity")
	}
	store, err := s.live(ctx)
	if err != nil {
		return err
	}
	if err := store.Bind(ctx, string(id)); err != nil {
		return err
	}
	reg := Registration{Version: 2, Dataset: s.cfg.Dataset, Source: source, SourceID: string(id), Repository: s.cfg.Repository}
	s.log.Info("Source registered", "source", source, "dataset", s.cfg.Dataset)
	return writeJSON(s.statePath("registration.json"), reg)
}

func (s *Service) registration(ctx context.Context) (Registration, error) {
	var reg Registration
	if err := readJSON(s.statePath("registration.json"), &reg); err != nil {
		return reg, fmt.Errorf("source setup is incomplete: %w", err)
	}
	if reg.Version != 2 {
		return reg, errors.New("legacy registration: use a new state directory and a fresh Live/Snapshots target; existing backups are unchanged")
	}
	if reg.Dataset != s.cfg.Dataset || reg.Repository != s.cfg.Repository || reg.SourceID == "" {
		return reg, errors.New("configuration does not match registered dataset/repository")
	}
	if len(reg.SourceID) != 64 {
		return reg, errors.New("invalid registered source identity")
	}
	if reg.IdentityMode != "" && reg.IdentityMode != "marker" && reg.IdentityMode != "state" {
		return reg, errors.New("invalid source identity mode")
	}
	if reg.IdentityMode == "state" && reg.SourceFileID == "" {
		return reg, errors.New("state-backed source identity is incomplete")
	}
	if _, err := hex.DecodeString(reg.SourceID); err != nil {
		return reg, errors.New("invalid registered source identity")
	}
	store, err := s.live(ctx)
	if err != nil {
		return reg, err
	}
	id, err := store.Identity(ctx)
	if err != nil {
		if !missingObject(err) && !errors.Is(err, os.ErrNotExist) {
			return reg, err
		}
		// Local state already proves ownership of this source; recreate the
		// marker someone deleted on the target instead of refusing forever.
		if err := store.Repair(ctx, reg.SourceID); err != nil {
			return reg, fmt.Errorf("recreate missing identity marker: %w", err)
		}
		s.log.Warn("Recreated missing Live identity marker", "dataset", reg.Dataset)
		id = reg.SourceID
	}
	if reg.SourceID != id {
		return reg, errors.New("Live source identity changed; refusing operation")
	}
	return reg, nil
}

// Resolve existing parent symlinks even when the final path does not exist yet.
func resolved(path string) (string, error) {
	path = filepath.Clean(path)
	actual, err := filepath.EvalSymlinks(path)
	if err == nil {
		return actual, nil
	}
	if !errors.Is(err, os.ErrNotExist) || path == filepath.Dir(path) {
		return "", err
	}
	parent, err := resolved(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func (s *Service) safePaths(source string) error {
	state, err := resolved(s.cfg.StateDir)
	if err != nil {
		return err
	}
	restore, err := resolved(s.cfg.RestoreRoot)
	if err != nil {
		return err
	}
	if overlaps(source, state) || overlaps(source, restore) || overlaps(state, restore) {
		return errors.New("resolved paths overlap protected data")
	}
	t, err := parseTarget(s.cfg.Repository, s.cfg.S3Endpoint)
	if err != nil {
		return err
	}
	if t.local != "" {
		dest, err := resolved(t.local)
		if err != nil {
			return err
		}
		if overlaps(source, dest) || overlaps(state, dest) || overlaps(restore, dest) {
			return errors.New("local target overlaps protected paths")
		}
	}
	return nil
}

func hasFiles(source string) error {
	return hasFilesContext(context.Background(), source)
}

func hasFilesContext(ctx context.Context, source string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("source must be a directory")
	}
	found := false
	err = walkTree(ctx, source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != filepath.Join(source, markerName) && d.Type().IsRegular() {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return errSourceEmpty
	}
	return nil
}

func (s *Service) source(ctx context.Context) (string, error) {
	source, err := s.sourceIdentity(ctx)
	if err != nil {
		return "", err
	}
	if err := s.hasSourceFiles(ctx, source); err != nil {
		return "", err
	}
	return source, nil
}

func (s *Service) sourceIdentity(ctx context.Context) (string, error) {
	reg, err := s.registration(ctx)
	if err != nil {
		return "", err
	}
	source, err := filepath.EvalSymlinks(s.cfg.Source)
	if err != nil {
		return "", err
	}
	if source != reg.Source {
		return "", errors.New("source path differs from registered source")
	}
	if err := s.safePaths(source); err != nil {
		return "", err
	}
	if reg.IdentityMode == "state" {
		info, err := os.Stat(source)
		if err != nil {
			return "", err
		}
		_, id := fileMetadata(info)
		if !info.IsDir() {
			return "", errors.New("source must be a directory")
		}
		if id != reg.SourceFileID {
			if err := checkSourceMarker(source, reg.SourceID); err != nil {
				return "", errors.New("source volume changed; existing backup identity preserved")
			}
			reg.SourceFileID = id
			if err := writeJSON(s.statePath("registration.json"), reg); err != nil {
				return "", err
			}
			s.log.Info("Recovered source identity verified", "source", source)
		}
		if _, err := os.Lstat(filepath.Join(source, markerName)); err == nil {
			if err := checkSourceMarker(source, reg.SourceID); err != nil {
				return "", err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	} else if err := checkSourceMarker(source, reg.SourceID); err != nil {
		return "", err
	}
	return source, nil
}

func (s *Service) hasSourceFiles(ctx context.Context, source string) error {
	if len(s.cfg.Excludes) == 0 {
		return hasFilesContext(ctx, source)
	}
	found := false
	err := walkTree(ctx, source, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if s.excluded(rel, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return errSourceEmpty
	}
	return nil
}

func (s *Service) prune(ctx context.Context) error {
	if !s.cfg.Snapshots.Enabled {
		return errors.New("snapshots are disabled")
	}
	var status Status
	if err := readJSON(s.statePath("status.json"), &status); err != nil {
		return fmt.Errorf("retention requires a successful backup: %w", err)
	}
	if status.SnapshotFormat != "tar" || status.LastSnapshot.IsZero() || status.SnapshotError != "" {
		return errors.New("retention blocked: the last snapshot did not succeed")
	}
	return s.retain(ctx)
}

func (s *Service) restore(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(s.err)
	snapshot := flags.String("snapshot", "", "explicit snapshot ID")
	target := flags.String("target", "", "new or empty directory under restore_root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || len(*snapshot) < 8 || len(*snapshot) > 64 || !filepath.IsAbs(*target) {
		return errors.New("restore requires --snapshot ID (8–64 characters) and --target ABSOLUTE_PATH")
	}
	if !validArchiveID(*snapshot) {
		return errors.New("snapshot must be the exact .tar.gz filename returned by snapshots")
	}
	unlock, err := lockState(s.cfg.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.cleanupTransfers(); err != nil {
		return err
	}
	reg, err := s.registration(ctx)
	if err != nil {
		return err
	}
	root, err := resolved(s.cfg.RestoreRoot)
	if err != nil {
		return err
	}
	dest, err := resolved(*target)
	if err != nil {
		return err
	}
	source, err := resolved(s.cfg.Source)
	if err != nil {
		return err
	}
	state, err := resolved(s.cfg.StateDir)
	if err != nil {
		return err
	}
	if dest == root || !within(root, dest) || overlaps(dest, source) || overlaps(dest, reg.Source) || overlaps(dest, state) {
		return errors.New("restore target must be a child of restore_root and must not overlap protected paths")
	}
	entries, err := os.ReadDir(dest)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(entries) != 0 {
		return errors.New("restore target is not empty")
	}
	return s.restoreTar(ctx, reg, *snapshot, dest)
}
