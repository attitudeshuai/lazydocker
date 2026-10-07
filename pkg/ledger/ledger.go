// Package ledger implements the operation ledger: a single, durable record
// outlet for every mutating operation lazydocker performs against docker
// objects, regardless of whether the operation went through the docker API
// (SDK) or through a subprocess (custom commands, bulk commands, compose
// wrappers, attach/exec shells, ...).
//
// The ledger stores one JSON object per line in an append-only file and keeps
// an in-memory copy of all surviving records for querying. Writes are
// serialised by a mutex (so concurrent operations stay ordered and no record
// is interleaved/lost), each line is fsync'd, and reads return point-in-time
// snapshots so a reader can never observe a half-written record.
//
// The file rolls over on both a size and an age limit: records older than the
// age limit are dropped, then the oldest records are dropped until the file
// fits the size limit. Rewrites are atomic (temp file + rename in the same
// directory).
//
// A disabled ledger performs no I/O whatsoever and every recording helper is
// a no-op, leaving the rest of the application behaving exactly as before.
package ledger

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FileName is the ledger file name inside the configured config directory.
const FileName = "ledger.log"

// DefaultMaxSize is the default on-disk size budget for the ledger file (1 MiB).
const DefaultMaxSize int64 = 1 << 20

// DefaultMaxAge is the default retention period for records (30 days).
const DefaultMaxAge = 30 * 24 * time.Hour

// PathKind identifies which execution path an operation took.
type PathKind string

const (
	// PathAPI is the docker SDK / API path.
	PathAPI PathKind = "api"
	// PathProcess is the subprocess (shell command) path.
	PathProcess PathKind = "process"
)

// ObjectKind identifies the type of object an operation targeted.
type ObjectKind string

const (
	ObjectContainer ObjectKind = "container"
	ObjectImage     ObjectKind = "image"
	ObjectVolume    ObjectKind = "volume"
	ObjectNetwork   ObjectKind = "network"
	ObjectService   ObjectKind = "service"
	ObjectProject   ObjectKind = "project"
)

// Target identifies the object an operation ran against. Not all fields are
// populated for every operation (e.g. prunes carry only Kind).
type Target struct {
	// Kind is the object type ("container", "image", ...).
	Kind ObjectKind `json:"kind,omitempty"`
	// ID is the durable object identifier (full container id, volume name, ...).
	ID string `json:"id,omitempty"`
	// Name is the human readable object name at the time of the operation.
	Name string `json:"name,omitempty"`
	// Project is the compose project the object belonged to, if any.
	Project string `json:"project,omitempty"`
	// Service is the compose service the object belonged to, if any.
	Service string `json:"service,omitempty"`
}

// Entry is a single immutable ledger record.
type Entry struct {
	// Seq is the monotonically increasing, gap-free sequence number assigned
	// when the record is committed. It defines record order.
	Seq int64 `json:"seq"`

	// BatchID groups records that belong to the same bulk operation. Records
	// within a batch are ordered with BatchIndex/BatchTotal so the per-item
	// results of e.g. "stop all containers" can be retrieved together.
	BatchID    string `json:"batchId,omitempty"`
	BatchIndex int    `json:"batchIndex,omitempty"`
	BatchTotal int    `json:"batchTotal,omitempty"`

	// Path is "api" or "process".
	Path PathKind `json:"path"`
	// Action is what was attempted, e.g. "container.remove" or "bulk-command".
	Action string `json:"action"`

	// Target describes the operated object.
	Target Target `json:"target"`

	// Connection is the docker host/endpoint the operation used.
	Connection string `json:"connection,omitempty"`

	// StartedAt/EndedAt bracket the operation. DurationMS is the delta.
	StartedAt  time.Time `json:"startedAt"`
	EndedAt    time.Time `json:"endedAt"`
	DurationMS int64     `json:"durationMs"`

	// Success is true when the operation returned no error.
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`

	// Command is the raw command line (process path only).
	Command string `json:"command,omitempty"`
	// ExitCode is the process exit code (process path only). It is -1 when the
	// process could not be started or was killed by a signal. It stays nil for
	// API operations.
	ExitCode *int `json:"exitCode,omitempty"`
}

// Filter selects ledger records. Empty fields are ignored.
type Filter struct {
	// ObjectKind filters by Target.Kind.
	ObjectKind ObjectKind
	// ObjectID filters by Target.ID.
	ObjectID string
	// Project filters by Target.Project.
	Project string
	// Service filters by Target.Service.
	Service string
	// BatchID returns every item of one bulk operation.
	BatchID string
	// Since/Until bracket StartedAt (inclusive). Zero values are unbounded.
	Since time.Time
	Until time.Time
}

func (f Filter) matches(e Entry) bool {
	if f.ObjectKind != "" && e.Target.Kind != f.ObjectKind {
		return false
	}
	if f.ObjectID != "" && e.Target.ID != f.ObjectID {
		return false
	}
	if f.Project != "" && e.Target.Project != f.Project {
		return false
	}
	if f.Service != "" && e.Target.Service != f.Service {
		return false
	}
	if f.BatchID != "" && e.BatchID != f.BatchID {
		return false
	}
	if !f.Since.IsZero() && e.StartedAt.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && e.StartedAt.After(f.Until) {
		return false
	}
	return true
}

// Config configures a Ledger.
type Config struct {
	// Enabled turns the ledger on. When false, New performs no I/O and all
	// recording methods are no-ops.
	Enabled bool
	// Dir is the directory holding the ledger file (typically the lazydocker
	// config directory).
	Dir string
	// MaxSize bounds the on-disk file size in bytes; 0 means DefaultMaxSize.
	MaxSize int64
	// MaxAge bounds how long records are retained; 0 means DefaultMaxAge.
	MaxAge time.Duration
	// Connection is the docker host/endpoint recorded on every entry. It can be
	// refined later via Ledger.SetConnection once the host has been resolved.
	Connection string
	// Now is the clock used by the ledger (overridable in tests).
	Now func() time.Time
}

// Ledger is the operation ledger. The zero value is usable as a disabled
// ledger; all methods are nil-safe so callers do not need nil checks.
type Ledger struct {
	cfg Config

	mu       sync.Mutex
	file     *os.File
	path     string
	size     int64
	seq      int64
	entries  []Entry
	closed   bool
	conn     string
	batchSeq uint64
}

// New opens (or creates) the ledger for cfg and replays any previously
// persisted records. When cfg.Enabled is false a disabled ledger is returned
// without touching the file system. Startup/parse failures are returned so the
// caller can report them explicitly instead of silently losing records.
func New(cfg Config) (*Ledger, error) {
	if !cfg.Enabled {
		return &Ledger{cfg: cfg, closed: true}, nil
	}

	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = DefaultMaxSize
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = DefaultMaxAge
	}
	if cfg.Dir == "" {
		return nil, errors.New("ledger: directory is required")
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("ledger: create directory %q: %w", cfg.Dir, err)
	}

	l := &Ledger{
		cfg:  cfg,
		path: filepath.Join(cfg.Dir, FileName),
		conn: cfg.Connection,
	}

	if err := l.replay(); err != nil {
		return nil, err
	}
	if err := l.openFile(); err != nil {
		return nil, err
	}
	// Apply retention rules to whatever was replayed from disk.
	l.mu.Lock()
	rotateErr := l.rotateLocked()
	l.mu.Unlock()
	if rotateErr != nil {
		return nil, rotateErr
	}

	return l, nil
}

// Enabled reports whether the ledger records operations.
func (l *Ledger) Enabled() bool {
	return l != nil && l.cfg.Enabled && !l.closed
}

// SetConnection records the docker host/endpoint that subsequent operations
// use. Safe to call on a disabled ledger.
func (l *Ledger) SetConnection(host string) {
	if l == nil || !l.cfg.Enabled {
		return
	}
	l.mu.Lock()
	l.conn = host
	l.mu.Unlock()
}

// Close flushes and closes the underlying file. Safe to call on a disabled
// ledger or more than once.
func (l *Ledger) Close() error {
	if l == nil || !l.cfg.Enabled {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// NewBatchID returns a unique identifier for one bulk operation. All per-item
// records of that bulk operation share the id, with ascending BatchIndex
// values. Returns "" on a disabled ledger.
func (l *Ledger) NewBatchID() string {
	if l == nil || !l.Enabled() {
		return ""
	}
	l.mu.Lock()
	l.batchSeq++
	seq := l.batchSeq
	l.mu.Unlock()

	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("b-%d-%d-%s", l.cfg.Now().UnixNano(), seq, hex.EncodeToString(b))
}

// Start opens a recorded operation. StartedAt is captured immediately so the
// caller can run the operation afterwards. The returned *Operation is safe to
// use on a disabled ledger (all of its methods, including Run/FinishProcess,
// are no-ops).
func (l *Ledger) Start(path PathKind, action string) *Operation {
	// Note: we gate on configuration here, not on Enabled(): a ledger that was
	// enabled but has since been closed must still produce an explicit write
	// error rather than silently swallowing the record.
	if l == nil || !l.cfg.Enabled {
		return &Operation{}
	}
	now := l.cfg.Now()
	l.mu.Lock()
	conn := l.conn
	l.mu.Unlock()
	return &Operation{
		l: l,
		e: Entry{
			Path:       path,
			Action:     action,
			Connection: conn,
			StartedAt:  now,
			EndedAt:    now,
		},
	}
}

// Operation is an in-flight ledger record. It is a builder: configure the
// target/batch, then finish with Run (API path) or FinishProcess (subprocess
// path).
type Operation struct {
	l *Ledger
	e Entry
}

// For sets the operated object.
func (op *Operation) For(t Target) *Operation {
	op.e.Target = t
	return op
}

// Batch attaches the record to a bulk operation. An empty id detaches it.
func (op *Operation) Batch(id string, index, total int) *Operation {
	if id != "" {
		op.e.BatchID = id
		op.e.BatchIndex = index
		op.e.BatchTotal = total
	}
	return op
}

// Run executes fn (the docker API call), records its outcome, and returns the
// operation error joined with any ledger write error (so a failed write is
// reported rather than silently swallowed). On a disabled ledger it simply
// runs fn and returns its error untouched.
func (op *Operation) Run(fn func() error) error {
	var opErr error
	if fn != nil {
		opErr = fn()
	}
	if op.l == nil {
		return opErr
	}
	recErr := op.commit(opErr)
	return errors.Join(opErr, recErr)
}

// FinishProcess records the outcome of a subprocess that the caller already
// ran. command is the raw command line, exitCode the process exit code (-1 if
// it could not be obtained) and err the execution error. Returns the
// operation error joined with any ledger write error, so a simple error check
// surfaces both.
func (op *Operation) FinishProcess(command string, exitCode int, err error) error {
	recErr := op.RecordProcessResult(command, exitCode, err)
	if op.l == nil {
		// disabled ledger: preserve the caller's error exactly
		return err
	}
	return errors.Join(err, recErr)
}

// RecordProcessResult persists a subprocess outcome and returns ONLY the
// ledger's persistence error — never the operation error itself. Interactive
// subprocesses use this because their failures are shown in-terminal and were
// historically not turned into error panels; a ledger write failure, on the
// other hand, must still be reported.
func (op *Operation) RecordProcessResult(command string, exitCode int, err error) error {
	if op.l == nil {
		return nil
	}
	op.e.Path = PathProcess
	op.e.Command = command
	code := exitCode
	op.e.ExitCode = &code
	return op.commit(err)
}

func (op *Operation) commit(err error) error {
	op.e.EndedAt = op.l.cfg.Now()
	op.e.DurationMS = op.e.EndedAt.Sub(op.e.StartedAt).Milliseconds()
	op.e.Success = err == nil
	if err != nil {
		op.e.Error = err.Error()
	}
	return op.l.append(op.e)
}

// Append commits a fully prepared entry. Seq is assigned by the ledger; the
// connection defaults to the ledger's connection when empty. This is the
// single write outlet used by Operation. It returns an error explicitly when
// the record cannot be persisted.
func (l *Ledger) Append(e Entry) error {
	if l == nil || !l.cfg.Enabled {
		return nil
	}
	return l.append(e)
}

func (l *Ledger) append(e Entry) error {
	if e.Action == "" {
		return errors.New("ledger: entry is missing an action")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed || l.file == nil {
		return errors.New("ledger: is closed")
	}

	l.seq++
	e.Seq = l.seq
	if e.Connection == "" {
		e.Connection = l.conn
	}
	if e.StartedAt.IsZero() {
		e.StartedAt = l.cfg.Now()
	}
	if e.EndedAt.IsZero() {
		e.EndedAt = e.StartedAt
	}
	e.DurationMS = e.EndedAt.Sub(e.StartedAt).Milliseconds()

	data, err := json.Marshal(e)
	if err != nil {
		l.seq--
		return fmt.Errorf("ledger: encode entry: %w", err)
	}
	data = append(data, '\n')

	// One write call under the mutex keeps lines intact and ordered; Sync
	// makes sure a crash cannot silently drop the record.
	n, err := l.file.Write(data)
	if err != nil {
		l.seq--
		return fmt.Errorf("ledger: write %q: %w", l.path, err)
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("ledger: flush %q: %w", l.path, err)
	}

	l.size += int64(n)
	l.entries = append(l.entries, e)

	return l.rotateLocked()
}

// Query returns a point-in-time, ordered copy of the records matching f. The
// returned slice can be consumed without holding any lock and will never
// contain a half-written record.
func (l *Ledger) Query(f Filter) []Entry {
	if l == nil || !l.cfg.Enabled {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	result := make([]Entry, 0, len(l.entries))
	for _, e := range l.entries {
		if f.matches(e) {
			result = append(result, e)
		}
	}
	return result
}

// Recent returns at most limit records ending at limitOffset from the newest
// record (0 = newest page). limit <= 0 means all records.
func (l *Ledger) Recent(limit, limitOffset int) []Entry {
	if l == nil || !l.cfg.Enabled {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	end := len(l.entries) - limitOffset
	if end < 0 {
		end = 0
	}
	start := 0
	if limit > 0 && end-limit > start {
		start = end - limit
	}
	result := make([]Entry, end-start)
	copy(result, l.entries[start:end])
	return result
}

// replay loads every valid JSON line from disk. A truncated final line (a
// process killed mid-write in a previous run) is discarded together with any
// other unparseable lines, and the file is repaired; the in-memory view never
// exposes half-written records.
func (l *Ledger) replay() error {
	data, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("ledger: read %q: %w", l.path, err)
	}

	lines := strings.Split(string(data), "\n")
	entries := make([]Entry, 0, len(lines))
	corrupt := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			corrupt = true
			continue
		}
		entries = append(entries, e)
		if e.Seq > l.seq {
			l.seq = e.Seq
		}
	}
	l.entries = entries
	if info, err := os.Stat(l.path); err == nil {
		l.size = info.Size()
	}

	if corrupt {
		// Repair the file before appending anything. Do not reopen it: New
		// opens the regular append handle itself once replay is done.
		if err := l.writeLocked(l.entries, true, false); err != nil {
			return err
		}
	}
	return nil
}

func (l *Ledger) openFile() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("ledger: open %q: %w", l.path, err)
	}
	l.file = f
	if info, err := f.Stat(); err == nil {
		l.size = info.Size()
	}
	return nil
}

// rotateLocked enforces the age and size limits. The caller must hold l.mu.
func (l *Ledger) rotateLocked() error {
	now := l.cfg.Now()
	cutoff := now.Add(-l.cfg.MaxAge)

	overAge := false
	for _, e := range l.entries {
		if e.EndedAt.Before(cutoff) {
			overAge = true
			break
		}
	}
	overSize := l.cfg.MaxSize > 0 && l.size > l.cfg.MaxSize
	if !overAge && !overSize {
		return nil
	}

	// Age rule: drop everything older than MaxAge.
	survivors := make([]Entry, 0, len(l.entries))
	for _, e := range l.entries {
		if e.EndedAt.Before(cutoff) {
			continue
		}
		survivors = append(survivors, e)
	}

	// Size rule: drop the oldest records until the survivors fit MaxSize.
	// Marshalled line length is deterministic for a given Entry, so this
	// matches the on-disk bytes.
	for l.persistedSize(survivors) > l.cfg.MaxSize && len(survivors) > 0 {
		survivors = survivors[1:]
	}

	return l.writeLocked(survivors, false, true)
}

// writeLocked atomically replaces the ledger file with survivors. When force
// is false it short-circuits when nothing changed and the file is within the
// size budget. When reopen is false the old handle (if any) stays closed and
// the rewritten file is not reopened — used during replay in New before the
// regular handle is opened. The caller must hold l.mu.
func (l *Ledger) writeLocked(survivors []Entry, force bool, reopen bool) error {
	if !force && len(survivors) == len(l.entries) && l.size <= l.cfg.MaxSize {
		return nil
	}

	tmp, err := os.CreateTemp(l.cfg.Dir, ".ledger-*.tmp")
	if err != nil {
		return fmt.Errorf("ledger: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	var total int64
	for i := range survivors {
		data, err := json.Marshal(survivors[i])
		if err != nil {
			tmp.Close()
			cleanup()
			return fmt.Errorf("ledger: encode entry during rotation: %w", err)
		}
		data = append(data, '\n')
		n, err := tmp.Write(data)
		total += int64(n)
		if err != nil {
			tmp.Close()
			cleanup()
			return fmt.Errorf("ledger: write temp file: %w", err)
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("ledger: flush temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("ledger: close temp file: %w", err)
	}

	// Windows cannot rename over an open file, so close the current handle
	// first. If anything fails past this point the ledger is marked closed so
	// subsequent writes surface the error instead of pretending to work.
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	if err := os.Rename(tmpName, l.path); err != nil {
		if reopen {
			l.closed = true
		}
		return fmt.Errorf("ledger: rotate %q: %w", l.path, err)
	}
	if reopen {
		if err := l.openFile(); err != nil {
			l.closed = true
			return err
		}
	}

	l.entries = survivors
	l.size = total
	if len(survivors) > 0 {
		l.seq = survivors[len(survivors)-1].Seq
	}
	return nil
}

func (l *Ledger) persistedSize(entries []Entry) int64 {
	var total int64
	for i := range entries {
		data, err := json.Marshal(entries[i])
		if err != nil {
			continue
		}
		total += int64(len(data) + 1)
	}
	return total
}

// ParseSize parses a human readable byte size such as "512", "512B",
// "100KB", "1MB" or "2GB" (binary units, 1KB = 1024 bytes). A plain number is
// interpreted as bytes.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("ledger: empty size")
	}

	units := []struct {
		suffix string
		mult   int64
	}{
		{"GB", 1 << 30},
		{"MB", 1 << 20},
		{"KB", 1 << 10},
		{"G", 1 << 30},
		{"M", 1 << 20},
		{"K", 1 << 10},
		{"B", 1},
	}

	upper := strings.ToUpper(s)
	for _, u := range units {
		if strings.HasSuffix(upper, u.suffix) {
			numPart := strings.TrimSpace(s[:len(s)-len(u.suffix)])
			n, err := strconv.ParseFloat(numPart, 64)
			if err != nil {
				return 0, fmt.Errorf("ledger: invalid size %q: %w", s, err)
			}
			return int64(n * float64(u.mult)), nil
		}
	}

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("ledger: invalid size %q", s)
	}
	return n, nil
}
