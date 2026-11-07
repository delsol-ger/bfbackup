// Package match implements exclude patterns.
//
// Syntax (close to .gitignore):
//
//	*.tmp         a name anywhere in the tree
//	cache/        a directory named "cache" anywhere
//	/Downloads    "Downloads" directly below the root only
//	photos/*.raw  a path relative to the root (a "/" inside anchors it)
//	**/logs       "**" matches zero or more directories
//	# comment     lines starting with "#" and empty lines are ignored
//
// "*", "?" and "[...]" match within one path segment, as in path.Match.
package match

import (
	"fmt"
	"path"
	"strings"
)

// Matcher holds compiled exclude rules. The zero value excludes nothing.
type Matcher struct {
	rules []rule
}

type rule struct {
	segs    []string
	dirOnly bool
	source  string
}

// New compiles patterns. It fails on malformed patterns, so bad input is
// caught when a job is saved, not during a run.
func New(patterns []string) (*Matcher, error) {
	m := &Matcher{}
	for _, raw := range patterns {
		p := strings.TrimSpace(raw)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		r := rule{source: p}
		if strings.HasSuffix(p, "/") {
			r.dirOnly = true
			p = strings.TrimRight(p, "/")
		}
		anchored := strings.Contains(p, "/")
		p = strings.TrimPrefix(p, "/")
		if p == "" {
			return nil, fmt.Errorf("pattern %q matches nothing", raw)
		}
		segs := strings.Split(p, "/")
		for _, s := range segs {
			if s == "" {
				return nil, fmt.Errorf("pattern %q has an empty segment", raw)
			}
			if s == "**" {
				continue
			}
			if _, err := path.Match(s, ""); err != nil {
				return nil, fmt.Errorf("pattern %q: %w", raw, err)
			}
		}
		if !anchored {
			segs = append([]string{"**"}, segs...)
		}
		r.segs = segs
		m.rules = append(m.rules, r)
	}
	return m, nil
}

// Excluded reports whether rel (relative to the scan root, "/"-separated)
// matches a rule. Only rel itself is tested; callers walking a tree skip
// excluded directories, so their content is excluded implicitly.
func (m *Matcher) Excluded(rel string, isDir bool) bool {
	if m == nil || len(m.rules) == 0 || rel == "" {
		return false
	}
	parts := strings.Split(rel, "/")
	for _, r := range m.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if matchSegs(r.segs, parts) {
			return true
		}
	}
	return false
}

// ExcludedPath reports whether rel or any of its parent directories is
// excluded. Use it for single paths that did not come from a tree walk.
func (m *Matcher) ExcludedPath(rel string, isDir bool) bool {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		if m.Excluded(strings.Join(parts[:i], "/"), true) {
			return true
		}
	}
	return m.Excluded(rel, isDir)
}

func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			if len(rest) == 0 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegs(rest, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}
