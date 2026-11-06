package claudesessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRepairTranscript(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	good := `{"a":1}` + "\n" + `{"b":2}` + "\n"
	cases := []struct {
		name     string
		content  string
		want     string
		repaired bool
		wantErr  bool
	}{
		{"intact", good, good, false, false},
		{"empty", "", "", false, false},
		{"complete last record missing only its newline", `{"a":1}` + "\n" + `{"b":2}`, `{"a":1}` + "\n" + `{"b":2}` + "\n", true, false},
		{"partial last line", good + `{"c":`, good, true, false},
		{"zero-filled tail", good + strings.Repeat("\x00", 4096), good, true, false},
		{"zero block then newline", good + strings.Repeat("\x00", 10) + "\n", good, true, false},
		{"two damaged lines", good + "garbage\n" + `{"x"` + "\n", good, true, false},
		{"only damage", "junk\n", "", true, false},
		{"too much damage left alone", good + "x\ny\nz\nw\n", good + "x\ny\nz\nw\n", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "11111111-1111-4111-8111-111111111111.jsonl")
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}
			backupDir := filepath.Join(dir, "backups")
			backup, err := RepairTranscript(path, backupDir, now)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			data, _ := os.ReadFile(path)
			if string(data) != c.want {
				t.Fatalf("content = %q, want %q", data, c.want)
			}
			if (backup != "") != c.repaired {
				t.Fatalf("backup = %q, repaired %v", backup, c.repaired)
			}
			if c.repaired {
				orig, _ := os.ReadFile(backup)
				if string(orig) != c.content || !strings.HasSuffix(backup, "11111111-1111-4111-8111-111111111111-20260102T030405Z.jsonl") {
					t.Fatalf("backup %s holds %q", backup, orig)
				}
				if err := RestoreBackup(backup, path); err != nil {
					t.Fatal(err)
				}
				if back, _ := os.ReadFile(path); string(back) != c.content {
					t.Fatalf("RestoreBackup = %q", back)
				}
			}
		})
	}
}

func TestRepairTranscriptLongLines(t *testing.T) {
	long := `{"blob":"` + strings.Repeat("x", 200<<10) + `"}` // longer than one read chunk
	path := filepath.Join(t.TempDir(), "id.jsonl")
	content := long + "\n" + long + "\n" + `{"partial`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RepairTranscript(path, t.TempDir(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != long+"\n"+long+"\n" {
		t.Fatalf("len = %d", len(data))
	}
}
