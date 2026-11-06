package claudesessions

import (
	"path/filepath"
	"strings"

	"github.com/rerangst/agent-keeper/internal/config"
)

// sessionArgs are Claude options that pick or name the conversation, or start
// Remote Control. They are never carried over: restore supplies its own
// --resume and -n, and Remote Control reattaches from the transcript.
var sessionArgs = map[string]string{
	"-c":               config.ArityBool,
	"--continue":       config.ArityBool,
	"-r":               "optional",
	"--resume":         "optional",
	"--session-id":     config.ArityValue,
	"--fork-session":   config.ArityBool,
	"-n":               config.ArityValue,
	"--name":           config.ArityValue,
	"--remote-control": "optional",
}

// ExtractFlags returns the allowlisted launch flags from a Claude process argv,
// in their original order and spelling, plus the unknown flags it dropped.
// Session-selecting options and positional arguments (prompts) are dropped
// silently. allowlist maps flag name to arity (config.Arity*).
func ExtractFlags(argv []string, allowlist map[string]string) (kept, droppedUnknown []string) {
	args := argv[programArgsStart(argv):]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" { // everything after is positional
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			continue // positional (prompt)
		}
		name, _, hasInline := strings.Cut(arg, "=")
		if arity, ok := allowlist[name]; ok {
			if hasInline {
				kept = append(kept, arg)
				continue
			}
			if arity == config.ArityValue && i+1 >= len(args) {
				continue // value missing: carrying the bare flag would swallow --resume
			}
			kept = append(kept, arg)
			switch arity {
			case config.ArityValue:
				i++
				kept = append(kept, args[i])
			case config.ArityVariadic:
				for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++
					kept = append(kept, args[i])
				}
			}
			continue
		}
		if arity, ok := sessionArgs[name]; ok {
			if hasInline {
				continue
			}
			switch arity {
			case config.ArityValue:
				i++
			case "optional":
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++
				}
			}
			continue
		}
		droppedUnknown = append(droppedUnknown, arg)
	}
	return kept, droppedUnknown
}

// programArgsStart skips the program itself. Native installs run as
// "claude …"; npm installs may run as "node [node-opts] /path/cli.js …".
func programArgsStart(argv []string) int {
	if len(argv) == 0 {
		return 0
	}
	switch filepath.Base(argv[0]) {
	case "node", "nodejs", "bun":
		for i := 1; i < len(argv); i++ {
			if !strings.HasPrefix(argv[i], "-") {
				return i + 1 // first non-option is the script
			}
		}
		return len(argv)
	}
	return 1
}
