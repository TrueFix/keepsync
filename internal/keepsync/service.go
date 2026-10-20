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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Service struct {
	cfg Config
	out io.Writer
	err io.Writer
}

func New(c Config, out, stderr io.Writer) *Service { return &Service{c, out, stderr} }

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
	unlock, err := lockState(s.cfg.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
	switch args[0] {
	case "init":
		return s.restic(ctx, s.out, "init")
	case "register-source":
		return s.register(ctx)
	case "backup":
		return s.backup(ctx)
	case "snapshots", "check", "prune":
		if _, err := s.registration(ctx); err != nil {
			return err
		}
		switch args[0] {
		case "snapshots":
			return s.restic(ctx, s.out, "snapshots", "--tag", s.tag())
		case "check":
			return s.restic(ctx, s.out, "check", "--read-data")
		default:
			return s.prune(ctx)
		}
	case "status":
		var status Status
		if err := readJSON(s.statePath("status.json"), &status); err != nil {
			return err
		}
		return json.NewEncoder(s.out).Encode(status)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func (s *Service) statePath(name string) string { return filepath.Join(s.cfg.StateDir, name) }
func (s *Service) tag() string                  { return "keepsync:" + s.cfg.Dataset }

func (s *Service) restic(ctx context.Context, out io.Writer, args ...string) error {
	base := []string{"--repo", s.cfg.Repository, "--password-file", s.cfg.PasswordFile}
	cmd := exec.CommandContext(ctx, s.cfg.Restic, append(base, args...)...)
	// The configuration, rather than inherited Restic selectors, defines this dataset.
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(key, "RESTIC_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Stdout, cmd.Stderr = out, s.err
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restic %s failed: %w", args[0], err)
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
	if err := hasFiles(source); err != nil {
		return err
	}
	repoID, err := s.repositoryID(ctx)
	if err != nil {
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
	reg := Registration{s.cfg.Dataset, source, string(id), s.cfg.Repository, repoID}
	return writeJSON(s.statePath("registration.json"), reg)
}

func (s *Service) registration(ctx context.Context) (Registration, error) {
	var reg Registration
	if err := readJSON(s.statePath("registration.json"), &reg); err != nil {
		return reg, fmt.Errorf("source must be explicitly registered: %w", err)
	}
	if reg.Dataset != s.cfg.Dataset || reg.Repository != s.cfg.Repository || reg.SourceID == "" || reg.RepositoryID == "" {
		return reg, errors.New("configuration does not match registered dataset/repository")
	}
	id, err := s.repositoryID(ctx)
	if err != nil {
		return reg, err
	}
	if reg.RepositoryID != id {
		return reg, errors.New("repository identity changed; refusing operation")
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
	return nil
}

func hasFiles(source string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("source must be a directory")
	}
	found := false
	err = filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != filepath.Join(source, markerName) && (d.Type().IsRegular() || d.Type()&os.ModeSymlink != 0) {
			found = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return errors.New("source is empty; backup blocked")
	}
	return nil
}

func (s *Service) backup(ctx context.Context) (result error) {
	var status Status
	if err := readJSON(s.statePath("status.json"), &status); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot read previous backup status: %w", err)
	}
	status.LastAttempt = time.Now().UTC()
	status.LastError = "backup interrupted or still running"
	if err := writeJSON(s.statePath("status.json"), status); err != nil {
		return err
	}
	defer func() {
		if result == nil {
			status.LastSuccess, status.LastError = time.Now().UTC(), ""
		} else {
			status.LastError = result.Error()
		}
		result = errors.Join(result, writeJSON(s.statePath("status.json"), status))
	}()
	reg, err := s.registration(ctx)
	if err != nil {
		return err
	}
	source, err := filepath.EvalSymlinks(s.cfg.Source)
	if err != nil {
		return err
	}
	if source != reg.Source {
		return errors.New("source path differs from registered source")
	}
	if err := s.safePaths(source); err != nil {
		return err
	}
	markerPath := filepath.Join(source, markerName)
	info, err := os.Lstat(markerPath)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("source identity marker missing or not a regular file")
	}
	id, err := os.ReadFile(markerPath)
	if err != nil {
		return err
	}
	if string(id) != reg.SourceID {
		return errors.New("source identity changed; backup blocked")
	}
	if err := hasFiles(source); err != nil {
		return err
	}
	args := []string{"backup", "--tag", s.tag(), "--host", s.cfg.Dataset, "--exclude", markerPath}
	for _, exclude := range s.cfg.Excludes {
		args = append(args, "--exclude", exclude)
	}
	args = append(args, "--", source)
	return s.restic(ctx, s.out, args...)
}

func (s *Service) serve(ctx context.Context) error {
	interval, _ := time.ParseDuration(s.cfg.Interval)
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := s.Run(ctx, []string{"backup"}); err != nil && ctx.Err() == nil {
			fmt.Fprintln(s.err, "backup blocked or failed:", err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (s *Service) prune(ctx context.Context) error {
	r := s.cfg.Retention
	if !r.Enabled {
		return errors.New("retention is disabled; no snapshots removed")
	}
	var status Status
	if err := readJSON(s.statePath("status.json"), &status); err != nil {
		return fmt.Errorf("retention requires a successful backup: %w", err)
	}
	if status.LastSuccess.IsZero() || status.LastError != "" {
		return errors.New("retention blocked: the last backup did not succeed")
	}
	return s.restic(ctx, s.out, "forget", "--prune", "--tag", s.tag(), "--group-by", "tags", "--keep-daily", fmt.Sprint(r.KeepDaily), "--keep-weekly", fmt.Sprint(r.KeepWeekly), "--keep-monthly", fmt.Sprint(r.KeepMonthly))
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
		return errors.New("restore requires --snapshot HEX_ID (8–64 characters) and --target ABSOLUTE_PATH")
	}
	if _, err := hex.DecodeString(*snapshot); err != nil {
		return errors.New("snapshot must be a hexadecimal ID, not latest")
	}
	unlock, err := lockState(s.cfg.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
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
	return s.restic(ctx, s.out, "restore", snapshots[0].ID, "--target", dest, "--verify")
}
