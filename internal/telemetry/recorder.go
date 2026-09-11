package telemetry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"relay/internal/keychain"
	"relay/internal/paths"
)

const (
	// fileName is the single JSONL stream. One line per invocation keeps the
	// format append-only and cheap to tail (spec §31).
	fileName = "usage.jsonl"

	// defaultMaxBytes bounds the live file. On reaching it the recorder rotates
	// the live file to the backup path, so telemetry can never grow without
	// limit. One MiB holds roughly ten thousand events, far more than the MVP
	// needs to surface common workflows, while staying small enough to read
	// whole.
	defaultMaxBytes = 1 << 20

	fileMode = 0o600
	dirMode  = 0o700
)

// Event is a single capability invocation (spec §31, §32, §57). Its shape is
// deliberately narrow: it has no field for input values, request bodies,
// responses, or headers, so a caller cannot accidentally record a secret by
// stashing a payload somewhere in it. Only the identity of the call and how it
// behaved are kept. Everything added to this struct must remain non-sensitive.
type Event struct {
	Tool       string    `json:"tool"`
	Operation  string    `json:"operation"`
	Timestamp  time.Time `json:"timestamp"`
	DurationMs int64     `json:"durationMs"`
	Success    bool      `json:"success"`
	ErrorCode  string    `json:"errorCode,omitempty"`
}

// Dir is the telemetry directory under the Relay home (spec §32).
func Dir(layout paths.Layout) string { return filepath.Join(layout.Root, "telemetry") }

// File is the live JSONL stream. It lives beside the backup handled by rotation.
func File(layout paths.Layout) string { return filepath.Join(Dir(layout), fileName) }

// Backup is the rotated predecessor of File, kept so recent history survives a
// rotation without an unbounded chain of files.
func Backup(layout paths.Layout) string {
	return filepath.Join(Dir(layout), "usage.1.jsonl")
}

// Recorder appends Events as JSONL to the telemetry file (spec §31). It is safe
// for concurrent use: a mutex serializes each encode-and-append so lines can
// never interleave or be torn. The daemon is concurrent, so this is a hard
// requirement rather than an optimization.
//
// Growth is bounded by rotation: once the live file would exceed maxBytes it is
// renamed to the backup path and a fresh live file is started, so on-disk usage
// is at most two files. A single event larger than the cap is still written
// whole, because splitting or dropping it would corrupt the JSONL stream.
type Recorder struct {
	mu       sync.Mutex
	dir      string
	path     string
	backup   string
	writer   io.Writer // non-nil when injected: no file, no rotation
	file     *os.File
	written  int64
	maxBytes int64
	now      func() time.Time
	secrets  []string
}

// New returns a Recorder writing to the Relay layout’s telemetry file. The file
// is opened lazily on first Record so constructing a recorder cannot fail an
// invocation; secrets are optional and passed to keychain.Redact as defence in
// depth (spec §32).
func New(layout paths.Layout, secrets ...string) *Recorder {
	return &Recorder{
		dir:      Dir(layout),
		path:     File(layout),
		backup:   Backup(layout),
		maxBytes: defaultMaxBytes,
		now:      time.Now,
		secrets:  secrets,
	}
}

// NewWriter returns a Recorder appending to w. It is the injection seam tests
// use to drive the recorder without touching the filesystem; because the caller
// owns w, rotation is disabled.
func NewWriter(w io.Writer, secrets ...string) *Recorder {
	return &Recorder{writer: w, maxBytes: defaultMaxBytes, now: time.Now, secrets: secrets}
}

// Record appends one Event as a JSONL line. Timestamp is filled from the clock
// when zero. Redaction runs over the free-form string fields so a stray secret
// is scrubbed even on this narrow surface.
//
// Record returns an error so tests and diagnostics can observe failures, but the
// contract is best-effort: telemetry must never fail the invocation it observes.
// Callers should discard the error and continue normally.
func (r *Recorder) Record(e Event) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = r.now()
	}
	e.Tool = r.sanitize(e.Tool)
	e.Operation = r.sanitize(e.Operation)
	e.ErrorCode = r.sanitize(e.ErrorCode)

	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.writer != nil {
		_, err := r.writer.Write(line)
		return err
	}
	if err := r.ensureOpen(); err != nil {
		return err
	}
	if r.written > 0 && r.written+int64(len(line)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return err
		}
		if err := r.ensureOpen(); err != nil {
			return err
		}
	}
	n, err := r.file.Write(line)
	r.written += int64(n)
	return err
}

// Close releases the owned file. It is a no-op for injected writers and for a
// recorder that never opened its file.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

// ensureOpen opens the live file in append mode and records its current size so
// the rotation bound accounts for bytes written by a previous process.
func (r *Recorder) ensureOpen() error {
	if r.file != nil {
		return nil
	}
	if err := os.MkdirAll(r.dir, dirMode); err != nil {
		return err
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.file = f
	r.written = info.Size()
	return nil
}

// rotate moves the live file aside and resets the size counter. Rename replaces
// any existing backup, so a full live file plus one backup is the hard ceiling.
func (r *Recorder) rotate() error {
	if r.file != nil {
		if err := r.file.Close(); err != nil {
			r.file = nil
			return err
		}
		r.file = nil
	}
	r.written = 0
	if _, err := os.Stat(r.path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.Rename(r.path, r.backup)
}

// sanitize scrubs any configured secret from a free-form field. It is defence in
// depth: the Event type already excludes payloads, but a caller could still put
// a credential into an operation name (spec §22, §54).
func (r *Recorder) sanitize(s string) string {
	if s == "" || len(r.secrets) == 0 {
		return s
	}
	return keychain.Redact(s, r.secrets...)
}

// ReadEvents decodes a JSONL stream back into Events. Blank lines are skipped.
//
// A complete but malformed line is an error: that is genuine corruption and
// should be visible rather than silently dropped. The one exception is a
// trailing fragment with no terminating newline. Record emits each event as a
// single complete line, so an unterminated final line can only be a write that
// is still in flight -- what a reader sees when it overlaps an append, or a
// rotation that has just moved the file being read. Skipping that fragment lets
// a read succeed against a live writer without hiding real damage: a malformed
// line that is newline-terminated (so no longer the tail) still fails the read,
// and a genuine I/O error is still returned.
func ReadEvents(rd io.Reader) ([]Event, error) {
	var out []Event
	br := bufio.NewReaderSize(rd, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			body := line[:len(line)-1]
			if len(bytes.TrimSpace(body)) != 0 {
				var e Event
				if jerr := json.Unmarshal(body, &e); jerr != nil {
					return nil, jerr
				}
				out = append(out, e)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// An unterminated trailing fragment is a write in flight; the
				// events before it are complete and are returned as-is.
				return out, nil
			}
			return nil, err
		}
	}
}
