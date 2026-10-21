package keepsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

type fileEntry struct {
	Size       int64  `json:"size"`
	Hash       string `json:"sha256"`
	Deleted    bool   `json:"deleted,omitempty"`
	ModTime    int64  `json:"mtime_ns"`
	ChangeTime int64  `json:"ctime_ns"`
	FileID     string `json:"file_id"`
	Generation uint64 `json:"generation"`
}

var errFileBusy = errors.New("source file is still changing")

func validRelativeFile(rel string) bool {
	return rel != "" && rel != "." && rel != markerName && !strings.HasPrefix(rel, "/") && !strings.ContainsAny(rel, "\\\x00") && path.Clean(rel) == rel && rel != ".." && !strings.HasPrefix(rel, "../")
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func fingerprint(r io.Reader) (fileEntry, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	return fileEntry{Size: n, Hash: hex.EncodeToString(h.Sum(nil))}, err
}

func (s *Service) excluded(rel string, dir bool) bool {
	if rel == "." {
		return false
	}
	if rel == markerName {
		return true
	}
	for _, pattern := range s.cfg.Excludes {
		if ok, _ := doublestar.Match(pattern, rel); ok {
			return true
		}
		if dir {
			if ok, _ := doublestar.Match(pattern, rel+"/"); ok {
				return true
			}
		}
	}
	return false
}

// Stage one stable file at a time. S3 receives a fixed byte stream even if the
// application starts another write while the upload is in progress.
func (s *Service) stage(ctx context.Context, root *os.Root, rel string) (*os.File, fileEntry, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, fileEntry{}, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, fileEntry{}, err
	}
	if !before.Mode().IsRegular() {
		return nil, fileEntry{}, errors.New("not a regular file")
	}
	tmp, err := os.CreateTemp(s.cfg.StateDir, ".stage-*")
	if err != nil {
		return nil, fileEntry{}, err
	}
	cleanup := func() { tmp.Close(); os.Remove(tmp.Name()) }
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), &contextReader{ctx, f})
	if err != nil {
		cleanup()
		return nil, fileEntry{}, err
	}
	after, err := f.Stat()
	if err != nil {
		cleanup()
		return nil, fileEntry{}, err
	}
	current, err := root.Lstat(rel)
	beforeCT, _ := fileMetadata(before)
	afterCT, _ := fileMetadata(after)
	if err != nil || !os.SameFile(before, current) || !current.Mode().IsRegular() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || beforeCT != afterCT || n != after.Size() {
		cleanup()
		return nil, fileEntry{}, fmt.Errorf("%w: %q", errFileBusy, rel)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, fileEntry{}, err
	}
	ctime, id := fileMetadata(before)
	return tmp, fileEntry{Size: n, Hash: hex.EncodeToString(h.Sum(nil)), ModTime: before.ModTime().UnixNano(), ChangeTime: ctime, FileID: id}, nil
}

func (s *Service) backup(ctx context.Context) (result error) {
	s.pending.take()
	return s.backupSelection(ctx, []string{""})
}

func (s *Service) backupChanges(ctx context.Context) error {
	paths, full := s.pending.take()
	if full {
		return s.backupSelection(ctx, []string{""})
	}
	if len(paths) == 0 {
		return nil
	}
	return s.backupSelection(ctx, paths)
}

func (s *Service) backupSelection(ctx context.Context, scopes []string) (result error) {
	if err := s.cleanupTransfers(); err != nil {
		return err
	}
	var status Status
	if err := readJSON(s.statePath("status.json"), &status); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	status.LastAttempt = time.Now().UTC()
	status.LastError = "Live backup interrupted or still running"
	if err := writeJSON(s.statePath("status.json"), status); err != nil {
		return err
	}
	defer func() {
		if result == nil {
			status.LastSuccess = time.Now().UTC()
			status.LastError = ""
		} else {
			status.LastError = result.Error()
		}
		result = errors.Join(result, writeJSON(s.statePath("status.json"), status))
	}()
	fullScan := len(scopes) == 1 && scopes[0] == ""
	source, err := s.sourceIdentity(ctx)
	if err != nil {
		return err
	}
	if fullScan {
		if err := s.hasSourceFiles(ctx, source); err != nil {
			return err
		}
	}
	store, err := s.live(ctx)
	if err != nil {
		return err
	}
	index, err := openIndex(s.statePath("live-index.db"))
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, index.close()) }()
	gen, err := index.generation()
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	seen := 0
	uploaded := 0
	removed := 0
	busy := 0
	var total int64
	started := time.Now()
	s.log.Debug("Live backup started")
	visit := func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(source, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if s.excluded(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			s.log.Debug("Skipping non-regular Live entry", "path", rel)
			return nil
		}
		seen++
		old, exists, err := index.get(rel)
		if err != nil {
			return err
		}
		info, err := root.Lstat(filepath.FromSlash(rel))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		ctime, id := fileMetadata(info)
		force := false
		for _, scope := range scopes {
			if scope == rel {
				force = true
				break
			}
		}
		if !force && ctime != 0 && exists && !old.Deleted && old.Size == info.Size() && old.ModTime == info.ModTime().UnixNano() && old.ChangeTime == ctime && old.FileID == id {
			// Periodic full scans verify remote metadata too. Watch-triggered
			// updates avoid a HEAD request for unaffected content.
			matches := true
			if len(scopes) == 1 && scopes[0] == "" {
				matches, err = store.Matches(ctx, rel, old)
				if err != nil {
					return err
				}
			}
			if matches {
				old.Generation = gen
				return index.touch(rel, old)
			}
		}
		stage, entry, err := s.stage(ctx, root, filepath.FromSlash(rel))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if errors.Is(err, errFileBusy) {
			busy++
			s.pending.add(rel, false)
			if exists {
				old.Generation = gen
				if err := index.touch(rel, old); err != nil {
					return err
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
		defer func() { stage.Close(); os.Remove(stage.Name()) }()
		entry.Generation = gen
		if exists && !old.Deleted && old.Hash == entry.Hash {
			matches, err := store.Matches(ctx, rel, entry)
			if err != nil {
				return err
			}
			if matches {
				return index.touch(rel, entry)
			}
		}
		if err := store.Upload(ctx, rel, stage, entry); err != nil {
			return fmt.Errorf("upload %q: %w", rel, err)
		}
		if err := index.put(rel, entry); err != nil {
			return err
		}
		uploaded++
		total += entry.Size
		if s.cfg.LogFiles {
			action := "modified"
			if !exists || old.Deleted {
				action = "added"
			}
			s.log.Info("FILE uploaded", "path", rel, "change", action, "size_change", fmt.Sprintf("%d -> %d B", old.Size, entry.Size), "old_bytes", old.Size, "bytes", entry.Size)
		}
		return nil
	}
	for _, scope := range scopes {
		if scope != "" && !validRelativeFile(scope) {
			return errors.New("invalid changed path")
		}
		start := filepath.Join(source, filepath.FromSlash(scope))
		if _, err := os.Lstat(start); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := walkTree(ctx, start, visit); err != nil {
			return err
		}
	}
	if err := index.flush(); err != nil {
		return err
	}
	if seen == 0 && len(scopes) == 1 && scopes[0] == "" {
		return errors.New("no eligible regular files; Live backup blocked")
	}
	// A final identity/empty-source check prevents a removed mount from becoming
	// a deletion sweep. Only keys from our persisted index can ever be removed.
	verify := s.sourceIdentity
	if fullScan {
		verify = s.source
	}
	if _, err := verify(ctx); err != nil {
		return err
	}
	for _, scope := range scopes {
		after := ""
		for {
			rel, entry, err := index.stale(scope, after, gen)
			if err != nil {
				return err
			}
			if rel == "" {
				break
			}
			after = rel
			if s.excluded(rel, false) {
				continue
			}
			if s.cfg.DeleteRemote {
				if err := store.Delete(ctx, rel); err != nil {
					return err
				}
				if err := index.remove(rel); err != nil {
					return err
				}
			} else {
				entry.Deleted = true
				if err := index.put(rel, entry); err != nil {
					return err
				}
			}
			if s.cfg.LogFiles {
				s.log.Info("FILE removed locally", "path", rel, "remote_deleted", s.cfg.DeleteRemote)
			}
			removed++
		}
	}
	if !s.cfg.LogFiles && (uploaded > 0 || removed > 0) {
		s.log.Info("Live backup complete", "uploaded_files", uploaded, "removed_files", removed, "uploaded_bytes", total, "duration", time.Since(started).Round(time.Millisecond))
	} else {
		s.log.Debug("Live backup complete", "uploaded_files", uploaded, "removed_files", removed, "uploaded_bytes", total, "duration", time.Since(started).Round(time.Millisecond))
	}
	if busy > 0 {
		return fmt.Errorf("%w: %d files queued again", errFileBusy, busy)
	}
	return nil
}

func (s *Service) cleanupTransfers() error {
	for _, pattern := range []string{".stage-*", ".archive-*"} {
		stale, err := filepath.Glob(s.statePath(pattern))
		if err != nil {
			return err
		}
		for _, p := range stale {
			if err := os.Remove(p); err != nil {
				return fmt.Errorf("remove interrupted transfer file: %w", err)
			}
		}
	}
	return nil
}
