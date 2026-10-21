package keepsync

import (
	"bytes"
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
	"os/exec"
	"path/filepath"
	"strings"
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

var errStateVolumeRequired = errors.New("local source state is missing for a non-empty backup target; reattach the original keepsync-state volume")

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
		if !s.cfg.Snapshots.Enabled {
			s.log.Info("Snapshots disabled; Live setup runs automatically on startup")
			return nil
		}
		if s.cfg.Snapshots.Format == "tar" {
			s.log.Info("TAR snapshots need no password or repository initialization")
			return nil
		}
		var reg Registration
		existing := false
		if err := readJSON(s.statePath("registration.json"), &reg); err == nil {
			existing = true
			if _, err := s.registration(ctx); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := s.restic(ctx, s.out, "init"); err != nil {
			return err
		}
		if existing {
			id, err := s.repositoryID(ctx)
			if err != nil {
				return err
			}
			reg.RepositoryID = id
			return writeJSON(s.statePath("registration.json"), reg)
		}
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
		if _, err := s.snapshotRegistration(ctx); err != nil {
			return err
		}
		switch args[0] {
		case "snapshots":
			if s.cfg.Snapshots.Format == "tar" {
				return s.listTar(ctx)
			}
			return s.restic(ctx, s.out, "snapshots", "--tag", s.tag())
		case "check":
			if s.cfg.Snapshots.Format == "tar" {
				return s.checkTar(ctx)
			}
			return s.restic(ctx, s.out, "check", "--read-data")
		default:
			return s.prune(ctx)
		}
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func (s *Service) statePath(name string) string { return filepath.Join(s.cfg.StateDir, name) }
func (s *Service) tag() string                  { return "keepsync:" + s.cfg.Dataset }

func (s *Service) restic(ctx context.Context, out io.Writer, args ...string) error {
	t, err := parseTarget(s.cfg.Repository, s.cfg.S3Endpoint)
	if err != nil {
		return err
	}
	base := []string{"--repo", t.snapshots(), "--password-file", s.cfg.PasswordFile}
	if t.bucket != "" {
		base = append(base, "-o", "s3.connections=2")
	}
	cmd := exec.CommandContext(ctx, s.cfg.Restic, append(base, args...)...)
	// The configuration, rather than inherited Restic selectors, defines this dataset.
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(key, "RESTIC_") && key != "GOMEMLIMIT" && key != "GOMAXPROCS" && key != "TMPDIR" {
			cmd.Env = append(cmd.Env, value)
		}
	}
	memory := os.Getenv("KEEPSYNC_SNAPSHOT_MEMORY_LIMIT")
	if memory == "" {
		memory = "128MiB"
	}
	if err := os.MkdirAll(s.statePath("tmp"), 0700); err != nil {
		return err
	}
	cmd.Env = append(cmd.Env, "GOMAXPROCS=1", "GOMEMLIMIT="+memory, "TMPDIR="+s.statePath("tmp"))
	stderr := cappedOutput{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restic %s failed: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (s *Service) repositoryID(ctx context.Context) (string, error) {
	var b bytes.Buffer
	if err := s.restic(ctx, &b, "cat", "config"); err != nil {
		return "", err
	}
	var cfg struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b.Bytes(), &cfg); err != nil || cfg.ID == "" {
		return "", errors.New("cannot read repository identity")
	}
	return cfg.ID, nil
}

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
	var repoID string
	if s.cfg.Snapshots.Enabled && s.cfg.Snapshots.Format == "restic" {
		repoID, err = s.repositoryID(ctx)
		if err != nil {
			return err
		}
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
	reg := Registration{Version: 2, Dataset: s.cfg.Dataset, Source: source, SourceID: string(id), Repository: s.cfg.Repository, RepositoryID: repoID}
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
		return reg, err
	}
	if reg.SourceID != id {
		return reg, errors.New("Live source identity changed; refusing operation")
	}
	return reg, nil
}

func (s *Service) snapshotRegistration(ctx context.Context) (Registration, error) {
	reg, err := s.registration(ctx)
	if err != nil {
		return reg, err
	}
	if s.cfg.Snapshots.Format == "tar" {
		return reg, nil
	}
	if reg.RepositoryID == "" {
		return reg, errors.New("encrypted snapshot repository is not initialized yet")
	}
	id, err := s.repositoryID(ctx)
	if err != nil {
		return reg, err
	}
	if id != reg.RepositoryID {
		return reg, errors.New("Snapshots repository identity changed; refusing operation")
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
	password, err := resolved(s.cfg.PasswordFile)
	if err != nil {
		return err
	}
	if overlaps(source, state) || overlaps(source, restore) || overlaps(state, restore) || within(source, password) {
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
		if overlaps(source, dest) || overlaps(state, dest) || overlaps(restore, dest) || within(dest, password) {
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
	if snapshotFormat(status) != s.cfg.Snapshots.Format || status.LastSnapshot.IsZero() || status.SnapshotError != "" {
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
	if s.cfg.Snapshots.Format == "tar" {
		if !validArchiveID(*snapshot) {
			return errors.New("snapshot must be the exact .tar.gz filename returned by snapshots")
		}
	} else {
		*snapshot = strings.ToLower(*snapshot)
		for _, ch := range *snapshot {
			if !strings.ContainsRune("0123456789abcdef", ch) {
				return errors.New("snapshot must be a hexadecimal ID, not latest")
			}
		}
	}
	unlock, err := lockState(s.cfg.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.cleanupTransfers(); err != nil {
		return err
	}
	reg, err := s.snapshotRegistration(ctx)
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
	password, err := resolved(s.cfg.PasswordFile)
	if err != nil {
		return err
	}
	if dest == root || !within(root, dest) || overlaps(dest, source) || overlaps(dest, reg.Source) || overlaps(dest, state) || within(dest, password) {
		return errors.New("restore target must be a child of restore_root and must not overlap protected paths")
	}
	entries, err := os.ReadDir(dest)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(entries) != 0 {
		return errors.New("restore target is not empty")
	}
	if s.cfg.Snapshots.Format == "tar" {
		return s.restoreTar(ctx, reg, *snapshot, dest)
	}
	var b bytes.Buffer
	if err := s.restic(ctx, &b, "snapshots", "--json", "--tag", s.tag(), *snapshot); err != nil {
		return err
	}
	var snapshots []struct {
		ID   string   `json:"id"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(b.Bytes(), &snapshots); err != nil {
		return err
	}
	if len(snapshots) != 1 || !strings.HasPrefix(snapshots[0].ID, *snapshot) {
		return errors.New("snapshot not uniquely found in this dataset")
	}
	tagged := false
	for _, tag := range snapshots[0].Tags {
		if tag == s.tag() {
			tagged = true
		}
	}
	if !tagged {
		return errors.New("snapshot belongs to another dataset")
	}
	if err := os.MkdirAll(dest, 0700); err != nil {
		return err
	}
	if err := s.restic(ctx, s.out, "restore", snapshots[0].ID, "--target", dest, "--verify"); err != nil {
		return err
	}
	return writeRecoveryMarker(dest, reg)
}

func writeRecoveryMarker(dest string, reg Registration) error {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()
	rel := filepath.Join(strings.TrimPrefix(reg.Source, string(filepath.Separator)), markerName)
	if existing, err := root.ReadFile(rel); err == nil {
		if string(existing) != reg.SourceID {
			return errors.New("restored source identity differs from registration")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(rel)
	info, err := root.Stat(dir)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0200 == 0 {
		if err := root.Chmod(dir, info.Mode().Perm()|0200); err != nil {
			return err
		}
		defer root.Chmod(dir, info.Mode().Perm())
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0444)
	if err != nil {
		return err
	}
	_, err = f.WriteString(reg.SourceID)
	return errors.Join(err, f.Sync(), f.Close())
}
