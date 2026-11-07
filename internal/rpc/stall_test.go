package rpc

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/fsx"
	"bfbackup/internal/model"
)

// hangEP is a Local endpoint whose file reads send a little data and then
// hang, and whose Create never answers: a peer with a stuck disk.
type hangEP struct {
	*endpoint.Local
	release chan struct{}
}

func (h hangEP) Open(ctx context.Context, path string) (endpoint.Reader, error) {
	r, err := h.Local.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	return &hangReader{Reader: r, release: h.release}, nil
}

func (h hangEP) Create(ctx context.Context, path string, e model.Entry) (endpoint.Writer, error) {
	select {
	case <-h.release:
	case <-ctx.Done():
	}
	return nil, errors.New("released")
}

type hangReader struct {
	endpoint.Reader
	release chan struct{}
	sent    bool
}

func (r *hangReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return io.ReadFull(r.Reader, p) // enough to get past the server's write buffer
	}
	<-r.release
	return 0, io.ErrUnexpectedEOF
}

func TestStalledTransferStops(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "f"), make([]byte, 4<<20), 0o644)
	f, err := fsx.New([]string{root}, fsx.Options{UID: -1, GID: -1})
	if err != nil {
		t.Fatal(err)
	}
	ep := hangEP{Local: &endpoint.Local{FS: f, Version: "test"}, release: make(chan struct{})}
	t.Cleanup(func() { close(ep.release) })

	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		sess, _ := yamux.Client(a, SessionConfig(nil))
		(&Server{EP: ep}).Serve(ctx, sess)
	}()
	sess, err := yamux.Server(b, SessionConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	r := &Remote{Session: sess, IdleTimeout: 200 * time.Millisecond}

	rd, err := r.Open(context.Background(), filepath.Join(root, "f"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = io.Copy(io.Discard, rd)
	rd.Close()
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("read err = %v, want ErrStalled", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("stall detected too late")
	}

	if _, err := r.Create(context.Background(), filepath.Join(root, "g"), model.Entry{Size: 1}); !errors.Is(err, ErrStalled) {
		t.Fatalf("create err = %v, want ErrStalled", err)
	}

	// The session survives a stalled stream.
	if _, err := r.Info(context.Background()); err != nil {
		t.Fatalf("info after stall: %v", err)
	}
}
