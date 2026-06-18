// Package jobs surfaces clown's job-wakeup channel (clown RFC-0009, the
// job-output spool/status probe from RFC-0010) in the trapeze UI.
//
// The channel has a two-layer design — a durable on-disk JSONL journal
// (the at-least-once source of truth) plus a lossy unix-datagram nudge
// for sub-second latency — and trapeze consumes it the way a phone
// consumes APNs: the datagram is a best-effort push that says "something
// changed", and the journal is reconciled on every push (and on a slow
// ticker, so a lost datagram only delays, never loses, an update).
//
// Trapeze is a *display surface* for the channel, not the wake monitor:
// it never writes ack cursors (those belong to `clown job-watch`, which
// uses them to decide what to replay to the agent) and it never appends
// records. If a clown monitor already owns the channel's nudge socket,
// trapeze degrades to ticker-only polling.
//
// This file is the on-disk/wire contract, byte-compatible with clown's
// internal/jobwake (RFC-0009 §§2-6).
package jobs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// SchemaVersion is the journal record / nudge wire version (RFC-0009
// §4, §6).
const SchemaVersion = 1

// Event types (RFC-0009 §5). The four terminal types plus "message"
// wake the session monitor; "started"/"progress" are journal-only.
const (
	TypeStarted     = "started"
	TypeProgress    = "progress"
	TypeSucceeded   = "succeeded"
	TypeFailed      = "failed"
	TypeCancelled   = "cancelled"
	TypeInterrupted = "interrupted"
	TypeMessage     = "message"
)

// StateRunning is the derived state of a job whose journal has no
// terminal record yet. It is not a record type.
const StateRunning = "running"

// IsTerminal reports whether t is one of the four terminal record
// types.
func IsTerminal(t string) bool {
	switch t {
	case TypeSucceeded, TypeFailed, TypeCancelled, TypeInterrupted:
		return true
	}
	return false
}

// Record is one journal line (RFC-0009 §4).
type Record struct {
	V         int    `json:"v"`
	Job       string `json:"job"`
	Session   string `json:"session"`
	Source    string `json:"source"`
	From      string `json:"from,omitempty"`
	Type      string `json:"type"`
	Seq       int    `json:"seq"`
	TS        string `json:"ts"`
	Message   string `json:"message,omitempty"`
	ResultRef string `json:"result_ref,omitempty"`
}

// Time parses the record timestamp (RFC3339Nano). Returns the zero
// time for malformed values.
func (r Record) Time() time.Time {
	t, err := time.Parse(time.RFC3339Nano, r.TS)
	if err != nil {
		return time.Time{}
	}
	return t
}

// SessionKey resolves the session key for this process per RFC-0009
// §2: CLOWN_SESSION_ID, then SPINCLASS_SESSION_ID, then
// CLAUDE_SESSION_ID, then a freshly generated random 128-bit hex key.
func SessionKey() string {
	for _, name := range []string{"CLOWN_SESSION_ID", "SPINCLASS_SESSION_ID", "CLAUDE_SESSION_ID"} {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to something process-unique; the channel just
		// stays empty for an unknown key.
		return fmt.Sprintf("trapeze-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// ChannelID hashes a session key to its channel id: the first 16 bytes
// of SHA-256(key), lowercase hex (32 chars).
func ChannelID(sessionKey string) string {
	sum := sha256.Sum256([]byte(sessionKey))
	return hex.EncodeToString(sum[:16])
}

// jobsRoot returns the journal root, $XDG_STATE_HOME/clown/jobs
// (default ~/.local/state/clown/jobs).
func jobsRoot() string {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "clown", "jobs")
}

// JournalDir returns the per-channel journal directory.
func JournalDir(channelID string) string {
	root := jobsRoot()
	if root == "" {
		return ""
	}
	return filepath.Join(root, channelID)
}

// SocketPath returns the channel's nudge socket path:
// $XDG_RUNTIME_DIR/<cid>.sock, or $TMPDIR/clown-jobs-<uid>/<cid>.sock
// when XDG_RUNTIME_DIR is unset.
func SocketPath(channelID string) string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("clown-jobs-%d", os.Getuid()))
	}
	return filepath.Join(dir, channelID+".sock")
}

// readJournal parses one job's JSONL journal file, skipping malformed
// lines (a truncated tail write must not hide the rest of the job).
func readJournal(path string) []Record {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var records []Record
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if r.V != SchemaVersion || r.Job == "" {
			continue
		}
		records = append(records, r)
	}
	slices.SortStableFunc(records, func(a, b Record) int { return a.Seq - b.Seq })
	return records
}
