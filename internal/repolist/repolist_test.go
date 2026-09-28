package repolist

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// write replaces the file with a distinct modification time, as a change within the filesystem's
// timestamp granularity would otherwise go unnoticed.
func write(t *testing.T, file, data string, n int) {
	t.Helper()
	if err := os.WriteFile(file, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1_700_000_000+int64(n), 0)
	if err := os.Chtimes(file, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func check(t *testing.T, f *File, want map[string]bool) {
	t.Helper()
	for repo, match := range want {
		if got := f.Match(repo); got != match {
			t.Errorf("Match(%q) = %v, want %v", repo, got, match)
		}
	}
}

func TestReload(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secret-repos")
	f, err := Open(file, discard)
	if err != nil {
		t.Fatal(err)
	}
	check(t, f, map[string]bool{"session-foundation/libsession-util": false})

	write(t, file, "# trusted\nsession-foundation/*  oxen-io/*\n\njagerman/loki-mq # just this one\n", 1)
	check(t, f, map[string]bool{
		"session-foundation/libsession-util": true,
		"oxen-io/oxen-core":                  true,
		"jagerman/loki-mq":                   true,
		"jagerman/other":                     false,
		"Bilb/session-desktop":               false,
		"session-foundation":                 false,
	})

	write(t, file, "Bilb/*\n", 2)
	check(t, f, map[string]bool{"Bilb/session-desktop": true, "session-foundation/libsession-util": false})

	write(t, file, "session-foundation/*\n[\n", 3)
	check(t, f, map[string]bool{"Bilb/session-desktop": true, "session-foundation/libsession-util": false})

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	check(t, f, map[string]bool{"Bilb/session-desktop": false})
}

func TestOpenInvalid(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secret-repos")
	write(t, file, "session-foundation/*\n[\n", 1)
	if _, err := Open(file, discard); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("expected an error for line 2, got %v", err)
	}
}
