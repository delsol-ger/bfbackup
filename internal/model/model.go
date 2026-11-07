// Package model holds the types shared by every BFBackup component.
package model

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// Reserved names inside job and target roots.
const (
	TrashDirName = ".bfbackup-trash"
	TempPrefix   = ".BFBackup-"
	TempSuffix   = ".tmp"
	// LegacyTempPrefix is the temp prefix of older versions. Leftovers with
	// it are still internal and get cleaned up.
	LegacyTempPrefix = ".bfb-"
)

// LocalNodeID is the ID of the controller's own file system.
const LocalNodeID = "local"

// DriveNodePrefix starts the node ID of a USB drive on the controller
// ("usb:<uuid>"). Paths on such a node are relative to the drive's root.
const DriveNodePrefix = "usb:"

// DriveNodeID returns the node ID of a USB drive.
func DriveNodeID(uuid string) string { return DriveNodePrefix + uuid }

// DriveUUID returns the drive of a USB drive node ID.
func DriveUUID(nodeID string) (string, bool) {
	if len(nodeID) > len(DriveNodePrefix) && nodeID[:len(DriveNodePrefix)] == DriveNodePrefix {
		return nodeID[len(DriveNodePrefix):], true
	}
	return "", false
}

// DriveMarkerName is the file at the root of a USB drive that identifies it.
const DriveMarkerName = ".bfbackup-drive.json"

// EntryType is the kind of a file system entry.
type EntryType uint8

const (
	TypeFile    EntryType = 1
	TypeSymlink EntryType = 2
	TypeDir     EntryType = 3
)

func (t EntryType) String() string {
	switch t {
	case TypeFile:
		return "file"
	case TypeSymlink:
		return "symlink"
	case TypeDir:
		return "dir"
	}
	return "unknown"
}

// Entry describes one file, symlink or directory.
// MTime is in Unix nanoseconds, Mode holds permission bits only.
type Entry struct {
	Name  string    `json:"n"`
	Type  EntryType `json:"t"`
	Size  int64     `json:"s,omitempty"`
	MTime int64     `json:"m,omitempty"`
	Mode  uint32    `json:"p,omitempty"`
	Link  string    `json:"l,omitempty"`
	Hash  string    `json:"h,omitempty"`
}

// DirListing is the content of one directory, relative to a scan root.
// Dir is "" for the root itself.
type DirListing struct {
	Dir     string  `json:"d"`
	Entries []Entry `json:"e"`
	// More marks a partial listing; the next listing continues the same dir.
	More bool `json:"more,omitempty"`
	// Skipped names devices, sockets and FIFOs in the dir; they are never
	// backed up.
	Skipped []string `json:"s,omitempty"`
}

// ScanRequest asks an endpoint to walk Root.
type ScanRequest struct {
	Root     string   `json:"root"`
	Includes []string `json:"includes,omitempty"` // relative dirs; empty = whole root
	Excludes []string `json:"excludes,omitempty"`
	Hash     bool     `json:"hash,omitempty"` // compute SHA-256 for every file
	Workers  int      `json:"workers,omitempty"`
	// Hashed, if set, is called with the bytes read for checksums as the
	// scan goes, from several goroutines. A folder of large files takes long
	// to hash; this shows that the scan still moves. Not sent to peers: they
	// report it in their own frames.
	Hashed func(n int64) `json:"-"`
	// Reading, if set, is called as the scan starts reading folders: dir is
	// the latest one, n how many were started since the last call. From
	// several goroutines. Shows where a slow scan is while no listing comes
	// back. Not sent to peers: they report it in their own frames.
	Reading func(dir string, n int64) `json:"-"`
}

// StatResult is the result of stat-ing one path.
type StatResult struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Entry  Entry  `json:"entry"`
}

// FSUsage is the capacity of the file system holding a path.
type FSUsage struct {
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

// HookResult is the outcome of a hook script.
type HookResult struct {
	ExitCode   int    `json:"exit_code"`
	Output     string `json:"output"`
	DurationMs int64  `json:"duration_ms"`
}

// RootInfo describes one allowed root on a node.
type RootInfo struct {
	Path  string `json:"path"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

// DiskInfo is the capacity of one file system a node can use.
type DiskInfo struct {
	Path   string `json:"path"`
	FSType string `json:"fstype,omitempty"`
	Total  uint64 `json:"total"`
	Used   uint64 `json:"used"`
	Free   uint64 `json:"free"`
}

// NodeInfo describes a node (controller or peer).
type NodeInfo struct {
	Hostname string     `json:"hostname"`
	Version  string     `json:"version"`
	Roots    []RootInfo `json:"roots"`
	Disks    []DiskInfo `json:"disks"`
	Hooks    []string   `json:"hooks"`
}

// Compare modes decide when a file counts as changed.
const (
	CompareSize     = "size"
	CompareMTime    = "mtime"
	CompareChecksum = "checksum"
)

// ValidCompareMode reports whether m is a known compare mode.
func ValidCompareMode(m string) bool {
	return m == CompareSize || m == CompareMTime || m == CompareChecksum
}

// Errors that keep their meaning across the network.
var (
	ErrChanged      = errors.New("file changed while it was read")
	ErrHashMismatch = errors.New("checksum mismatch")
	ErrOutsideRoot  = errors.New("path is outside the allowed roots")
	ErrTypeConflict = errors.New("a different kind of entry exists at this path")
	ErrOffline      = errors.New("node is offline")
	// ErrUnsupported: the node runs an older version that lacks the call.
	ErrUnsupported = errors.New("not supported by this node's BFBackup version")
	ErrInvalidPath = errors.New("invalid path")
	// ErrConnLost: the connection to the node broke or stalled during a call.
	ErrConnLost = errors.New("connection to the node was lost")
)

// IsConnError reports whether err means the node could not be reached, as
// opposed to an error of the operation itself. Such calls can be retried
// once the node is back.
func IsConnError(err error) bool {
	return errors.Is(err, ErrOffline) || errors.Is(err, ErrConnLost)
}

var codes = []struct {
	code string
	err  error
}{
	{"not_found", fs.ErrNotExist},
	{"exists", fs.ErrExist},
	{"permission", fs.ErrPermission},
	{"changed", ErrChanged},
	{"hash_mismatch", ErrHashMismatch},
	{"outside_root", ErrOutsideRoot},
	{"type_conflict", ErrTypeConflict},
	{"offline", ErrOffline},
	{"unsupported", ErrUnsupported},
	{"invalid_path", ErrInvalidPath},
	{"conn_lost", ErrConnLost},
}

// ErrorCode returns a stable code for err, or "" when it has none.
func ErrorCode(err error) string {
	for _, c := range codes {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return ""
}

// ErrorFromCode rebuilds an error received from a remote node so that
// errors.Is works against the sentinels above.
func ErrorFromCode(code, msg string) error {
	if msg == "" && code == "" {
		return nil
	}
	for _, c := range codes {
		if c.code == code {
			return &remoteError{msg: msg, err: c.err}
		}
	}
	return errors.New(msg)
}

type remoteError struct {
	msg string
	err error
}

func (e *remoteError) Error() string { return e.msg }
func (e *remoteError) Unwrap() error { return e.err }

// CleanRel validates a path relative to a root. It returns "" for the root
// itself and rejects absolute paths, ".." and NUL bytes.
func CleanRel(p string) (string, error) {
	if strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%q: %w", p, ErrInvalidPath)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%q: %w", p, ErrInvalidPath)
		}
	}
	c := path.Clean("/" + p)[1:]
	return c, nil
}

// CleanAbs validates an absolute path.
func CleanAbs(p string) (string, error) {
	if !strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%q: %w", p, ErrInvalidPath)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%q: %w", p, ErrInvalidPath)
		}
	}
	return path.Clean(p), nil
}

// JoinRel joins a relative dir and a name; dir "" is the root.
func JoinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// SplitRel splits a relative path into dir and name.
func SplitRel(rel string) (dir, name string) {
	i := strings.LastIndexByte(rel, '/')
	if i < 0 {
		return "", rel
	}
	return rel[:i], rel[i+1:]
}

// IsInternalName reports whether a file name is one of BFBackup's own
// temporary files and must never be scanned or copied.
func IsInternalName(name string) bool {
	return (strings.HasPrefix(name, TempPrefix) || strings.HasPrefix(name, LegacyTempPrefix)) && strings.HasSuffix(name, TempSuffix)
}

// RunStats are the counters of one run. Unused fields stay zero.
type RunStats struct {
	Scanned      int64 `json:"scanned,omitempty"`
	ScannedBytes int64 `json:"scanned_bytes,omitempty"`
	New          int64 `json:"new,omitempty"`
	NewBytes     int64 `json:"new_bytes,omitempty"`
	Changed      int64 `json:"changed,omitempty"`
	ChangedBytes int64 `json:"changed_bytes,omitempty"`
	Unchanged    int64 `json:"unchanged,omitempty"`
	NotInSource  int64 `json:"not_in_source,omitempty"`
	// Gone: files that disappeared from the source since the last backup.
	Gone int64 `json:"gone,omitempty"`
	// Kept: changed files the job's setting kept on the target unreplaced.
	Kept    int64 `json:"kept,omitempty"`
	Removed int64 `json:"removed,omitempty"` // removed from the target by a sync
	// Adopted: files already on the target (not copied by BFBackup) that
	// matched the source, so they were not copied again.
	Adopted int64 `json:"adopted,omitempty"`

	Transferred      int64 `json:"transferred,omitempty"`
	TransferredBytes int64 `json:"transferred_bytes,omitempty"`
	Failed           int64 `json:"failed,omitempty"`

	Checked    int64 `json:"checked,omitempty"`
	Missing    int64 `json:"missing,omitempty"`
	Mismatched int64 `json:"mismatched,omitempty"`
	Untracked  int64 `json:"untracked,omitempty"`
	Repaired   int64 `json:"repaired,omitempty"`

	Skipped int64 `json:"skipped,omitempty"`
	Trashed int64 `json:"trashed,omitempty"`
	Deleted int64 `json:"deleted,omitempty"` // removed for good, without the trash
	Indexed int64 `json:"indexed,omitempty"`

	TargetFree  uint64 `json:"target_free,omitempty"`
	TargetTotal uint64 `json:"target_total,omitempty"`
}

// HumanBytes formats a size with binary units, e.g. "1.5 GiB".
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
