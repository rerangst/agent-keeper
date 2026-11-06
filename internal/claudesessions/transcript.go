package claudesessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// maxDroppedLines bounds how many trailing lines repair may remove. Power
// loss damages only the tail; more damage than this is left for Claude to
// report rather than silently discarding history.
const maxDroppedLines = 3

// RepairTranscript makes a JSONL transcript resumable after power loss.
// A complete last record missing only its newline gets the newline; a
// trailing partial write or zero-filled block, and up to maxDroppedLines
// invalid trailing lines, are removed. Either way the original is first
// copied to backupDir/<id>-<timestamp>.jsonl. It returns the backup path, or
// "" when the file was already intact.
func RepairTranscript(path, backupDir string, now time.Time) (string, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()
	end := size
	addNewline := false
	if end > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, end-1); err != nil {
			return "", err
		}
		if last[0] != '\n' { // final write cut short
			nl, err := lastNewline(f, end)
			if err != nil {
				return "", err
			}
			seg := make([]byte, end-(nl+1))
			if _, err := f.ReadAt(seg, nl+1); err != nil {
				return "", err
			}
			if json.Valid(seg) {
				addNewline = true // complete record, only its newline is missing
			} else {
				end = nl + 1
			}
		}
	}
	for dropped := 0; end > 0 && !addNewline; dropped++ {
		start, err := lastNewline(f, end-1)
		if err != nil {
			return "", err
		}
		line := make([]byte, end-1-(start+1))
		if _, err := f.ReadAt(line, start+1); err != nil {
			return "", err
		}
		if json.Valid(line) {
			break
		}
		if dropped == maxDroppedLines {
			return "", fmt.Errorf("%s: more than %d damaged trailing lines; not repaired", path, maxDroppedLines)
		}
		end = start + 1
	}
	if end == size && !addNewline {
		return "", nil
	}
	id := trimExt(filepath.Base(path))
	backup := filepath.Join(backupDir, fmt.Sprintf("%s-%s.jsonl", id, now.UTC().Format("20060102T150405Z")))
	if err := copyFile(f, backup); err != nil {
		return "", fmt.Errorf("backup before repair: %w", err)
	}
	if addNewline {
		if _, err := f.WriteAt([]byte{'\n'}, size); err != nil {
			return backup, err
		}
	} else if err := f.Truncate(end); err != nil {
		return backup, err
	}
	return backup, f.Sync()
}

// lastNewline returns the offset of the last '\n' before end, or -1.
func lastNewline(f *os.File, end int64) (int64, error) {
	const chunk = 64 << 10
	buf := make([]byte, chunk)
	for end > 0 {
		n := int64(chunk)
		if end < n {
			n = end
		}
		if _, err := f.ReadAt(buf[:n], end-n); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return end - n + int64(i), nil
		}
		end -= n
	}
	return -1, nil
}

func copyFile(src *os.File, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.NewSectionReader(src, 0, 1<<62)); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(dst)) // make the backup's directory entry durable
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// RestoreBackup puts a backup made by RepairTranscript back in place.
func RestoreBackup(backup, path string) error {
	src, err := os.Open(backup)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := path + ".agent-keeper-restore"
	if err := copyFile(src, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func trimExt(name string) string {
	return name[:len(name)-len(filepath.Ext(name))]
}
