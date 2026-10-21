package keepsync

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

type compactHandler struct {
	out   io.Writer
	mu    *sync.Mutex
	level slog.Level
	color bool
	attrs []slog.Attr
	group string
}

func newCompactHandler(out io.Writer, c Config, level slog.Level) slog.Handler {
	return &compactHandler{out: out, mu: &sync.Mutex{}, level: level, color: c.LogColor && os.Getenv("NO_COLOR") == ""}
}
func (h *compactHandler) Enabled(_ context.Context, level slog.Level) bool { return level >= h.level }
func (h *compactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &next
}
func (h *compactHandler) WithGroup(group string) slog.Handler {
	next := *h
	if group != "" {
		if next.group != "" {
			next.group += "."
		}
		next.group += group
	}
	return &next
}
func (h *compactHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]slog.Value, len(h.attrs)+r.NumAttrs())
	var add func(slog.Attr)
	add = func(a slog.Attr) {
		a.Value = a.Value.Resolve()
		if a.Value.Kind() == slog.KindGroup {
			for _, child := range a.Value.Group() {
				add(child)
			}
			return
		}
		attrs[a.Key] = a.Value
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(func(a slog.Attr) bool { add(a); return true })
	str := func(key string) string {
		if value, ok := attrs[key]; ok {
			return fmt.Sprint(value.Any())
		}
		return ""
	}
	num := func(key string) int64 {
		if value, ok := attrs[key]; ok && value.Kind() == slog.KindInt64 {
			return value.Int64()
		}
		return 0
	}
	label, text, color := "INFO", r.Message, "36"
	switch r.Message {
	case "KeepSync started":
		label = "WATCH"
		if str("method") != "live" {
			label = "READY"
		}
		text = str("source") + " · " + str("method")
		if str("method") == "live" {
			text += " · quiet " + str("debounce")
		} else {
			text += " · every " + str("interval")
		}
	case "FILE uploaded":
		label, text, color = "ADD", logText(str("path"))+" · "+shortBytes(num("bytes")), "32"
		if str("change") == "modified" {
			label = "EDIT"
			color = "36"
			text = logText(str("path")) + " · " + shortBytes(num("old_bytes")) + " → " + shortBytes(num("bytes"))
		}
		text += " uploaded"
	case "FILE removed locally":
		label, color = "DEL", "33"
		text = logText(str("path"))
		if str("remote_deleted") == "true" {
			text += " · removed from S3"
		} else {
			text += " · kept on S3"
		}
	case "Live backup started":
		label, text = "SCAN", "checking files"
	case "Live backup complete":
		label, color = "SYNC", "32"
		text = fmt.Sprintf("%d uploaded · %s · %s", num("uploaded_files"), shortBytes(num("uploaded_bytes")), str("duration"))
		if num("removed_files") > 0 {
			text += fmt.Sprintf(" · %d removed", num("removed_files"))
		}
	case "Snapshot started":
		label, color = "SNAP", "35"
		text = "creating " + str("format")
	case "Snapshot saved":
		label, color = "SNAP", "35"
		id := str("snapshot")
		if len(id) == 64 {
			id = id[:8]
		}
		text = id
		size := num("bytes")
		if size == 0 {
			size = num("new_data_bytes")
		}
		text += " · " + shortBytes(size)
	case "Snapshot retention complete":
		label, color = "PRUNE", "33"
		text = fmt.Sprintf("%d expired · keep %d", num("removed"), num("keep_last"))
	case "Operation failed; retry scheduled":
		label, color = "ERROR", "31"
		text = str("operation") + ": " + str("error") + " · retry " + str("retry_in")
	case "Waiting for source files":
		label, color = "WAIT", "33"
		text = str("source") + " · waiting for files"
	case "Waiting for keepsync state":
		label, color = "WAIT", "33"
		text = "run `keepsync reclaim` to reattach · " + str("target")
	case "Source initialized automatically":
		label, text = "SETUP", str("source")
	case "Backup target reclaimed":
		label, color = "SETUP", "32"
		text = str("source") + " · state rebuilt from remote identity"
	case "Initializing encrypted snapshot repository":
		label, text = "INIT", "encrypted snapshots"
	case "Archive restored":
		label, text, color = "RESTORE", str("target"), "32"
	case "Archive integrity check complete":
		label, text, color = "CHECK", fmt.Sprintf("%d snapshots OK", num("snapshots")), "32"
	case "Recovered source identity verified":
		label, text = "SOURCE", str("source")+" · recovery verified"
	case "Live change detected":
		label, text = "EVENT", str("change")+" "+logText(str("path"))
	case "Filesystem change":
		label, text = "EVENT", strings.ToLower(str("operation"))+" "+logText(str("path"))
	case "Metadata polling enabled":
		label, text = "WATCH", "poll every "+str("interval")
	case "Waiting for file writes":
		label, text = "WAIT", "file still changing · retry "+str("retry_in")
	default:
		if r.Level >= slog.LevelError {
			label, color = "ERROR", "31"
		} else if r.Level >= slog.LevelWarn {
			label, color = "WARN", "33"
		} else if r.Level < slog.LevelInfo {
			label, color = "DEBUG", "90"
		}
		if str("error") != "" {
			text += " · " + str("error")
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, text)
	if len(text) > 320 {
		text = string([]rune(text)[:min(317, len([]rune(text)))]) + "..."
	}
	if h.group != "" {
		text = h.group + ": " + text
	}
	timestamp := r.Time.Format("15:04:05")
	line := fmt.Sprintf("%s %-7s %s\n", timestamp, label, text)
	if h.color {
		line = fmt.Sprintf("\x1b[90m%s\x1b[0m \x1b[%sm%-7s\x1b[0m %s\n", timestamp, color, label, text)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.out, line)
	return err
}

func logText(text string) string {
	for _, r := range text {
		if unicode.IsControl(r) {
			return strconv.Quote(text)
		}
	}
	return text
}
func shortBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	value := float64(n)
	units := []string{"K", "M", "G", "T"}
	unit := "B"
	for _, u := range units {
		value /= 1024
		unit = u
		if value < 1024 {
			break
		}
	}
	if value < 10 {
		return fmt.Sprintf("%.1f%s", value, unit)
	}
	return fmt.Sprintf("%.0f%s", value, unit)
}
