package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TrashStampLayout starts every trash batch name. UTC, sortable, no colons.
const TrashStampLayout = "20060102T150405Z"

// TrashBatch is one folder below .bfbackup-trash: what one run (or, with
// older versions, one second) moved to the trash.
type TrashBatch struct {
	Name  string `json:"name"`
	Time  int64  `json:"time"`             // unix seconds, from the name
	RunID int64  `json:"run_id,omitempty"` // 0: older version, no run in the name
	Files int64  `json:"files"`
	Bytes int64  `json:"bytes"`
}

// TrashBatchName names the trash batch of a run: <start>-run<id>. Without
// a run ID it is just the time.
func TrashBatchName(start time.Time, runID int64) string {
	s := start.UTC().Format(TrashStampLayout)
	if runID > 0 {
		s += fmt.Sprintf("-run%d", runID)
	}
	return s
}

// ParseTrashBatch reads a batch name: <stamp>[-run<id>][-<n>], where -<n>
// keeps a second copy of the same path apart. ok is false for any other
// name, so it is also the check for batch names a controller sends.
func ParseTrashBatch(name string) (t time.Time, runID int64, ok bool) {
	parts := strings.Split(name, "-")
	t, err := time.Parse(TrashStampLayout, parts[0])
	if err != nil {
		return time.Time{}, 0, false
	}
	for _, p := range parts[1:] {
		digits := strings.TrimPrefix(p, "run")
		n, err := strconv.ParseInt(digits, 10, 64)
		if err != nil || n < 0 || digits == "" || digits[0] == '+' {
			return time.Time{}, 0, false
		}
		if digits != p {
			runID = n
		}
	}
	return t, runID, true
}
