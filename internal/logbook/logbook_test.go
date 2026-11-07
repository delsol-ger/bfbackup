package logbook

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Book {
	t.Helper()
	b, err := Open(filepath.Join(t.TempDir(), "logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func TestLevelsAndQuery(t *testing.T) {
	b := open(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	b.SetClock(func() time.Time { return now })

	b.Add(Entry{Level: LevelFile, Kind: "backup", Path: "dropped.txt", Message: "dropped at level 2"})
	b.SetLevel(LevelFile)
	b.Add(Entry{Level: FileError, Kind: "backup", Node: "colo", SrcNode: "local", JobID: 1, RunID: 7, Path: "a/x.txt", Message: "copy failed"})
	b.Add(Entry{Level: LevelEvent, Kind: KindPeer, Node: "colo", Message: "connected"})
	b.Add(Entry{Level: FileOK, Kind: "backup", Node: "colo", SrcNode: "local", JobID: 1, RunID: 7, Path: "b/y.txt", Message: "copied (new, 1 B)"})
	b.Add(Entry{Level: FileIssue, Kind: "verify_quick", Node: "colo", JobID: 1, RunID: 8, Path: "c/z.txt", Message: "unknown file on the target"})
	b.Add(Entry{Level: LevelEvent, Kind: KindSystem, Node: "local", Message: "maintenance 100%_done"})
	b.Add(Entry{Level: LevelError, Kind: "backup", Node: "colo", JobID: 1, RunID: 7, Message: "Backup failed"})

	main, total, err := b.Query(ctx, Filter{})
	if err != nil || total != 3 {
		t.Fatalf("main log: %d %v %v", total, main, err)
	}
	if main[0].Message != "Backup failed" || main[1].TS != now.UnixMilli() {
		t.Fatalf("newest first: %+v", main)
	}
	files, total, err := b.QueryFiles(ctx, Filter{})
	if err != nil || total != 3 {
		t.Fatalf("file log: %d %v %v", total, files, err)
	}
	for _, e := range files {
		if e.Path == "" {
			t.Fatalf("file log entry without path: %+v", e)
		}
	}

	count := func(q func(context.Context, Filter) ([]Entry, int, error), f Filter) int {
		t.Helper()
		_, n, err := q(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	for name, c := range map[string]struct {
		files bool
		f     Filter
		want  int
	}{
		"main level":        {false, Filter{Level: LevelError}, 1},
		"main kind":         {false, Filter{Kind: KindPeer}, 1},
		"main node":         {false, Filter{Node: "colo"}, 2},
		"main search":       {false, Filter{Search: "100%_"}, 1},
		"main no match":     {false, Filter{Search: "100%x"}, 0},
		"files error":       {true, Filter{Level: FileError}, 1},
		"files issue":       {true, Filter{Level: FileIssue}, 1},
		"files source node": {true, Filter{Node: "local"}, 2},
		"files run":         {true, Filter{RunID: 7}, 2},
		"files job":         {true, Filter{JobID: 1}, 3},
		"files path search": {true, Filter{Search: "y.txt"}, 1},
		"files msg search":  {true, Filter{Search: "FAILED"}, 1},
		"files since":       {true, Filter{Since: now.Add(time.Second).UnixMilli()}, 0},
	} {
		q := b.Query
		if c.files {
			q = b.QueryFiles
		}
		if got := count(q, c.f); got != c.want {
			t.Errorf("%s: %d entries, want %d", name, got, c.want)
		}
	}

	byLevel, _, _ := b.QueryFiles(ctx, Filter{Sort: "level", Asc: true})
	if byLevel[0].Level != FileError || byLevel[2].Level != FileOK {
		t.Fatalf("sort by level: %+v", byLevel)
	}
	byPath, _, _ := b.QueryFiles(ctx, Filter{Sort: "path", Asc: false, Limit: 1})
	if len(byPath) != 1 || byPath[0].Path != "c/z.txt" {
		t.Fatalf("sort by path desc, limit 1: %+v", byPath)
	}
	page2, total, _ := b.QueryFiles(ctx, Filter{Limit: 2, Offset: 2})
	if total != 3 || len(page2) != 1 {
		t.Fatalf("page 2: %d %+v", total, page2)
	}
	// Unknown sort columns fall back to time instead of reaching SQL.
	if _, _, err := b.Query(ctx, Filter{Sort: "id; DROP TABLE logs"}); err != nil {
		t.Fatal(err)
	}
}

// A log written before the file log existed: its per-file lines move over.
func TestMigrateFileLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	db.Exec("INSERT INTO logs(ts, level, kind, run_id, path, message) VALUES(1, 3, 'backup', 5, 'a.txt', 'copied'), (2, 2, 'backup', 5, '', 'Backup finished')")
	db.Close()

	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	main, _, _ := b.Query(context.Background(), Filter{})
	files, _, _ := b.QueryFiles(context.Background(), Filter{})
	if len(main) != 1 || main[0].Message != "Backup finished" || len(files) != 1 || files[0].Path != "a.txt" || files[0].RunID != 5 {
		t.Fatalf("after migration: main %+v, files %+v", main, files)
	}
}

func TestPrune(t *testing.T) {
	b := open(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		b.Add(Entry{Level: LevelError, TS: now.Add(time.Duration(i-10) * 24 * time.Hour).UnixMilli(), Message: "old"})
	}
	for i := 0; i < 5; i++ {
		b.Add(Entry{Level: LevelError, TS: now.UnixMilli(), Message: "new"})
	}
	b.Add(Entry{Level: FileError, TS: now.Add(-10 * 24 * time.Hour).UnixMilli(), Path: "old.txt", Message: "old file line"})
	n, err := b.Prune(ctx, now.Add(-3*24*time.Hour), 0)
	if _, files, _ := b.QueryFiles(ctx, Filter{}); err != nil || n != 8 || files != 0 {
		t.Fatalf("by age: %d %v", n, err)
	}
	n, err = b.Prune(ctx, now.Add(-30*24*time.Hour), 4)
	if err != nil || n != 4 {
		t.Fatalf("by count: %d %v", n, err)
	}
	left, total, _ := b.Query(ctx, Filter{Sort: "ts", Asc: true})
	if total != 4 || left[0].Message != "new" {
		t.Fatalf("left %d %+v", total, left)
	}
}

func TestCloseWritesQueued(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.db")
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2500; i++ {
		b.Add(Entry{Level: LevelError, Message: "line"})
	}
	b.Close()
	b.Add(Entry{Level: LevelError, Message: "after close is ignored"})

	b, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_, total, err := b.Query(context.Background(), Filter{})
	if err != nil || total != 2500 {
		t.Fatalf("after reopen: %d %v", total, err)
	}
	if _, total, _ := b.Query(context.Background(), Filter{Search: "after close"}); total != 0 {
		t.Fatal("entry added after Close was written")
	}
}

func TestNilBookIsDisabled(t *testing.T) {
	var b *Book
	if b.Enabled(LevelError) {
		t.Fatal("nil book enabled")
	}
	b.Add(Entry{Level: LevelError, Message: strings.Repeat("x", 3)}) // must not panic
}
