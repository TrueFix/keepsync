package keepsync

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

func (s *Service) pollWatch(ctx context.Context, changed chan<- time.Time, ready chan<- struct{}) {
	s.log.Debug("Metadata polling enabled", "interval", s.cfg.WatchInterval)
	close(ready)
	timer := time.NewTicker(duration(s.cfg.WatchInterval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			found, err := s.pollChanges(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				s.log.Warn("Polling scan failed", "error", err)
				continue
			}
			if found {
				select {
				case changed <- time.Now():
				default:
				}
			}
		}
	}
}

// Compare metadata, not file contents. Hold no filename map; the on-disk index
// is opened only while the upload worker is idle. Emit a wake-up after unlock.
func (s *Service) pollChanges(ctx context.Context) (bool, error) {
	unlock, err := lockState(s.cfg.StateDir)
	if err != nil {
		return false, nil
	}
	defer unlock()
	var reg Registration
	if err := readJSON(s.statePath("registration.json"), &reg); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	source, err := filepath.EvalSymlinks(s.cfg.Source)
	if err != nil {
		return false, err
	}
	if source != reg.Source {
		s.pending.add(".", true)
		return true, nil
	}
	info, err := os.Stat(source)
	if err != nil {
		return false, err
	}
	_, id := fileMetadata(info)
	if reg.IdentityMode == "state" && id != reg.SourceFileID {
		s.pending.add(".", true)
		return true, nil
	}
	if _, err := os.Stat(s.statePath("live-index.db")); errors.Is(err, os.ErrNotExist) {
		if err := s.hasSourceFiles(ctx, source); errors.Is(err, errSourceEmpty) {
			return false, nil
		} else if err != nil {
			return false, err
		}
		s.pending.add(".", true)
		return true, nil
	} else if err != nil {
		return false, err
	}
	index, err := openIndex(s.statePath("live-index.db"))
	if err != nil {
		return false, err
	}
	defer index.close()
	root, err := os.OpenRoot(source)
	if err != nil {
		return false, err
	}
	defer root.Close()
	found := false
	err = walkTree(ctx, source, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
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
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := root.Lstat(filepath.FromSlash(rel))
		if errors.Is(err, os.ErrNotExist) {
			s.pending.add(rel, false)
			found = true
			return nil
		}
		if err != nil {
			return err
		}
		old, exists, err := index.get(rel)
		if err != nil {
			return err
		}
		ctime, id := fileMetadata(info)
		if !exists || old.Deleted || old.Size != info.Size() || old.ModTime != info.ModTime().UnixNano() || old.ChangeTime != ctime || old.FileID != id {
			s.pending.add(rel, false)
			found = true
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	err = index.each(func(rel string, entry fileEntry) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.Deleted || s.excluded(rel, false) {
			return nil
		}
		info, err := root.Lstat(filepath.FromSlash(rel))
		if errors.Is(err, os.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
			s.pending.add(rel, false)
			found = true
			return nil
		}
		return err
	})
	return found, err
}
