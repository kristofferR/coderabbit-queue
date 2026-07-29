package crq

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SessionLogTail is the newest part of one autofix session's output.
type SessionLogTail struct {
	Text      string `json:"text"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
}

// TailSessionLog reads a bounded tail of a log that belongs to repo's crq
// workspace. The caller supplies the path recorded in shared state, so this
// boundary must reject a forged state value that points elsewhere.
func (s *Service) TailSessionLog(ctx context.Context, repo, path string, maxBytes int64) (SessionLogTail, error) {
	if err := ctx.Err(); err != nil {
		return SessionLogTail{}, err
	}
	dir, err := s.workspace(ctx).LogDir(repo)
	if err != nil {
		return SessionLogTail{}, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return SessionLogTail{}, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return SessionLogTail{}, fmt.Errorf("resolving session log directory: %w", err)
	}
	path, err = filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return SessionLogTail{}, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return SessionLogTail{}, fmt.Errorf("resolving session log path: %w", err)
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return SessionLogTail{}, errors.New("session log is outside this repository's workspace")
	}
	if maxBytes <= 0 || maxBytes > 1<<20 {
		maxBytes = 128 << 10
	}
	file, err := os.Open(path)
	if err != nil {
		return SessionLogTail{}, fmt.Errorf("opening session log: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return SessionLogTail{}, err
	}
	start := info.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return SessionLogTail{}, err
	}
	body, err := io.ReadAll(io.LimitReader(file, maxBytes))
	if err != nil {
		return SessionLogTail{}, err
	}
	return SessionLogTail{Text: string(body), Size: info.Size(), Truncated: start > 0}, nil
}
