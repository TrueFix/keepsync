package keepsync

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCompactColoredTextLogs(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	c := defaultConfig()
	var out bytes.Buffer
	s := New(c, &out, &out)
	s.log.Info("KeepSync started", "method", "live", "source", "/data", "debounce", "2s")
	s.log.Info("FILE uploaded", "path", "FILEA.JSON", "change", "modified", "old_bytes", 1125, "bytes", 5500)
	s.log.Info("FILE removed locally", "path", "old.json", "remote_deleted", false)
	text := out.String()
	for _, wanted := range []string{"\x1b[", "WATCH", "EDIT", "FILEA.JSON", "1.1K → 5.4K uploaded", "DEL", "kept on S3"} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("missing %q in %q", wanted, text)
		}
	}
	for _, noise := range []string{"time=", "level=", "msg=", "old_bytes=", "remote_deleted="} {
		if strings.Contains(text, noise) {
			t.Fatalf("verbose field %q remains", noise)
		}
	}
	if strings.Count(text, "\n") != 3 {
		t.Fatal("logs are not one line per event")
	}
}

func TestColorCanBeDisabledAndJSONStaysStructured(t *testing.T) {
	for _, mode := range []string{"setting", "no_color", "json"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("NO_COLOR", "")
			c := defaultConfig()
			if mode == "setting" {
				c.LogColor = false
			}
			if mode == "no_color" {
				t.Setenv("NO_COLOR", "1")
			}
			if mode == "json" {
				c.LogFormat = "json"
			}
			var out bytes.Buffer
			s := New(c, &out, &out)
			s.log.Info("FILE uploaded", "path", "file.json", "change", "added", "bytes", 50)
			if strings.Contains(out.String(), "\x1b[") {
				t.Fatal("unexpected ANSI sequences")
			}
			if mode == "json" {
				var record map[string]any
				if err := json.Unmarshal(out.Bytes(), &record); err != nil {
					t.Fatal(err)
				}
				if record["bytes"] != float64(50) || record["path"] != "file.json" {
					t.Fatal("structured attributes lost")
				}
			}
		})
	}
}

func TestTextLogsEscapeUntrustedPaths(t *testing.T) {
	c := defaultConfig()
	c.LogColor = false
	var out bytes.Buffer
	s := New(c, &out, &out)
	s.log.Info("FILE uploaded", "path", "bad\x1b[31m\nname.json", "change", "added", "bytes", 1)
	if strings.Contains(out.String(), "\x1b") || strings.Count(out.String(), "\n") != 1 {
		t.Fatal("filename injected terminal controls or lines")
	}
}
