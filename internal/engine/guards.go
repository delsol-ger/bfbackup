package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"path"
	"sort"
	"strings"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/model"
)

// Guards are the anomaly checks run before a backup changes anything on a
// target. They are global settings; each check can be switched off. When
// one fires, the run stops and waits for the user to continue or stop it.
type Guards struct {
	// History compares this run with the last backups of the same target:
	// changed files, or files gone from the source, far above normal.
	History       bool    `json:"history"`
	HistoryFactor float64 `json:"history_factor"` // times the median of the last runs
	HistoryMin    int64   `json:"history_min"`    // and at least this many files

	// Fixed is the classic limit: more than FixedPct % of the backed-up
	// files changed, and at least FixedMin.
	Fixed    bool    `json:"fixed"`
	FixedPct float64 `json:"fixed_pct"`
	FixedMin int64   `json:"fixed_min"`

	// Deletions stops before a backup removes many files from the target
	// (sync mode, or removing excluded files).
	Deletions bool    `json:"deletions"`
	DeletePct float64 `json:"delete_pct"`
	DeleteMin int64   `json:"delete_min"`
	// DeleteCount stops when more than DeleteCountMax files would be
	// removed, however large the target is. With both deletion checks on,
	// the lower limit wins.
	DeleteCount    bool  `json:"delete_count"`
	DeleteCountMax int64 `json:"delete_count_max"`

	// Encryption reads the start of a sample of changed files and stops
	// when many no longer look like their type (encrypted by ransomware).
	Encryption    bool    `json:"encryption"`
	EncryptSample int     `json:"encrypt_sample"`
	EncryptPct    float64 `json:"encrypt_pct"`

	// Extensions stops when many files are renamed to a new extension
	// (a.docx → a.docx.locked) or a new extension appears on many files.
	Extensions bool  `json:"extensions"`
	ExtMin     int64 `json:"ext_min"`

	// Shrink stops when the source is much smaller than at the last
	// backup (a disk that is only partly mounted, a wiped folder).
	Shrink    bool    `json:"shrink"`
	ShrinkPct float64 `json:"shrink_pct"`

	// Truncation stops when many changed files became empty.
	Truncation bool  `json:"truncation"`
	TruncMin   int64 `json:"trunc_min"`

	// TimeJump stops when many files got a modification time in the
	// future (a wrong clock, or tampered files).
	TimeJump    bool  `json:"time_jump"`
	TimeJumpMin int64 `json:"time_jump_min"`
}

// DefaultGuards are the settings of a new installation.
func DefaultGuards() Guards {
	return Guards{
		History: true, HistoryFactor: 5, HistoryMin: 50,
		Fixed: false, FixedPct: 10, FixedMin: 100,
		Deletions: true, DeletePct: 10, DeleteMin: 50,
		DeleteCount: true, DeleteCountMax: 50,
		Encryption: true, EncryptSample: 20, EncryptPct: 30,
		Extensions: true, ExtMin: 20,
		Shrink: true, ShrinkPct: 50,
		Truncation: true, TruncMin: 20,
		TimeJump: false, TimeJumpMin: 50,
	}
}

// Validate checks the ranges.
func (g Guards) Validate() error {
	checks := []struct {
		name    string
		v, l, h float64
	}{
		{"history factor", g.HistoryFactor, 1.5, 100},
		{"history minimum", float64(g.HistoryMin), 1, 1e9},
		{"fixed limit %", g.FixedPct, 1, 100},
		{"fixed limit minimum", float64(g.FixedMin), 1, 1e9},
		{"deletion limit %", g.DeletePct, 0, 100},
		{"deletion limit minimum", float64(g.DeleteMin), 1, 1e9},
		{"deletion count limit", float64(g.DeleteCountMax), 1, 1e9},
		{"encryption sample", float64(g.EncryptSample), 5, 500},
		{"encryption %", g.EncryptPct, 1, 100},
		{"extension minimum", float64(g.ExtMin), 1, 1e9},
		{"shrink %", g.ShrinkPct, 1, 99},
		{"truncation minimum", float64(g.TruncMin), 1, 1e9},
		{"time jump minimum", float64(g.TimeJumpMin), 1, 1e9},
	}
	for _, c := range checks {
		if c.v < c.l || c.v > c.h {
			return fmt.Errorf("anomaly check %s must be between %g and %g", c.name, c.l, c.h)
		}
	}
	return nil
}

// DeleteLimit is how many files a run may remove from a target without
// asking; 0 means no limit. With both deletion checks on, the lower wins.
func (g *Guards) DeleteLimit(stored int64) int64 {
	if g == nil {
		return 0
	}
	var limit int64
	if g.Deletions {
		limit = max(int64(math.Ceil(g.DeletePct/100*float64(stored))), g.DeleteMin)
	}
	if g.DeleteCount && (limit == 0 || g.DeleteCountMax < limit) {
		limit = g.DeleteCountMax
	}
	return limit
}

// Anomaly is one check that fired.
type Anomaly struct {
	Check   string   `json:"check"`
	Message string   `json:"message"`
	Sample  []string `json:"sample,omitempty"`
}

// scanFacts are what a backup's scan learned for the checks.
type scanFacts struct {
	knownExt   map[string]bool  // extensions of files already in the backup
	newExt     map[string]int64 // extensions of new files not seen in the backup
	newExtEg   map[string]string
	renamed    int64 // new files named like a vanished file plus an extension
	renamedEg  []string
	truncated  int64 // changed files that became empty
	truncEg    []string
	future     int64 // files with a modification time in the future
	futureEg   []string
	candidates []string // changed files with a known type, for the encryption check
}

func newScanFacts() *scanFacts {
	return &scanFacts{knownExt: map[string]bool{}, newExt: map[string]int64{}, newExtEg: map[string]string{}}
}

func ext(name string) string { return strings.ToLower(path.Ext(name)) }

const exampleCount = 5

func addExample(list []string, p string) []string {
	if len(list) < exampleCount {
		list = append(list, p)
	}
	return list
}

// median of the values (0 for none).
func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

// minHistory is how many earlier backups the history check needs.
const minHistory = 3

// check runs the enabled guards. history are the stats of the last backups
// of the target, newest first.
func (g *Guards) check(ctx context.Context, st model.RunStats, gone, removals, stored int64, history []model.RunStats,
	facts *scanFacts, src endpoint.Endpoint, root string) []Anomaly {
	if g == nil {
		return nil
	}
	var out []Anomaly
	if g.History && len(history) >= minHistory {
		var changed, goneHist []int64
		for _, h := range history {
			changed = append(changed, h.Changed)
			goneHist = append(goneHist, h.Gone)
		}
		if m := median(changed); st.Changed >= g.HistoryMin && float64(st.Changed) > g.HistoryFactor*float64(m) {
			out = append(out, Anomaly{Check: "history", Message: fmt.Sprintf("%d files changed; usually %d per backup (median of the last %d). That is more than %g times as many", st.Changed, m, len(history), g.HistoryFactor)})
		}
		if m := median(goneHist); gone >= g.HistoryMin && float64(gone) > g.HistoryFactor*float64(m) {
			out = append(out, Anomaly{Check: "history", Message: fmt.Sprintf("%d files disappeared from the source since the last backup; usually %d. That is more than %g times as many", gone, m, g.HistoryFactor)})
		}
	}
	if g.Fixed {
		limit := max(int64(math.Ceil(g.FixedPct/100*float64(stored))), g.FixedMin)
		if st.Changed > limit {
			out = append(out, Anomaly{Check: "fixed", Message: fmt.Sprintf("%d existing files changed, more than the limit of %d (%g %% of %d backed-up files, at least %d)", st.Changed, limit, g.FixedPct, stored, g.FixedMin)})
		}
	}
	if limit := g.DeleteLimit(stored); limit > 0 && removals > limit {
		out = append(out, Anomaly{Check: "deletions", Message: fmt.Sprintf("this backup would remove %d files from the target, more than the limit of %d", removals, limit)})
	}
	if g.Shrink && len(history) > 0 && history[0].ScannedBytes > 0 {
		prev := history[0].ScannedBytes
		if float64(st.ScannedBytes) < (1-g.ShrinkPct/100)*float64(prev) {
			out = append(out, Anomaly{Check: "shrink", Message: fmt.Sprintf("the source has %s, at the last backup it had %s: more than %g %% less. Is a disk missing?", model.HumanBytes(st.ScannedBytes), model.HumanBytes(prev), g.ShrinkPct)})
		}
	}
	if g.Truncation && facts.truncated >= g.TruncMin {
		out = append(out, Anomaly{Check: "truncation", Message: fmt.Sprintf("%d changed files are now empty (0 bytes)", facts.truncated), Sample: facts.truncEg})
	}
	if g.TimeJump && facts.future >= g.TimeJumpMin {
		out = append(out, Anomaly{Check: "time_jump", Message: fmt.Sprintf("%d files have a modification time in the future", facts.future), Sample: facts.futureEg})
	}
	if g.Extensions {
		if facts.renamed >= g.ExtMin {
			out = append(out, Anomaly{Check: "extensions", Message: fmt.Sprintf("%d files were renamed to a new extension (e.g. a.docx → a.docx.locked)", facts.renamed), Sample: facts.renamedEg})
		}
		for e, n := range facts.newExt {
			if n >= g.ExtMin && n*2 >= st.New {
				out = append(out, Anomaly{Check: "extensions", Message: fmt.Sprintf("%d new files have the extension %q, which the backup did not have before", n, e), Sample: []string{facts.newExtEg[e]}})
			}
		}
	}
	if g.Encryption && len(facts.candidates) > 0 {
		checked, bad, eg := encryptedSample(ctx, src, root, facts.candidates, g.EncryptSample)
		if checked >= minEncryptChecked && float64(bad)*100 >= g.EncryptPct*float64(checked) {
			out = append(out, Anomaly{Check: "encryption", Message: fmt.Sprintf("%d of %d sampled changed files no longer look like their file type (encrypted?)", bad, checked), Sample: eg})
		}
	}
	return out
}

// minEncryptChecked: fewer readable samples say nothing.
const minEncryptChecked = 5

// magic headers of common file types. A file of such a type that does not
// start with its header was probably rewritten by something else.
var magic = map[string][][]byte{
	".jpg": {{0xFF, 0xD8, 0xFF}}, ".jpeg": {{0xFF, 0xD8, 0xFF}},
	".png": {{0x89, 'P', 'N', 'G'}}, ".gif": {[]byte("GIF8")}, ".bmp": {[]byte("BM")},
	".tif": {{'I', 'I', '*', 0}, {'M', 'M', 0, '*'}}, ".tiff": {{'I', 'I', '*', 0}, {'M', 'M', 0, '*'}},
	".webp": {[]byte("RIFF")}, ".wav": {[]byte("RIFF")}, ".avi": {[]byte("RIFF")},
	".pdf": {[]byte("%PDF")},
	".zip": {{'P', 'K', 3, 4}, {'P', 'K', 5, 6}}, ".docx": {{'P', 'K', 3, 4}}, ".xlsx": {{'P', 'K', 3, 4}},
	".pptx": {{'P', 'K', 3, 4}}, ".odt": {{'P', 'K', 3, 4}}, ".ods": {{'P', 'K', 3, 4}}, ".epub": {{'P', 'K', 3, 4}},
	".jar": {{'P', 'K', 3, 4}}, ".apk": {{'P', 'K', 3, 4}},
	".doc": {{0xD0, 0xCF, 0x11, 0xE0}}, ".xls": {{0xD0, 0xCF, 0x11, 0xE0}}, ".ppt": {{0xD0, 0xCF, 0x11, 0xE0}},
	".gz": {{0x1F, 0x8B}}, ".7z": {{'7', 'z', 0xBC, 0xAF, 0x27, 0x1C}}, ".rar": {[]byte("Rar!")},
	".exe": {[]byte("MZ")}, ".dll": {[]byte("MZ")}, ".sqlite": {[]byte("SQLite format 3")}, ".db": {[]byte("SQLite format 3")},
	".flac": {[]byte("fLaC")}, ".ogg": {[]byte("OggS")}, ".mkv": {{0x1A, 0x45, 0xDF, 0xA3}},
}

// ftyp types carry "ftyp" at offset 4.
var ftyp = map[string]bool{".mp4": true, ".mov": true, ".m4a": true, ".m4v": true, ".heic": true, ".3gp": true}

// text types are low-entropy; encrypted data is not.
var text = map[string]bool{".txt": true, ".csv": true, ".md": true, ".json": true, ".xml": true, ".html": true,
	".htm": true, ".log": true, ".ini": true, ".yml": true, ".yaml": true, ".svg": true, ".rtf": true, ".tex": true}

func knownType(name string) bool {
	e := ext(name)
	return magic[e] != nil || ftyp[e] || text[e]
}

// looksWrong reports whether the start of a file contradicts its extension.
func looksWrong(name string, head []byte) bool {
	e := ext(name)
	switch {
	case ftyp[e]:
		return len(head) >= 8 && !bytes.Equal(head[4:8], []byte("ftyp"))
	case text[e]:
		return entropy(head) > 7.2
	}
	for _, m := range magic[e] {
		if bytes.HasPrefix(head, m) {
			return false
		}
	}
	return magic[e] != nil
}

// entropy in bits per byte (8 = random).
func entropy(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var n [256]int
	for _, c := range b {
		n[c]++
	}
	h := 0.0
	for _, c := range n {
		if c > 0 {
			p := float64(c) / float64(len(b))
			h -= p * math.Log2(p)
		}
	}
	return h
}

const headSize = 64 << 10

// encryptedSample reads the start of up to n candidate files.
func encryptedSample(ctx context.Context, src endpoint.Endpoint, root string, candidates []string, n int) (checked, bad int, eg []string) {
	step := max(len(candidates)/max(n, 1), 1) // spread the sample over the list
	for i := 0; i < len(candidates) && checked < n; i += step {
		rel := candidates[i]
		stepDone(ctx)
		r, err := src.Open(ctx, path.Join(root, rel))
		if err != nil {
			continue
		}
		head, err := io.ReadAll(io.LimitReader(r, headSize))
		r.Close()
		if (err != nil && len(head) == 0) || len(head) < 16 {
			continue
		}
		checked++
		if looksWrong(rel, head) {
			bad++
			eg = addExample(eg, rel)
		}
	}
	return checked, bad, eg
}
