// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// Logs is the log reading plugin: tail gives the last lines of a catalog log source. Which file
// a source is on this machine is local deployment data (the -source flags), never a path from
// a call (INJ-1). The file is opened without following a symbolic link, and only a regular
// file is read (DEP-3, TEN-5).
type Logs struct {
	Sources map[string]string // source id -> absolute file path
	// MaxBytes bounds how far back from the end the plugin reads.
	MaxBytes int64
}

// Call runs tail.
func (l *Logs) Call(ctx context.Context, tool string, args map[string]any, _ *mcp.Meta) (*mcp.CallResult, error) {
	if tool != "tail" {
		return nil, errors.New("unknown tool")
	}
	src, err := argString(args, "source")
	if err != nil {
		return nil, err
	}
	path, ok := l.Sources[src]
	if !ok {
		return nil, errors.New("the source is not on this machine")
	}
	lines := 100
	if n, ok := args["lines"].(float64); ok && n >= 1 && n <= 500 {
		lines = int(n)
	}
	text, err := tail(path, lines, l.MaxBytes)
	if err != nil {
		return nil, err
	}
	return Text(text), nil
}

// tail reads the last n lines of a regular file, at most limit bytes from its end.
func tail(path string, n int, limit int64) (string, error) {
	if limit <= 0 {
		limit = 1 << 20
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("the source cannot be opened")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", errors.New("the source is not a regular file")
	}
	size := st.Size()
	start := max(0, size-limit)
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return "", errors.New("the source cannot be read")
	}
	buf = bytes.TrimRight(buf, "\n")
	if start > 0 {
		// The first line is likely cut: drop it.
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	idx := len(buf)
	for k := 0; k < n && idx > 0; k++ {
		j := bytes.LastIndexByte(buf[:idx], '\n')
		if j < 0 {
			idx = 0
			break
		}
		if k == n-1 {
			idx = j + 1
			break
		}
		idx = j
	}
	return string(buf[idx:]), nil
}
