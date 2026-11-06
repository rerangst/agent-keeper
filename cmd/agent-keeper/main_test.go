package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rerangst/agent-keeper/internal/config"
	"github.com/rerangst/agent-keeper/internal/registry"
)

// testPaths points every path at a temp dir, so no real state is touched.
func testPaths(t *testing.T) config.Paths {
	t.Helper()
	root := t.TempDir()
	return config.Paths{
		ConfigFile: filepath.Join(root, "config", "config.json"),
		StateDir:   filepath.Join(root, "state"),
		ClaudeHome: filepath.Join(root, "claude"),
		ProcRoot:   filepath.Join(root, "proc"),
	}
}

func TestRunVersionAndHelp(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 0 || strings.TrimSpace(out.String()) != version {
			t.Errorf("run(%v) = %d, %q", args, code, out.String())
		}
	}
	for _, args := range [][]string{{}, {"help"}, {"-h"}, {"--help"}} {
		var out bytes.Buffer
		if code := run(args, &out, &out); code != 0 || !strings.Contains(out.String(), "Usage:") {
			t.Errorf("run(%v) = %d, want usage text", args, code)
		}
	}
}

func TestDispatchErrors(t *testing.T) {
	paths := testPaths(t)
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"bogus"}, paths, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), `unknown command "bogus"`) {
		t.Errorf("unknown command: code=%d stderr=%q", code, errOut.String())
	}
	errOut.Reset()
	if code := dispatch([]string{"snapshot", "--nope"}, paths, &out, &errOut); code != 2 {
		t.Errorf("bad snapshot flag: code=%d stderr=%q", code, errOut.String())
	}
}

func TestDispatchRejectsBadConfig(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`{"poll_interval_s": 0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"snapshot", "--once"}, paths, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "config:") {
		t.Fatalf("code=%d stderr=%q, want config error", code, errOut.String())
	}
	if _, err := os.Stat(paths.Registry()); !os.IsNotExist(err) {
		t.Fatalf("registry written despite config error: %v", err)
	}
}

func TestSnapshotOnceWithoutSessions(t *testing.T) {
	paths := testPaths(t)
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"snapshot", "--once"}, paths, &out, &errOut); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "0 entries in "+paths.Registry()) {
		t.Fatalf("stdout = %q", out.String())
	}
	reg, err := (registry.Store{Path: paths.Registry()}).Load()
	if err != nil || reg.Schema != registry.SchemaVersion || len(reg.Entries) != 0 {
		t.Fatalf("registry = %+v, err = %v", reg, err)
	}
}

func TestSubcommandHelpExitsZero(t *testing.T) {
	for _, cmd := range []string{"snapshot", "daemon"} {
		var out, errOut bytes.Buffer
		if code := dispatch([]string{cmd, "-h"}, testPaths(t), &out, &errOut); code != 0 {
			t.Errorf("%s -h: code=%d", cmd, code)
		}
	}
}

func TestSnapshotOnceKeepsLogsOffStdout(t *testing.T) {
	paths := testPaths(t)
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"snapshot", "--once"}, paths, &out, &errOut); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if strings.Contains(out.String(), "level=") {
		t.Fatalf("log lines leaked into stdout: %q", out.String())
	}
}
