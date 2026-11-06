package restore

import "strings"

// Dialog is a Claude Code startup prompt that restore answers by moving the
// selection cursor to Choose and pressing Enter. When the UI text changes,
// this table is the one place to update.
type Dialog struct {
	Name   string
	Detect string // text that must be on screen
	Choose string // exact label of the option to select
}

// Dialogs lists the known prompts (texts from Claude Code 2.1.319). All three
// show the cancel/summary option first and focus it, so the cursor must move.
var Dialogs = []Dialog{
	{Name: "trust_folder", Detect: "Quick safety check", Choose: "Yes, I trust this folder"},
	{Name: "bypass_permissions", Detect: "Bypass Permissions mode", Choose: "Yes, I accept"},
	{Name: "large_session_resume", Detect: "Resuming the full session", Choose: "Resume full session as-is"},
}

// cursors are the markers Claude's select menus draw before the focused
// option (the unicode pointer, or its ASCII fallback).
var cursors = []string{"❯ ", "> "}

// MatchDialog returns the first known dialog drawn as a real select menu on
// screen, and the keys that pick its option: arrows from the focused row to
// the target row, then Enter. A menu is recognised only by structure: a row
// whose whole text is the target label, inside a block of consecutive rows
// (blank rows and box borders end it) that contains the focused row. Dialog
// text merely quoted in a conversation, or a ❯ on the REPL input line, does
// not match.
func MatchDialog(screen string) (Dialog, []string, bool) {
	lines := strings.Split(screen, "\n")
	for _, d := range Dialogs {
		if !strings.Contains(screen, d.Detect) {
			continue
		}
		if keys, ok := menuKeys(lines, d.Choose); ok {
			return d, keys, true
		}
	}
	return Dialog{}, nil, false
}

func menuKeys(lines []string, label string) ([]string, bool) {
	target := -1
	for i, l := range lines {
		if text, _ := optionRow(l); text == label {
			target = i // last match: the menu is drawn below any explanatory text
		}
	}
	if target < 0 {
		return nil, false
	}
	first, last := target, target
	for first > 0 && menuRow(lines[first-1]) {
		first--
	}
	for last < len(lines)-1 && menuRow(lines[last+1]) {
		last++
	}
	for i := first; i <= last; i++ {
		if _, focused := optionRow(lines[i]); focused {
			var keys []string
			for j := i; j < target; j++ {
				keys = append(keys, "Down")
			}
			for j := i; j > target; j-- {
				keys = append(keys, "Up")
			}
			return append(keys, "Enter"), true
		}
	}
	return nil, false
}

// optionRow strips box sides and the cursor from a menu row.
func optionRow(l string) (text string, focused bool) {
	t := strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "│|"))
	for _, c := range cursors {
		if rest, ok := strings.CutPrefix(t, c); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return t, false
}

// menuRow reports whether a screen line can be part of a select menu: not
// blank (ignoring box sides) and not a horizontal border.
func menuRow(l string) bool {
	return strings.Trim(l, " │|") != "" && !strings.Contains(l, "──")
}
