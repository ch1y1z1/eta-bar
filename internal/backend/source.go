package backend

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// ShellQuote encodes a literal POSIX shell argument, including embedded quotes.
func ShellQuote(s string) string {
	var b bytes.Buffer
	b.WriteByte('\'')
	for _, c := range s {
		if c == '\'' {
			b.WriteString("'\"'\"'")
		} else {
			b.WriteRune(c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}
func sshScript(path, root string) string {
	return "p=$(readlink -f -- " + ShellQuote(path) + ") || exit 1; " +
		"root=$(readlink -f -- " + ShellQuote(root) + ") || exit 1; " +
		"case \"$p\" in \"$root\"/*) ;; *) exit 1;; esac; " +
		"test -f \"$p\" || exit 1; tail -c 262145 -- \"$p\""
}

type limitedOutput struct{ bytes.Buffer }

func (b *limitedOutput) Write(p []byte) (int, error) {
	if len(p) > ReadLimit+1-b.Len() {
		return 0, errors.New("source output limit")
	}
	return b.Buffer.Write(p)
}
func (s *Server) readSource(ctx context.Context, source Source) ([]byte, error) {
	if source.Mode == "local" {
		var f *os.File
		var err error
		if source.localRoot != "" {
			root, openErr := os.OpenRoot(source.localRoot)
			if openErr != nil {
				return nil, openErr
			}
			defer root.Close()
			rel, relErr := filepath.Rel(source.localRoot, source.LogPath)
			if relErr != nil {
				return nil, relErr
			}
			f, err = root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		} else {
			f, err = os.OpenFile(source.LogPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		}
		if err != nil {
			return nil, err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("not a regular file")
		}
		offset := info.Size() - int64(ReadLimit+1)
		if offset < 0 {
			offset = 0
		}
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
		return io.ReadAll(io.LimitReader(f, ReadLimit+1))
	}
	h := s.config.Hosts[source.Host]
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-n", "-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=5", "-o", "ConnectionAttempts=1", "-o", "ServerAliveInterval=3", "-o", "ServerAliveCountMax=1", h.Alias, sshScript(source.LogPath, h.AllowedRoot))
	cmd.WaitDelay = time.Second
	var out limitedOutput
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, errors.New("source unavailable")
	}
	return out.Bytes(), nil
}
