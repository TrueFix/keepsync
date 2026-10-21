package keepsync

import (
	"bytes"
	"encoding/json"
	"errors"
)

type cappedOutput struct {
	data      []byte
	limit     int
	truncated bool
}

func (c *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := max(c.limit-len(c.data), 0)
	if len(p) > remaining {
		c.truncated = true
		p = p[:remaining]
	}
	c.data = append(c.data, p...)
	return n, nil
}
func (c *cappedOutput) String() string {
	if c.truncated {
		return string(c.data) + " (truncated)"
	}
	return string(c.data)
}

type snapshotSummary struct {
	MessageType   string `json:"message_type"`
	SnapshotID    string `json:"snapshot_id"`
	DataAdded     int64  `json:"data_added_packed"`
	OldSnapshotID string `json:"old_snapshot_id"`
	NewSnapshotID string `json:"new_snapshot_id"`
}
type snapshotOutput struct {
	line    []byte
	summary snapshotSummary
	changed snapshotSummary
	err     error
}

func (c *snapshotOutput) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		part := p
		if i >= 0 {
			part = p[:i]
		}
		if len(c.line)+len(part) > 1<<20 {
			c.err = errors.New("oversized snapshot progress record")
			c.line = nil
		} else if c.err == nil {
			c.line = append(c.line, part...)
		}
		if i < 0 {
			break
		}
		if c.err == nil {
			var summary snapshotSummary
			if json.Unmarshal(c.line, &summary) == nil && summary.MessageType == "summary" {
				c.summary = summary
			}
			if summary.MessageType == "changed" {
				c.changed = summary
			}
		}
		c.line = c.line[:0]
		p = p[i+1:]
	}
	return n, nil
}
