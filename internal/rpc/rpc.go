// Package rpc carries endpoint.Endpoint calls over a yamux session.
//
// Every call opens its own stream: one request frame, then method specific
// frames. Simple calls get one response frame. "scan" streams listings,
// "read" and "write" stream file data as chunks followed by a trailer frame
// with the SHA-256, so many files can move in parallel over one TCP
// connection.
package rpc

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hashicorp/yamux"

	"bfbackup/internal/model"
)

const (
	bufSize = 256 << 10
	// scanBatch splits huge directories into several frames.
	scanBatch = 2000
)

// ReplyTimeout bounds the first answer of a resumed transfer: before it
// answers, the peer hashes the part of the file that is already there,
// which can take much longer than Remote.IdleTimeout for large files.
var ReplyTimeout = 30 * time.Minute

type request struct {
	M string          `json:"m"`
	P json.RawMessage `json:"p,omitempty"`
	// Z asks for compressed file data or listings (see compress.go).
	Z bool `json:"z,omitempty"`
}

type response struct {
	Err  string          `json:"err,omitempty"`
	Code string          `json:"code,omitempty"`
	R    json.RawMessage `json:"r,omitempty"`
	// Z: the data that follows (read) or is expected (write) is compressed.
	Z bool `json:"z,omitempty"`
}

func (r response) error() error {
	if r.Code == "" && strings.HasPrefix(r.Err, "unknown method ") {
		// Older peers answer calls they do not know like this, without a code.
		return fmt.Errorf("%w (%s)", model.ErrUnsupported, r.Err)
	}
	return model.ErrorFromCode(r.Code, r.Err)
}

func errResponse(err error) response {
	return response{Err: err.Error(), Code: model.ErrorCode(err)}
}

type trailer struct {
	Hash  string `json:"hash,omitempty"`
	Abort bool   `json:"abort,omitempty"`
	Err   string `json:"err,omitempty"`
	Code  string `json:"code,omitempty"`
}

type scanFrame struct {
	L    *model.DirListing `json:"l,omitempty"`
	Done bool              `json:"done,omitempty"`
	Err  string            `json:"err,omitempty"`
	Code string            `json:"code,omitempty"`
	// Z, in the first frame only: the rest of the stream is compressed.
	Z bool `json:"z,omitempty"`
	// H is the bytes read for checksums since the last frame with H. Sent
	// while a slow hash produces no listings; older controllers skip it.
	H int64 `json:"h,omitempty"`
	// D is the folders the scan started reading since the last frame with
	// D, C the latest of them. Sent while a slow folder walk produces no
	// listings; older controllers skip them.
	D int64  `json:"d,omitempty"`
	C string `json:"c,omitempty"`
}

type pathParams struct {
	Path string `json:"path"`
}

type statParams struct {
	Paths []string `json:"paths"`
}

type trashParams struct {
	Root string `json:"root"`
	Rel  string `json:"rel"`
	// Batch is the trash folder to use (one per run). Older peers ignore
	// it and name the folder after the time.
	Batch string `json:"batch,omitempty"`
}

type purgeParams struct {
	Root      string `json:"root"`
	OlderThan int64  `json:"older_than"`
}

type hookParams struct {
	Name string            `json:"name"`
	Env  map[string]string `json:"env"`
}

type symlinkParams struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

type writeParams struct {
	Path  string      `json:"path"`
	Entry model.Entry `json:"entry"`
	// Resume keeps data of an aborted earlier attempt; the response tells
	// how many bytes are already there. Older peers ignore it and answer 0.
	Resume bool `json:"resume,omitempty"`
}

// readAtParams is for "readat": read from Offset on, hash the whole file.
type readAtParams struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
}

// SessionConfig is the yamux configuration used by both sides. Keepalives
// detect dead peers; the large stream window keeps single streams fast. The
// timeouts leave room for slow, busy WAN links, where a keepalive can wait
// behind megabytes of queued file data.
func SessionConfig(log io.Writer) *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 30 * time.Second
	cfg.ConnectionWriteTimeout = 2 * time.Minute
	cfg.StreamOpenTimeout = 75 * time.Second
	cfg.MaxStreamWindowSize = 16 << 20
	cfg.AcceptBacklog = 512
	if log == nil {
		log = io.Discard
	}
	cfg.LogOutput = log
	return cfg
}
