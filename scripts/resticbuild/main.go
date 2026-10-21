// resticbuild trims a temporary source copy without modifying the module cache.
// Builds and scans therefore use the same S3/local-only source graph.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	output := flag.String("output", "bin/restic", "output executable")
	action := flag.String("action", "build", "build, scan or test")
	goos := flag.String("os", runtime.GOOS, "target OS")
	arch := flag.String("arch", runtime.GOARCH, "target architecture")
	flag.Parse()
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	module := filepath.Join(root, "tools", "restic")
	if err := command(module, nil, "mod", "download", "github.com/restic/restic"); err != nil {
		return err
	}
	cmd := exec.Command("go", "-C", module, "list", "-m", "-f", "{{.Dir}}", "github.com/restic/restic")
	raw, err := cmd.Output()
	if err != nil {
		return err
	}
	upstream := strings.TrimSpace(string(raw))
	if upstream == "" {
		return fmt.Errorf("Restic source directory was not resolved")
	}
	tmp, err := os.MkdirTemp("", "keepsync-restic-build-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0700); err != nil {
		return err
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		b, err := os.ReadFile(filepath.Join(module, name))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(work, name), b, 0600); err != nil {
			return err
		}
	}
	// A relative replacement keeps build metadata independent of temporary paths.
	copyDir := filepath.Join(work, "upstream")
	if err := os.CopyFS(copyDir, os.DirFS(upstream)); err != nil {
		return err
	}
	if err := command(work, nil, "mod", "edit", "-replace=github.com/restic/restic=./upstream"); err != nil {
		return err
	}
	for dest, src := range map[string]string{
		filepath.Join(copyDir, "internal/backend/all/all.go"): filepath.Join(module, "backend_all.go.txt"),
		filepath.Join(copyDir, "cmd/restic/cmd_mount.go"):     filepath.Join(module, "cmd_mount.go.txt"),
	} {
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.Chmod(dest, 0644); err != nil {
			return err
		}
		if err := os.WriteFile(dest, b, 0644); err != nil {
			return err
		}
	}
	env := []string{"CGO_ENABLED=0", "GOOS=" + *goos, "GOARCH=" + *arch, "GOFLAGS=-mod=readonly"}
	switch *action {
	case "build":
		dest, err := filepath.Abs(*output)
		if err != nil {
			return err
		}
		return command(work, env, "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", dest, "github.com/restic/restic/cmd/restic")
	case "scan":
		return command(work, env, "run", "golang.org/x/vuln/cmd/govulncheck@v1.9.0", "github.com/restic/restic/cmd/restic")
	case "test":
		return command(work, env, "test", "github.com/restic/restic/internal/backend/s3", "github.com/restic/restic/internal/repository", "github.com/restic/restic/internal/restorer")
	default:
		return fmt.Errorf("unknown action %q", *action)
	}
}

func command(dir string, overrides []string, args ...string) error {
	cmd := exec.Command("go", append([]string{"-C", dir}, args...)...)
	for _, v := range os.Environ() {
		key, _, _ := strings.Cut(v, "=")
		replace := false
		for _, override := range overrides {
			if strings.HasPrefix(override, key+"=") {
				replace = true
				break
			}
		}
		if !replace {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, overrides...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
