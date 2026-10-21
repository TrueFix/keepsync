package keepsync

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

type SnapshotConfig struct {
	Enabled  bool   `json:"enabled"`
	Interval string `json:"interval"`
	KeepLast int    `json:"keep_last"`
}

type Config struct {
	Dataset      string         `json:"dataset"`
	Source       string         `json:"source"`
	Repository   string         `json:"repository"`
	PasswordFile string         `json:"password_file"`
	StateDir     string         `json:"state_dir"`
	RestoreRoot  string         `json:"restore_root"`
	Method       string         `json:"method"`
	Interval     string         `json:"interval"`
	Debounce     string         `json:"debounce"`
	MaxWait      string         `json:"max_wait"`
	MaxWatches   int            `json:"max_watches"`
	Restic       string         `json:"restic"`
	Excludes     []string       `json:"excludes"`
	DeleteRemote bool           `json:"delete_remote"`
	S3Endpoint   string         `json:"s3_endpoint"`
	LogFormat    string         `json:"log_format"`
	LogLevel     string         `json:"log_level"`
	LogFiles     bool           `json:"log_files"`
	Snapshots    SnapshotConfig `json:"snapshots"`
}

func defaultConfig() Config {
	return Config{
		Dataset: "files", Source: "/data", PasswordFile: "/run/secrets/restic_password",
		StateDir: "/var/lib/keepsync", RestoreRoot: "/restore", Method: "interval",
		Interval: "1h", Debounce: "30s", MaxWait: "5m", MaxWatches: 8192, Restic: "restic",
		LogFormat: "text", LogLevel: "info", LogFiles: true,
		Snapshots: SnapshotConfig{Interval: "24h", KeepLast: 7},
	}
}

func LoadConfig(path string) (Config, error) {
	if path == "" {
		return loadEnvironment()
	}
	c := defaultConfig()
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
	return c, c.Validate()
}

func loadEnvironment() (Config, error) {
	c := defaultConfig()
	for key, dest := range map[string]*string{
		"KEEPSYNC_DATASET": &c.Dataset, "KEEPSYNC_SOURCE_PATH": &c.Source,
		"KEEPSYNC_PASSWORD_FILE": &c.PasswordFile, "KEEPSYNC_STATE_DIR": &c.StateDir,
		"KEEPSYNC_RESTORE_ROOT": &c.RestoreRoot, "KEEPSYNC_METHOD": &c.Method,
		"KEEPSYNC_INTERVAL": &c.Interval, "KEEPSYNC_DEBOUNCE": &c.Debounce,
		"KEEPSYNC_MAX_WAIT": &c.MaxWait, "KEEPSYNC_LOG_FORMAT": &c.LogFormat,
		"KEEPSYNC_LOG_LEVEL": &c.LogLevel, "KEEPSYNC_S3_ENDPOINT": &c.S3Endpoint,
		"KEEPSYNC_SNAPSHOT_INTERVAL": &c.Snapshots.Interval,
	} {
		if v, ok := os.LookupEnv(key); ok {
			*dest = v
		}
	}
	c.Repository = os.Getenv("KEEPSYNC_TARGET")
	if c.Repository == "" {
		c.Repository = os.Getenv("KEEPSYNC_REPOSITORY")
	}
	for key, dest := range map[string]*bool{"KEEPSYNC_SNAPSHOTS_ENABLED": &c.Snapshots.Enabled, "KEEPSYNC_DELETE_REMOTE": &c.DeleteRemote, "KEEPSYNC_LOG_FILES": &c.LogFiles} {
		if v, ok := os.LookupEnv(key); ok {
			parsed, err := strconv.ParseBool(v)
			if err != nil {
				return c, fmt.Errorf("%s must be true or false", key)
			}
			*dest = parsed
		}
	}
	if v, ok := os.LookupEnv("KEEPSYNC_SNAPSHOT_RETAIN"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return c, errors.New("KEEPSYNC_SNAPSHOT_RETAIN must be a positive integer")
		}
		c.Snapshots.KeepLast = n
	}
	if v, ok := os.LookupEnv("KEEPSYNC_MAX_WATCHES"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return c, errors.New("KEEPSYNC_MAX_WATCHES must be a positive integer")
		}
		c.MaxWatches = n
	}
	if v, ok := os.LookupEnv("KEEPSYNC_EXCLUDES"); ok {
		if err := json.Unmarshal([]byte(v), &c.Excludes); err != nil {
			return c, fmt.Errorf("KEEPSYNC_EXCLUDES must be a JSON array of patterns: %w", err)
		}
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`).MatchString(c.Dataset) {
		return errors.New("dataset must be 1–64 letters, digits, underscores or hyphens")
	}
	if _, err := parseTarget(c.Repository, c.S3Endpoint); err != nil {
		return err
	}
	for name, path := range map[string]string{"source": c.Source, "state_dir": c.StateDir, "restore_root": c.RestoreRoot, "password_file": c.PasswordFile} {
		if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
			return fmt.Errorf("%s must be an absolute, non-root path", name)
		}
	}
	if overlaps(c.Source, c.StateDir) || overlaps(c.Source, c.RestoreRoot) || overlaps(c.StateDir, c.RestoreRoot) || within(c.Source, c.PasswordFile) {
		return errors.New("source, state, restore and password paths must be separated")
	}
	if c.Method != "interval" && c.Method != "live" {
		return errors.New("method must be interval or live")
	}
	for name, v := range map[string]string{"interval": c.Interval, "debounce": c.Debounce, "max_wait": c.MaxWait, "snapshot interval": c.Snapshots.Interval} {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second {
			return fmt.Errorf("%s must be a duration of at least 1s", name)
		}
	}
	if duration(c.MaxWait) < duration(c.Debounce) {
		return errors.New("max_wait must be at least debounce")
	}
	if c.Snapshots.KeepLast < 1 {
		return errors.New("snapshot keep_last must be at least 1")
	}
	if c.MaxWatches < 1 {
		return errors.New("max_watches must be at least 1")
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		return errors.New("log_format must be text or json")
	}
	if c.LogLevel != "debug" && c.LogLevel != "info" && c.LogLevel != "warn" && c.LogLevel != "error" {
		return errors.New("log_level must be debug, info, warn or error")
	}
	for _, p := range c.Excludes {
		if !doublestar.ValidatePattern(p) {
			return fmt.Errorf("invalid exclusion pattern %q", p)
		}
	}
	return nil
}

func duration(s string) time.Duration { d, _ := time.ParseDuration(s); return d }
func within(parent, child string) bool {
	r, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	return err == nil && r != ".." && !filepath.IsAbs(r) && (len(r) < 3 || r[:3] != ".."+string(filepath.Separator))
}
func overlaps(a, b string) bool { return within(a, b) || within(b, a) }
