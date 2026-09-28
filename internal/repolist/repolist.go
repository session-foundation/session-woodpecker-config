// Package repolist reads a file of repository patterns, picking up changes to it without a
// restart.
package repolist

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

// File is a list of path.Match patterns of repositories (owner/name), one or more per line, with
// blank lines and #-comments ignored.  A missing file is an empty list.
type File struct {
	path string
	log  *slog.Logger

	mu       sync.Mutex
	loaded   bool
	exists   bool
	modTime  time.Time
	size     int64
	patterns []string
}

// Open loads the list at path, failing if it cannot be read or contains an invalid pattern.
func Open(path string, log *slog.Logger) (*File, error) {
	f := &File{path: path, log: log}
	if err := f.reload(); err != nil {
		return nil, err
	}
	return f, nil
}

// Match reports whether repo matches any of the patterns.  The file is reloaded first if it has
// changed; if the new version cannot be used, the error is logged and the previous list is kept.
func (f *File) Match(repo string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.reload(); err != nil {
		f.log.Error("keeping the previous repository list", "file", f.path, "err", err)
	}
	for _, p := range f.patterns {
		if ok, _ := path.Match(p, repo); ok {
			return true
		}
	}
	return false
}

func (f *File) reload() error {
	fi, err := os.Stat(f.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if f.exists || !f.loaded {
			f.log.Warn("repository list does not exist; treating it as empty", "file", f.path)
		}
		f.loaded, f.exists, f.patterns = true, false, nil
		return nil
	case err != nil:
		return err
	case f.exists && fi.ModTime().Equal(f.modTime) && fi.Size() == f.size:
		return nil
	}
	// Recorded before parsing so that a broken file is reported once, not on every request.
	f.loaded, f.exists, f.modTime, f.size = true, true, fi.ModTime(), fi.Size()

	data, err := os.ReadFile(f.path)
	if err != nil {
		return err
	}
	patterns, err := parse(string(data))
	if err != nil {
		return fmt.Errorf("%s: %w", f.path, err)
	}
	f.patterns = patterns
	f.log.Info("loaded repository list", "file", f.path, "patterns", patterns)
	return nil
}

func parse(data string) ([]string, error) {
	var patterns []string
	for i, line := range strings.Split(data, "\n") {
		line, _, _ = strings.Cut(line, "#")
		for _, p := range strings.Fields(line) {
			if _, err := path.Match(p, ""); err != nil {
				return nil, fmt.Errorf("line %d: pattern %q: %w", i+1, p, err)
			}
			patterns = append(patterns, p)
		}
	}
	return patterns, nil
}
