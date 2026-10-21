package keepsync

import (
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Bound a burst's bookkeeping. Overflow promotes the next batch to a full scan.
type changeSet struct {
	mu    sync.Mutex
	paths map[string]struct{}
	full  bool
}

func (c *changeSet) add(rel string, full bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if full || rel == "." || len(c.paths) >= 4096 {
		c.full = true
		clear(c.paths)
		return
	}
	if c.full || !validRelativeFile(rel) {
		return
	}
	if c.paths == nil {
		c.paths = make(map[string]struct{})
	}
	c.paths[rel] = struct{}{}
}
func (c *changeSet) take() ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	full := c.full
	c.full = false
	paths := make([]string, 0, len(c.paths))
	for p := range c.paths {
		paths = append(paths, p)
	}
	clear(c.paths)
	sort.Strings(paths)
	compact := paths[:0]
	parents := map[string]bool{}
	for _, p := range paths {
		covered := false
		for parent := path.Dir(p); parent != "." && parent != "/"; parent = path.Dir(parent) {
			if parents[parent] {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		compact = append(compact, p)
		parents[p] = true
	}
	return compact, full
}

func (s *Service) watch(ctx context.Context, changed chan<- time.Time, ready chan<- struct{}) {
	w, err := fsnotify.NewBufferedWatcher(1024)
	if err != nil {
		close(ready)
		s.log.Error("Watcher unavailable; interval reconciliation remains active", "error", err)
		return
	}
	defer w.Close()
	notify := func(rel string, full bool) {
		s.pending.add(rel, full)
		select {
		case changed <- time.Now():
		default:
		}
	}
	root, err := filepath.EvalSymlinks(s.cfg.Source)
	if err != nil {
		root = filepath.Clean(s.cfg.Source)
	}
	watched := make(map[string]bool)
	limited, warned := false, false
	addTree := func(start string) error {
		return walkTree(ctx, start, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			if s.excluded(filepath.ToSlash(rel), d.IsDir()) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() && !watched[p] {
				if len(watched) >= s.cfg.MaxWatches {
					limited = true
					return fs.SkipDir
				}
				if err := w.Add(p); err != nil {
					return err
				}
				watched[p] = true
			}
			return nil
		})
	}
	if err := addTree(root); err != nil {
		s.log.Warn("Watcher registration incomplete; interval reconciliation remains active", "error", err)
	}
	if limited {
		s.log.Warn("Watch limit reached; remaining directories use interval reconciliation", "max_watches", s.cfg.MaxWatches)
		warned = true
	}
	rootInfo, _ := os.Stat(root)
	close(ready)
	// One root stat repairs mount replacements; no recurring tree walk when idle.
	repair := time.NewTicker(30 * time.Second)
	defer repair.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.Events:
			if !ok {
				return
			}
			rel, err := filepath.Rel(root, event.Name)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if rel != "." && !validRelativeFile(rel) {
				continue
			}
			if s.excluded(rel, false) {
				continue
			}
			if (event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename)) && watched[event.Name] {
				for p := range watched {
					if p == event.Name || strings.HasPrefix(p, event.Name+string(filepath.Separator)) {
						_ = w.Remove(p)
						delete(watched, p)
					}
				}
			}
			if event.Has(fsnotify.Create) {
				if info, err := os.Lstat(event.Name); err == nil && info.IsDir() {
					if err := addTree(event.Name); err != nil {
						s.log.Warn("Cannot watch new directory; requesting full reconciliation", "error", err)
						notify(".", true)
					}
				}
			}
			if limited && !warned {
				s.log.Warn("Watch limit reached; remaining directories use interval reconciliation", "max_watches", s.cfg.MaxWatches)
				warned = true
			}
			if s.cfg.LogLevel == "debug" {
				s.log.Debug("Filesystem change", "path", rel, "operation", event.Op.String())
			}
			notify(rel, false)
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			s.log.Warn("Watcher event lost; full reconciliation requested", "error", err)
			for p := range watched {
				_ = w.Remove(p)
				delete(watched, p)
			}
			if err := addTree(root); err != nil {
				s.log.Warn("Watcher repair incomplete", "error", err)
			}
			notify(".", true)
		case <-repair.C:
			info, err := os.Stat(s.cfg.Source)
			if err == nil && (rootInfo == nil || !os.SameFile(rootInfo, info)) {
				for p := range watched {
					_ = w.Remove(p)
					delete(watched, p)
				}
				if resolved, err := filepath.EvalSymlinks(s.cfg.Source); err == nil {
					root = resolved
				}
				rootInfo = info
				if err := addTree(root); err != nil {
					s.log.Warn("Returning source watch incomplete", "error", err)
				}
				notify(".", true)
			}
		}
	}
}

type batch struct{ first, last time.Time }

func (b *batch) changed(now time.Time) {
	if b.first.IsZero() {
		b.first = now
	}
	b.last = now
}
func (b batch) deadline(quiet, maxWait time.Duration) time.Time {
	if b.first.IsZero() {
		return time.Time{}
	}
	return earlier(b.last.Add(quiet), b.first.Add(maxWait))
}
func (b batch) due(now time.Time, quiet, maxWait time.Duration) bool {
	d := b.deadline(quiet, maxWait)
	return !d.IsZero() && !now.Before(d)
}
func earlier(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}
func later(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}

func (s *Service) serve(parent context.Context) error {
	if parent.Err() != nil {
		return nil
	}
	unlock, err := lockNamed(s.cfg.StateDir, "daemon.lock")
	if err != nil {
		return err
	}
	defer unlock()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	changes := make(chan time.Time, 1)
	watchDone := make(chan struct{})
	ready := make(chan struct{})
	if s.cfg.Method == "live" {
		go func() { defer close(watchDone); s.watch(ctx, changes, ready) }()
	} else {
		close(watchDone)
		close(ready)
	}
	defer func() { cancel(); <-watchDone }()
	select {
	case <-ready:
	case <-ctx.Done():
		return nil
	}
	s.log.Info("KeepSync started", "method", s.cfg.Method, "source", s.cfg.Source, "interval", s.cfg.Interval, "snapshots", s.cfg.Snapshots.Enabled)
	return s.runLoop(ctx, changes, func(ctx context.Context, command string) error {
		if command == "changes" {
			unlock, err := lockState(s.cfg.StateDir)
			if err != nil {
				return err
			}
			defer unlock()
			return s.backupChanges(ctx)
		}
		return s.Run(ctx, []string{command})
	})
}

func (s *Service) snapshotDeadline(now time.Time) time.Time {
	if !s.cfg.Snapshots.Enabled {
		return time.Time{}
	}
	var status Status
	if err := readJSON(s.statePath("status.json"), &status); err != nil || status.LastSnapshot.IsZero() || status.SnapshotError != "" {
		return now
	}
	return status.LastSnapshot.Add(duration(s.cfg.Snapshots.Interval))
}

// Sleep until the next actual deadline or filesystem event, rather than polling.
func (s *Service) runLoop(ctx context.Context, changes <-chan time.Time, run func(context.Context, string) error) error {
	type outcome struct {
		command string
		err     error
	}
	done := make(chan outcome, 1)
	pendingBackup, pendingChanges, pendingSnapshot := true, false, false
	firstLive := true
	lastWasSnapshot := false
	active := ""
	var dirty batch
	var retry time.Time
	nextBackup := time.Now().Add(duration(s.cfg.Interval))
	nextSnapshot := s.snapshotDeadline(time.Now())
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			if active != "" {
				<-done
			}
			return nil
		}
		now := time.Now()
		if active != "backup" && !pendingBackup && !now.Before(nextBackup) {
			pendingBackup = true
		}
		if s.cfg.Method == "live" && !pendingChanges && !now.Before(retry) && dirty.due(now, duration(s.cfg.Debounce), duration(s.cfg.MaxWait)) {
			pendingChanges = true
		}
		if active != "snapshot" && !pendingSnapshot && !nextSnapshot.IsZero() && !now.Before(nextSnapshot) {
			if s.snapshotDue(now) {
				pendingSnapshot = true
			} else {
				nextSnapshot = s.snapshotDeadline(now)
			}
		}
		if active == "" {
			command := ""
			if pendingSnapshot && !firstLive && !lastWasSnapshot {
				command = "snapshot"
				pendingSnapshot = false
			} else if pendingBackup {
				command = "backup"
				pendingBackup = false
				pendingChanges = false
				dirty = batch{}
			} else if pendingChanges {
				command = "changes"
				pendingChanges = false
				dirty = batch{}
			} else if pendingSnapshot {
				command = "snapshot"
				pendingSnapshot = false
			}
			if command != "" {
				active = command
				go func() { done <- outcome{command, run(ctx, command)} }()
			}
		}
		var deadline time.Time
		if active != "backup" && !pendingBackup {
			deadline = nextBackup
		}
		if s.cfg.Method == "live" && !pendingChanges {
			if d := dirty.deadline(duration(s.cfg.Debounce), duration(s.cfg.MaxWait)); !d.IsZero() {
				deadline = earlier(deadline, later(d, retry))
			}
		}
		if active != "snapshot" && !pendingSnapshot {
			deadline = earlier(deadline, nextSnapshot)
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		var wake <-chan time.Time
		if !deadline.IsZero() {
			timer.Reset(max(time.Until(deadline), 0))
			wake = timer.C
		}
		select {
		case <-ctx.Done():
			if active != "" {
				<-done
			}
			return nil
		case now, ok := <-changes:
			if !ok {
				changes = nil
				continue
			}
			if s.cfg.Method == "live" {
				dirty.changed(now)
			}
		case <-wake:
		case result := <-done:
			active = ""
			lastWasSnapshot = result.command == "snapshot"
			wait := duration(s.cfg.Interval)
			if result.err != nil {
				wait = min(wait, time.Minute)
				s.log.Error("Operation failed; retry scheduled", "operation", result.command, "error", result.err, "retry_in", wait)
			}
			if result.command == "snapshot" {
				nextSnapshot = s.snapshotDeadline(time.Now())
				if result.err != nil {
					nextSnapshot = later(nextSnapshot, time.Now().Add(wait))
				}
			} else {
				firstLive = false
				if result.command == "backup" {
					nextBackup = time.Now().Add(wait)
				}
				if result.err != nil {
					retry = time.Now().Add(wait)
					nextBackup = retry
					pendingBackup = false
					pendingChanges = false
				} else {
					retry = time.Time{}
				}
			}
		}
	}
}
