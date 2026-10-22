// Measure the actual sidecar process after an initial upload to a local S3
// emulator. The emulator and fixture generator run outside the measured child.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	binary := flag.String("binary", "bin/keepsync", "sidecar executable")
	files := flag.Int("files", 1000, "number of 1 KiB files")
	idle := flag.Duration("idle", 5*time.Second, "idle sampling duration")
	flag.Parse()
	if runtime.GOOS != "linux" {
		return fmt.Errorf("resource measurement uses Linux /proc")
	}
	if *files < 1 || *idle < time.Second {
		return fmt.Errorf("files must be positive and idle must be at least 1s")
	}
	executable, err := filepath.Abs(*binary)
	if err != nil {
		return err
	}
	root, err := os.MkdirTemp("", "keepsync-resource-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	source, state := filepath.Join(root, "data"), filepath.Join(root, "state")
	if err := os.MkdirAll(source, 0700); err != nil {
		return err
	}
	for i := 0; i < *files; i++ {
		if err := os.WriteFile(filepath.Join(source, fmt.Sprintf("file-%05d.json", i)), []byte(strings.Repeat("x", 1024)), 0600); err != nil {
			return err
		}
	}
	backend := s3mem.New()
	if err := backend.CreateBucket("backups"); err != nil {
		return err
	}
	server := httptest.NewServer(gofakes3.New(backend).Server())
	defer server.Close()
	env := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "KEEPSYNC_") && !strings.HasPrefix(v, "AWS_") && !strings.HasPrefix(v, "GOMAXPROCS=") && !strings.HasPrefix(v, "GOMEMLIMIT=") {
			env = append(env, v)
		}
	}
	env = append(env, "KEEPSYNC_TARGET=s3://backups/resources", "KEEPSYNC_S3_ENDPOINT="+server.URL, "KEEPSYNC_SOURCE_PATH="+source, "KEEPSYNC_STATE_DIR="+state, "KEEPSYNC_RESTORE_ROOT="+filepath.Join(root, "restore"), "KEEPSYNC_METHOD=live", "KEEPSYNC_INTERVAL=1h", "KEEPSYNC_SNAPSHOTS_ENABLED=false", "KEEPSYNC_LOG_FILES=false", "AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test", "AWS_REGION=us-east-1", "GOMAXPROCS=1", "GOMEMLIMIT=64MiB")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "serve")
	cmd.Env = env
	log, err := os.Create(filepath.Join(root, "process.log"))
	if err != nil {
		return err
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var status struct {
			LastSuccess time.Time `json:"last_success"`
			LastError   string    `json:"last_error"`
		}
		if data, err := os.ReadFile(filepath.Join(state, "status.json")); err == nil && json.Unmarshal(data, &status) == nil && !status.LastSuccess.IsZero() && status.LastError == "" {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("initial upload did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	before, err := ticks(cmd.Process.Pid)
	if err != nil {
		return err
	}
	start := time.Now()
	time.Sleep(*idle)
	elapsed := time.Since(start)
	after, err := ticks(cmd.Process.Pid)
	if err != nil {
		return err
	}
	rss, err := resident(cmd.Process.Pid)
	if err != nil {
		return err
	}
	hz := 100.0
	if raw, err := exec.Command("getconf", "CLK_TCK").Output(); err == nil {
		if n, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64); err == nil {
			hz = n
		}
	}
	info, err := os.Stat(executable)
	if err != nil {
		return err
	}
	fmt.Printf("Files: %d × 1 KiB; snapshots disabled; one watched directory\n", *files)
	fmt.Printf("Executable: %.2f MiB\nIdle RSS: %.2f MiB\nIdle CPU: %.3f%% over %s\n", float64(info.Size())/(1<<20), float64(rss)/1024, float64(after-before)/hz/elapsed.Seconds()*100, elapsed.Round(time.Millisecond))
	return nil
}
func ticks(pid int) (uint64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	text := string(raw)
	i := strings.LastIndex(text, ") ")
	if i < 0 {
		return 0, io.ErrUnexpectedEOF
	}
	fields := strings.Fields(text[i+2:])
	if len(fields) < 13 {
		return 0, io.ErrUnexpectedEOF
	}
	u, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, err
	}
	s, err := strconv.ParseUint(fields[12], 10, 64)
	return u + s, err
}
func resident(pid int) (uint64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			return strconv.ParseUint(fields[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("RSS not available")
}
