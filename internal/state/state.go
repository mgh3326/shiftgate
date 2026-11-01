// Package state owns the per-service durable record: the committed
// deployment (state.json, replaced atomically), the in-flight intent journal
// inside it, the append-only swap log, and the host and service locks.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mgh3326/shiftgate/internal/config"
)

// SchemaVersion is written into every state file.
const SchemaVersion = 1

// historyCap bounds the digest history kept for prune.
const historyCap = 50

// Slot is one colour's deployed identity.
type Slot struct {
	Color  config.Color `json:"color"`
	Digest string       `json:"digest"`
}

// Phase is how far an in-flight operation got before it was committed.
type Phase string

const (
	// PhaseStarting: the target container may be replaced or running; no
	// HAProxy runtime change has been sent yet.
	PhaseStarting Phase = "starting"
	// PhaseFlipping: runtime API commands may have been sent; HAProxy may
	// route to either colour until the operation commits or is recovered.
	PhaseFlipping Phase = "flipping"
)

// Intent is the journal of an operation that has not committed yet.
type Intent struct {
	OpID      string    `json:"op_id"`
	Op        string    `json:"op"` // deploy | rollback
	Phase     Phase     `json:"phase"`
	From      *Slot     `json:"from,omitempty"`
	To        Slot      `json:"to"`
	StartedAt time.Time `json:"started_at"`
}

// State is state.json. Active/Previous are committed truth; Pending is the
// journal of an operation that has not committed (nil when idle).
type State struct {
	SchemaVersion     int       `json:"schema_version"`
	Service           string    `json:"service"`
	Generation        int64     `json:"generation"`
	Active            *Slot     `json:"active,omitempty"`
	Previous          *Slot     `json:"previous,omitempty"`
	RollbackAvailable bool      `json:"rollback_available"`
	History           []string  `json:"history,omitempty"`
	LastSwapAt        time.Time `json:"last_swap_at,omitzero"`
	LastOp            string    `json:"last_op,omitempty"`
	LastResult        string    `json:"last_result,omitempty"`
	LastError         string    `json:"last_error,omitempty"`
	Pending           *Intent   `json:"pending,omitempty"`
}

// PushHistory records a successfully promoted digest, newest first.
func (s *State) PushHistory(digest string) {
	h := []string{digest}
	for _, d := range s.History {
		if d != digest && len(h) < historyCap {
			h = append(h, d)
		}
	}
	s.History = h
}

// Store is the state directory of one host.
type Store struct{ Dir string }

func (st Store) serviceDir(svc string) string { return filepath.Join(st.Dir, svc) }

// Path is the state file of svc.
func (st Store) Path(svc string) string { return filepath.Join(st.serviceDir(svc), "state.json") }

// LogPath is the swap log of svc.
func (st Store) LogPath(svc string) string { return filepath.Join(st.serviceDir(svc), "swaps.log") }

// ErrNoState means the service has never been deployed (no state file).
var ErrNoState = errors.New("no state")

// Load reads svc's state; ErrNoState when the file does not exist.
func (st Store) Load(svc string) (*State, error) {
	b, err := os.ReadFile(st.Path(svc))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoState
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("state %s is corrupt: %w", st.Path(svc), err)
	}
	if s.SchemaVersion != SchemaVersion || s.Service != svc {
		return nil, fmt.Errorf("state %s: schema %d / service %q not understood", st.Path(svc), s.SchemaVersion, s.Service)
	}
	return &s, nil
}

// Save replaces svc's state atomically: temp file, fsync, rename, fsync dir.
func (st Store) Save(s *State) error {
	s.SchemaVersion = SchemaVersion
	dir := st.serviceDir(s.Service)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(st.Path(s.Service), append(b, '\n'), 0o600)
}

// WriteFileAtomic writes data to path through a synced temp file and rename.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// SwapRecord is one line of swaps.log.
type SwapRecord struct {
	Time       time.Time    `json:"time"`
	OpID       string       `json:"op_id"`
	Service    string       `json:"service"`
	Op         string       `json:"op"`
	FromColor  config.Color `json:"from_color,omitempty"`
	ToColor    config.Color `json:"to_color"`
	FromDigest string       `json:"from_digest,omitempty"`
	ToDigest   string       `json:"to_digest"`
	Result     string       `json:"result"`
	Error      string       `json:"error,omitempty"`
	Warnings   []string     `json:"warnings,omitempty"`
}

// AppendSwap appends one JSON line to svc's swap log (audit, not authority).
func (st Store) AppendSwap(r SwapRecord) error {
	if err := os.MkdirAll(st.serviceDir(r.Service), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(st.LogPath(r.Service), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ErrLocked means another shiftgate process holds the lock.
var ErrLocked = errors.New("locked by another shiftgate operation")

// Lock is a held flock(2) lock; Unlock releases it.
type Lock struct{ f *os.File }

// Unlock releases the lock.
func (l *Lock) Unlock() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close() // closing the descriptor releases the flock
	l.f = nil
	return err
}

func flock(path string, how int) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return &Lock{f: f}, nil
}

// LockService takes svc's exclusive lock without waiting.
func (st Store) LockService(svc string) (*Lock, error) {
	l, err := flock(filepath.Join(st.serviceDir(svc), "lock"), syscall.LOCK_EX)
	if errors.Is(err, ErrLocked) {
		return nil, fmt.Errorf("service %s: %w", svc, ErrLocked)
	}
	return l, err
}

// LockHost takes the host coordination lock: shared for deploy/rollback,
// exclusive for render and prune, so a reload never interleaves with a flip.
// It waits up to wait for a conflicting holder.
func (st Store) LockHost(exclusive bool, wait time.Duration) (*Lock, error) {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	return lockWait(filepath.Join(st.Dir, "host.lock"), how, wait)
}

// LockConfigWrite serialises writers of the generated HAProxy file between
// concurrent deploys of different services.
func (st Store) LockConfigWrite(wait time.Duration) (*Lock, error) {
	return lockWait(filepath.Join(st.Dir, "config-write.lock"), syscall.LOCK_EX, wait)
}

func lockWait(path string, how int, wait time.Duration) (*Lock, error) {
	deadline := time.Now().Add(wait)
	for {
		l, err := flock(path, how)
		if !errors.Is(err, ErrLocked) {
			return l, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), ErrLocked)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// NewOpID returns a random operation id.
func NewOpID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
