package match

import "testing"

func TestExcluded(t *testing.T) {
	m, err := New([]string{
		"*.tmp",
		"cache/",
		"/Downloads",
		"photos/*.raw",
		"**/logs/**",
		"# comment",
		"",
		"a/**/z",
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		rel   string
		dir   bool
		want  bool
		about string
	}{
		{"x.tmp", false, true, "basename anywhere at top"},
		{"deep/er/x.tmp", false, true, "basename anywhere deep"},
		{"x.tmpl", false, false, "suffix must match whole name"},
		{"cache", true, true, "dir-only on dir"},
		{"a/cache", true, true, "dir-only deep"},
		{"cache", false, false, "dir-only does not match file"},
		{"Downloads", true, true, "anchored at root"},
		{"sub/Downloads", true, false, "anchored does not match deeper"},
		{"photos/a.raw", false, true, "path pattern"},
		{"x/photos/a.raw", false, false, "path pattern is anchored"},
		{"app/logs/x.log", false, true, "double star both sides"},
		{"a/z", true, true, "** matches zero dirs"},
		{"a/b/c/z", false, true, "** matches many dirs"},
		{"b/z", false, false, "no match"},
		{"", true, false, "root is never excluded"},
	}
	for _, c := range cases {
		if got := m.Excluded(c.rel, c.dir); got != c.want {
			t.Errorf("%s: Excluded(%q, %v) = %v, want %v", c.about, c.rel, c.dir, got, c.want)
		}
	}
}

func TestExcludedPath(t *testing.T) {
	m, _ := New([]string{"cache/"})
	if !m.ExcludedPath("a/cache/b/file.txt", false) {
		t.Fatal("parent dir exclusion must apply")
	}
	if m.ExcludedPath("a/b/file.txt", false) {
		t.Fatal("unexpected exclusion")
	}
}

func TestNilAndEmpty(t *testing.T) {
	var m *Matcher
	if m.Excluded("a", false) {
		t.Fatal("nil matcher excluded something")
	}
	m, _ = New(nil)
	if m.Excluded("a", false) {
		t.Fatal("empty matcher excluded something")
	}
}

func TestBadPatterns(t *testing.T) {
	for _, p := range []string{"[", "a//b", "/"} {
		if _, err := New([]string{p}); err == nil {
			t.Errorf("pattern %q accepted", p)
		}
	}
}
