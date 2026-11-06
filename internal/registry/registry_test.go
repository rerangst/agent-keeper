package registry

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadMissingCreatesEmpty(t *testing.T) {
	r, err := Store{Path: filepath.Join(t.TempDir(), "registry.json")}.Load()
	if err != nil {
		t.Fatal(err)
	}
	if r.Schema != SchemaVersion || r.Entries == nil || len(r.Entries) != 0 {
		t.Fatalf("new registry = %+v", r)
	}
}

func TestUpdateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "registry.json")
	s := Store{Path: path}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	res := "restored"
	err := s.Update(func(r *Registry) error {
		r.Entries["alpha"] = &Entry{
			TmuxSession: "alpha", SessionID: "id-1", Cwd: "/w", Name: "Alpha", RemoteControl: true,
			BridgeSessionID: "session_X", Flags: []string{"--model", "m"}, State: StateActive,
			FirstSeen: now, LastSeen: now, Restore: Restore{Attempts: 1, LastResult: &res, LastAt: &now},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("registry perm = %o, want 600", perm)
	}
	r, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	e := r.Entries["alpha"]
	if e == nil || e.SessionID != "id-1" || !e.RemoteControl || e.Restore.LastResult == nil || *e.Restore.LastResult != "restored" ||
		!e.LastSeen.Equal(now) || len(e.Flags) != 2 {
		t.Fatalf("round trip lost data: %+v", e)
	}
	if r.UpdatedAt.IsZero() {
		t.Error("UpdatedAt not stamped")
	}
	// No temp files left behind.
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".registry.json.tmp-*"))
	if len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestNullRestoreFieldsSerialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	s := Store{Path: path}
	if err := s.Update(func(r *Registry) error {
		r.Entries["a"] = &Entry{TmuxSession: "a", State: StateActive}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"last_result": null`) {
		t.Errorf("expected null last_result in %s", data)
	}
}

func TestUpdateErrorDoesNotSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	s := Store{Path: path}
	boom := errors.New("boom")
	err := s.Update(func(r *Registry) error {
		r.Entries["x"] = &Entry{}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registry written despite error: %v", err)
	}
}

func TestLoadRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"corrupt.json": `{"schema": 1, "entries": {`,
		"schema.json":  `{"schema": 99, "entries": {}}`,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := (Store{Path: path}).Load(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestConcurrentUpdatesSerialize(t *testing.T) {
	s := Store{Path: filepath.Join(t.TempDir(), "registry.json")}
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Update(func(r *Registry) error {
				e := r.Entries["counter"]
				if e == nil {
					e = &Entry{TmuxSession: "counter"}
					r.Entries["counter"] = e
				}
				e.Restore.Attempts++
				return nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	r, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Entries["counter"].Restore.Attempts; got != n {
		t.Fatalf("lost updates: attempts = %d, want %d", got, n)
	}
}

func TestWriteFileAtomicReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json")
	for _, v := range []string{"one", "two"} {
		if err := WriteFileAtomic(path, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	if string(data) != "two" {
		t.Fatalf("content = %q", data)
	}
}

func TestUpdateRejectsSchemaMismatchWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	original := `{"schema": 99, "entries": {"keep": {"tmux_session": "keep"}}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	err := (Store{Path: path}).Update(func(r *Registry) error {
		called = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported schema") {
		t.Fatalf("err = %v, want unsupported schema", err)
	}
	if called {
		t.Error("update callback ran on a registry it cannot read")
	}
	if data, _ := os.ReadFile(path); string(data) != original {
		t.Fatalf("registry rewritten after schema error: %s", data)
	}
}

func TestLoadNullEntriesNormalized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, []byte(`{"schema": 1, "entries": null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := (Store{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if r.Entries == nil {
		t.Fatal("entries map must be non-nil so callers can insert")
	}
}

func TestWriteFileAtomicKeepsOldContentOnFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	if err := WriteFileAtomic(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if err := WriteFileAtomic(path, []byte("new"), 0o600); err == nil {
		t.Fatal("expected error writing into a read-only directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "old" {
		t.Fatalf("content = %q, want old content kept", data)
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".registry.json.tmp-*"))
	if len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}
