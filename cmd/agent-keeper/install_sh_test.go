package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// installerSandbox runs install.sh with a fake HOME and TMPDIR, and PATH
// stubs for curl and uname. Downloads are served from release/ by URL basename.
type installerSandbox struct {
	t       *testing.T
	home    string
	tmp     string
	stubs   string
	release string
	curlLog string
	arch    string
}

func newInstallerSandbox(t *testing.T) *installerSandbox {
	t.Helper()
	uname, err := exec.LookPath("uname")
	if err != nil {
		t.Skip("uname not available")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	s := &installerSandbox{t: t, home: t.TempDir(), tmp: t.TempDir(), stubs: t.TempDir(),
		release: t.TempDir(), curlLog: filepath.Join(t.TempDir(), "curl.log"), arch: "x86_64"}
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(s.stubs, "uname"), `#!/bin/sh
if [ "$1" = "-m" ]; then echo "$STUB_ARCH"; else exec "`+uname+`" "$@"; fi
`)
	write(filepath.Join(s.stubs, "curl"), `#!/bin/sh
out=""; url=""
while [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift 2;; -*) shift;; *) url="$1"; shift;; esac; done
echo "$url" >> "$STUB_LOG"
f="$STUB_REL/$(basename "$url")"
[ -f "$f" ] || exit 22
cp "$f" "$out"
`)
	return s
}

// fakeBinary returns a shell script standing in for a release binary. It
// records every invocation so tests can prove whether it ran.
func (s *installerSandbox) fakeBinary() (content string, ranLog string) {
	ranLog = filepath.Join(s.t.TempDir(), "ran.log")
	content = "#!/bin/sh\necho \"$*\" >> " + ranLog + "\ncase \"$1\" in version) echo v0.0.0-test;; esac\n"
	return content, ranLog
}

func sha256Hex(t *testing.T, data string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func (s *installerSandbox) run(args ...string) (code int, stdout, stderr string) {
	s.t.Helper()
	cmd := exec.Command("sh", append([]string{"install.sh"}, args...)...)
	cmd.Dir = repoRoot(s.t)
	cmd.Env = []string{
		"PATH=" + s.stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + s.home,
		"TMPDIR=" + s.tmp,
		"STUB_REL=" + s.release,
		"STUB_LOG=" + s.curlLog,
		"STUB_ARCH=" + s.arch,
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		code = 0
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		s.t.Fatal(err)
	}
	return code, out.String(), errOut.String()
}

// repoRoot is the module root; go test runs each package from its own dir.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "install.sh")); err != nil {
		t.Fatal(err)
	}
	return root
}

func (s *installerSandbox) installedBinary() bool {
	_, err := os.Stat(filepath.Join(s.home, ".local", "bin", "agent-keeper"))
	return err == nil
}

// homeEntries lists everything the installer created under HOME.
func (s *installerSandbox) homeEntries() []string {
	var out []string
	_ = filepath.WalkDir(s.home, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != s.home {
			rel, _ := filepath.Rel(s.home, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

func TestInstallShArgumentErrors(t *testing.T) {
	s := newInstallerSandbox(t)
	cases := []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"--bogus"}, 1, "unknown option: --bogus"},
		{[]string{"--local"}, 1, "--local needs a path"},
		{[]string{"--local", filepath.Join(s.tmp, "missing")}, 1, "no such file"},
	}
	for _, c := range cases {
		code, _, stderr := s.run(c.args...)
		if code != c.code || !strings.Contains(stderr, c.msg) {
			t.Errorf("%v: code=%d stderr=%q", c.args, code, stderr)
		}
	}
	if entries := s.homeEntries(); len(entries) != 0 {
		t.Errorf("argument errors wrote into HOME: %v", entries)
	}
}

func TestInstallShHelpExitsZero(t *testing.T) {
	s := newInstallerSandbox(t)
	for _, arg := range []string{"--help", "-h"} {
		code, stdout, _ := s.run(arg)
		if code != 0 || !strings.Contains(stdout, "Usage: install.sh") {
			t.Errorf("%s: code=%d stdout=%q", arg, code, stdout)
		}
	}
}

func TestInstallShUnsupportedArch(t *testing.T) {
	s := newInstallerSandbox(t)
	s.arch = "riscv64"
	code, _, stderr := s.run()
	if code != 1 || !strings.Contains(stderr, "unsupported architecture: riscv64") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(s.curlLog); err == nil {
		t.Error("download attempted for unsupported arch")
	}
	if entries := s.homeEntries(); len(entries) != 0 {
		t.Errorf("unsupported arch wrote into HOME: %v", entries)
	}
}

func TestInstallShChecksumMismatchNeverInstalls(t *testing.T) {
	s := newInstallerSandbox(t)
	content, ranLog := s.fakeBinary()
	asset := "agent-keeper_linux_amd64"
	if err := os.WriteFile(filepath.Join(s.release, asset), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// Checksum of different bytes: the served asset must be rejected.
	wrong := sha256Hex(t, "not the binary")
	if err := os.WriteFile(filepath.Join(s.release, "checksums.txt"), []byte(wrong+"  "+asset+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := s.run()
	if code != 1 || !strings.Contains(stderr, "checksum mismatch for "+asset) {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if s.installedBinary() {
		t.Error("binary installed despite checksum mismatch")
	}
	if _, err := os.Stat(ranLog); err == nil {
		t.Error("unverified binary was executed")
	}
	if entries := s.homeEntries(); len(entries) != 0 {
		t.Errorf("failed verification wrote into HOME: %v", entries)
	}
}

func TestInstallShMissingDownloadsFail(t *testing.T) {
	s := newInstallerSandbox(t)
	content, _ := s.fakeBinary()
	asset := "agent-keeper_linux_amd64"

	// Checksums file absent: fail before installing anything.
	if err := os.WriteFile(filepath.Join(s.release, asset), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := s.run(); code != 1 || !strings.Contains(stderr, "download failed") {
		t.Errorf("no checksums: code=%d stderr=%q", code, stderr)
	}

	// Checksums present but without a line for this asset.
	if err := os.WriteFile(filepath.Join(s.release, "checksums.txt"), []byte(sha256Hex(t, content)+"  other_asset\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := s.run(); code != 1 || !strings.Contains(stderr, "not listed in checksums.txt") {
		t.Errorf("asset unlisted: code=%d stderr=%q", code, stderr)
	}
	if s.installedBinary() {
		t.Error("binary installed without a checksum entry")
	}
}

func TestInstallShVerifiedInstall(t *testing.T) {
	s := newInstallerSandbox(t)
	content, ranLog := s.fakeBinary()
	asset := "agent-keeper_linux_amd64"
	if err := os.WriteFile(filepath.Join(s.release, asset), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// BSD-style "*" binary marker must also match.
	if err := os.WriteFile(filepath.Join(s.release, "checksums.txt"), []byte(sha256Hex(t, content)+" *"+asset+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := s.run()
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "checksum ok") {
		t.Errorf("stdout = %q", stdout)
	}
	info, err := os.Stat(filepath.Join(s.home, ".local", "bin", "agent-keeper"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("installed binary: %v mode=%v", err, info)
	}
	ran, _ := os.ReadFile(ranLog)
	if !strings.Contains(string(ran), "install") {
		t.Errorf("installer did not hand off to `agent-keeper install`: %q", ran)
	}
	if _, err := os.Stat(filepath.Join(s.home, ".local", "bin", ".agent-keeper.new")); err == nil {
		t.Error("staging file left in bin dir")
	}
}

// Known defect: the final `exec` replaces the shell, so the EXIT trap never
// removes the temp dir that holds a copy of the verified binary.
func TestInstallShRemovesTempDirOnSuccess(t *testing.T) {
	s := newInstallerSandbox(t)
	content, _ := s.fakeBinary()
	asset := "agent-keeper_linux_amd64"
	if err := os.WriteFile(filepath.Join(s.release, asset), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.release, "checksums.txt"), []byte(sha256Hex(t, content)+"  "+asset+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := s.run(); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	left, _ := os.ReadDir(s.tmp)
	if len(left) != 0 {
		names := make([]string, 0, len(left))
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Errorf("temp dir left behind after successful install: %v", names)
	}
}
