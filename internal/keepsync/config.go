package keepsync

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

type Config struct {
	Dataset      string   `json:"dataset"`
	Source       string   `json:"source"`
	Repository   string   `json:"repository"`
	PasswordFile string   `json:"password_file"`
	StateDir     string   `json:"state_dir"`
	RestoreRoot  string   `json:"restore_root"`
	Interval     string   `json:"interval"`
	Restic       string   `json:"restic"`
	Excludes     []string `json:"excludes"`
	Retention    struct {
		Enabled     bool `json:"enabled"`
		KeepDaily   int  `json:"keep_daily"`
		KeepWeekly  int  `json:"keep_weekly"`
		KeepMonthly int  `json:"keep_monthly"`
	} `json:"retention"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("decode configuration: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, errors.New("configuration must contain one JSON object")
	}
	if c.Restic == "" {
		c.Restic = "restic"
	}
	if c.Interval == "" {
		c.Interval = "1h"
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`).MatchString(c.Dataset) {
		return errors.New("dataset must be 1–64 letters, digits, underscores or hyphens")
	}
	if c.Repository == "" {
		return errors.New("repository is required")
	}
	for name, path := range map[string]string{"source": c.Source, "state_dir": c.StateDir, "restore_root": c.RestoreRoot, "password_file": c.PasswordFile} {
		if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
			return fmt.Errorf("%s must be an absolute, non-root path", name)
		}
	}
	if overlaps(c.Source, c.StateDir) || overlaps(c.Source, c.RestoreRoot) || overlaps(c.StateDir, c.RestoreRoot) || within(c.Source, c.PasswordFile) {
		return errors.New("source, state, restore and password paths must be separated")
	}
	d, err := time.ParseDuration(c.Interval)
	if err != nil || d < time.Second {
		return errors.New("interval must be a duration of at least 1s")
	}
	r := c.Retention
	if r.KeepDaily < 0 || r.KeepWeekly < 0 || r.KeepMonthly < 0 || (r.Enabled && r.KeepDaily+r.KeepWeekly+r.KeepMonthly == 0) {
		return errors.New("enabled retention requires at least one positive keep count; counts cannot be negative")
	}
	return nil
}

func within(parent, child string) bool {
	r, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	return err == nil && r != ".." && !filepath.IsAbs(r) && (len(r) < 3 || r[:3] != ".."+string(filepath.Separator))
}

func overlaps(a, b string) bool { return within(a, b) || within(b, a) }
