package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rerangst/agent-keeper/internal/registry"
)

func seed(t *testing.T, store registry.Store, entries ...*registry.Entry) {
	t.Helper()
	if err := store.Update(func(r *registry.Registry) error {
		for _, e := range entries {
			r.Entries[e.TmuxSession] = e
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExcludeIncludeForget(t *testing.T) {
	paths := testPaths(t)
	store := registry.Store{Path: paths.Registry()}
	seed(t, store, &registry.Entry{TmuxSession: "alpha", State: registry.StateActive},
		&registry.Entry{TmuxSession: "beta", State: registry.StateActive})
	state := func(name string) string {
		r, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		if e := r.Entries[name]; e != nil {
			return e.State
		}
		return "<none>"
	}
	steps := []struct {
		args  []string
		code  int
		check string
		want  string
	}{
		{[]string{"exclude", "alpha"}, 0, "alpha", registry.StateExcluded},
		{[]string{"include", "alpha"}, 0, "alpha", registry.StateActive},
		{[]string{"include", "alpha"}, 1, "alpha", registry.StateActive}, // not excluded
		{[]string{"forget", "beta"}, 0, "beta", "<none>"},
		{[]string{"exclude", "nope"}, 1, "nope", "<none>"},
		{[]string{"exclude"}, 2, "alpha", registry.StateActive},
	}
	for _, s := range steps {
		var out, errOut bytes.Buffer
		if code := dispatch(s.args, paths, &out, &errOut); code != s.code {
			t.Fatalf("%v: code=%d want %d (stderr %q)", s.args, code, s.code, errOut.String())
		}
		if got := state(s.check); got != s.want {
			t.Fatalf("%v: %s state = %s, want %s", s.args, s.check, got, s.want)
		}
	}
}

func TestStatusTable(t *testing.T) {
	paths := testPaths(t)
	res := "restored"
	at := time.Now().Add(-time.Hour)
	seed(t, registry.Store{Path: paths.Registry()},
		&registry.Entry{TmuxSession: "alpha", SessionID: "11111111-1111-4111-8111-111111111111", State: registry.StateActive,
			RemoteControl: true, LastStatus: "idle", Cwd: "/w/alpha", Restore: registry.Restore{Attempts: 1, LastResult: &res, LastAt: &at}},
		&registry.Entry{TmuxSession: "beta", State: registry.StateExcluded, Restore: registry.Restore{InProgress: true}})
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"status"}, paths, &out, &errOut); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	s := out.String()
	for _, want := range []string{"TMUX", "alpha", "11111111", "on", "restored 1h0m0s ago", "beta", "excluded", "in progress", "linger:"} {
		if !strings.Contains(s, want) {
			t.Errorf("status output missing %q:\n%s", want, s)
		}
	}
}

func TestStatusEmptyRegistry(t *testing.T) {
	var out bytes.Buffer
	if code := dispatch([]string{"status"}, testPaths(t), &out, &out); code != 0 || !strings.Contains(out.String(), "no tracked sessions") {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
}

func TestPurgeDirKeepsRecordWhenAsked(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "disabled-conflicts.json")
	for _, f := range []string{keep, filepath.Join(dir, "registry.json"), filepath.Join(dir, "backups", "x.jsonl")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := purgeDir(dir, true, keep); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*"))
	if !slices.Equal(left, []string{keep}) {
		t.Fatalf("left = %v, want only the conflict record", left)
	}
	if err := purgeDir(dir, false, keep); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("full purge must remove the directory")
	}
}
