// Package registry is agent-keeper's durable record of tracked sessions,
// keyed by tmux session name. Writes are atomic (temp file, fsync, rename,
// directory fsync) and serialized with an flock on a sibling lock file.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// SchemaVersion is the on-disk format version.
const SchemaVersion = 1

// Entry states.
const (
	StateActive   = "active"   // restore at boot
	StateStopped  = "stopped"  // closed by the user while the machine was up
	StateExcluded = "excluded" // opted out by the user; never restored or revived
)

// Registry is the whole registry file.
type Registry struct {
	Schema    int               `json:"schema"`
	Host      string            `json:"host"`
	MachineID string            `json:"machine_id"`
	UpdatedAt time.Time         `json:"updated_at"`
	Entries   map[string]*Entry `json:"entries"`
}

// Entry is one tracked tmux session and the Claude session it runs.
type Entry struct {
	TmuxSession     string    `json:"tmux_session"`
	SessionID       string    `json:"session_id"`
	Cwd             string    `json:"cwd"`
	Name            string    `json:"name"`
	NameSource      string    `json:"name_source"`
	RemoteControl   bool      `json:"remote_control"`
	BridgeSessionID string    `json:"bridge_session_id,omitempty"`
	Flags           []string  `json:"flags"`
	LastStatus      string    `json:"last_status"`
	ClaudeVersion   string    `json:"claude_version"`
	State           string    `json:"state"`
	FirstSeen       time.Time `json:"first_seen"`
	LastSeen        time.Time `json:"last_seen"`
	LastSeenBoot    string    `json:"last_seen_boot"` // kernel boot_id when last seen alive
	Restore         Restore   `json:"restore"`
}

// Restore records the outcome of the most recent restore of an entry.
// InProgress is set by restore while it works on the entry (including the
// backoff between attempts) so the snapshot daemon never marks it stopped.
type Restore struct {
	Attempts   int        `json:"attempts"`
	LastResult *string    `json:"last_result"`
	LastAt     *time.Time `json:"last_at"`
	InProgress bool       `json:"in_progress"`
}

// New returns an empty registry stamped with this machine's identity.
func New() *Registry {
	r := &Registry{Schema: SchemaVersion, Entries: map[string]*Entry{}}
	r.StampHost()
	return r
}

// StampHost records hostname and machine id (for later multi-machine use).
func (r *Registry) StampHost() {
	if h, err := os.Hostname(); err == nil {
		r.Host = h
	}
	if id, err := os.ReadFile("/etc/machine-id"); err == nil {
		r.MachineID = strings.TrimSpace(string(id))
	}
}

// Store reads and writes one registry file.
type Store struct {
	Path string
}

// Load reads the registry; a missing file yields a new empty registry.
// Reads need no lock because writers replace the file atomically.
func (s Store) Load() (*Registry, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, err
	}
	var r Registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.Path, err)
	}
	if r.Schema != SchemaVersion {
		return nil, fmt.Errorf("%s: unsupported schema %d (want %d)", s.Path, r.Schema, SchemaVersion)
	}
	if r.Entries == nil {
		r.Entries = map[string]*Entry{}
	}
	for name, e := range r.Entries {
		if e == nil { // hand-edited "name": null
			delete(r.Entries, name)
		}
	}
	return &r, nil
}

// Update loads the registry under an exclusive lock, applies fn, and saves the
// result if fn returns nil. The lock is held for the whole read-modify-write.
func (s Store) Update(fn func(*Registry) error) error {
	return s.WithLock(func() error {
		r, err := s.Load()
		if err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
		return s.save(r)
	})
}

// WithLock runs fn while holding the registry's exclusive flock. The lock
// lives on a sibling file so atomic renames of the registry don't drop it.
func (s Store) WithLock(fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", lock.Name(), err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) // also released by Close
	return fn()
}

func (s Store) save(r *Registry) error {
	r.Schema = SchemaVersion
	r.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(s.Path, append(data, '\n'), 0o600)
}

// WriteFileAtomic writes data to path so readers see either the old or the
// new content, never a partial file, even across power loss.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
