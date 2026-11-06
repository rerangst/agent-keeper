// Package systemd installs agent-keeper's user units and talks to the user's
// systemd instance (systemctl --user, loginctl).
package systemd

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/rerangst/agent-keeper/internal/registry"
)

// Runner executes a program and returns its stdout (tmux.ExecRunner fits).
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// Unit names.
const (
	SnapshotUnit = "agent-keeper-snapshot.service"
	RestoreUnit  = "agent-keeper-restore.service"
)

// Units lists agent-keeper's units.
var Units = []string{SnapshotUnit, RestoreUnit}

// ExecPath is the stable binary path units run; updates replace the file.
const ExecPath = "%h/.local/bin/agent-keeper"

//go:embed units/*.tmpl
var unitFS embed.FS

// Render returns the unit file contents by name.
func Render() (map[string]string, error) {
	out := map[string]string{}
	for _, name := range Units {
		tmpl, err := template.ParseFS(unitFS, "units/"+name+".tmpl")
		if err != nil {
			return nil, err
		}
		var b bytes.Buffer
		if err := tmpl.Execute(&b, struct{ Exec string }{ExecPath}); err != nil {
			return nil, err
		}
		out[name] = b.String()
	}
	return out, nil
}

// UnitDir returns ~/.config/systemd/user (honoring XDG_CONFIG_HOME).
func UnitDir(home string) string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "systemd", "user")
	}
	return filepath.Join(home, ".config", "systemd", "user")
}

// WriteUnits writes the rendered units into dir, reporting whether any changed.
func WriteUnits(dir string) (changed bool, err error) {
	units, err := Render()
	if err != nil {
		return false, err
	}
	for name, content := range units {
		path := filepath.Join(dir, name)
		if old, err := os.ReadFile(path); err == nil && string(old) == content {
			continue
		}
		if err := registry.WriteFileAtomic(path, []byte(content), 0o644); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// RemoveUnits deletes agent-keeper's unit files from dir.
func RemoveUnits(dir string) error {
	var errs []error
	for _, name := range Units {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Ctl runs systemctl --user and loginctl.
type Ctl struct {
	Runner Runner
}

// Systemctl runs `systemctl --user args…`.
func (c Ctl) Systemctl(ctx context.Context, args ...string) (string, error) {
	out, err := c.Runner.Run(ctx, "systemctl", append([]string{"--user"}, args...)...)
	if err != nil {
		return out, fmt.Errorf("systemctl --user %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// Reachable reports whether the user's systemd instance answers.
func (c Ctl) Reachable(ctx context.Context) error {
	_, err := c.Systemctl(ctx, "show", "--property=Version")
	return err
}

// Query returns a one-word systemctl answer (is-enabled, is-active) without
// treating the non-zero exit of a negative answer as an error.
func (c Ctl) Query(ctx context.Context, verb, unit string) string {
	out, _ := c.Runner.Run(ctx, "systemctl", "--user", verb, unit)
	if s := strings.TrimSpace(out); s != "" {
		return strings.Fields(s)[0]
	}
	return "unknown"
}

// Linger reports whether user lingering is enabled for user ("yes"/"no"/"").
func (c Ctl) Linger(ctx context.Context, user string) string {
	out, err := c.Runner.Run(ctx, "loginctl", "show-user", user, "--property=Linger", "--value")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// EnableLinger turns lingering on so user units start at boot without login.
// It succeeds as root or where polkit allows it.
func (c Ctl) EnableLinger(ctx context.Context, user string) error {
	_, err := c.Runner.Run(ctx, "loginctl", "enable-linger", user)
	return err
}
