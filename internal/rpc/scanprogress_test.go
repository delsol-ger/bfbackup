package rpc

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/fsx"
	"bfbackup/internal/model"
)

// slowScanEP lists one folder, then starts reading another and hangs
// there: a peer walking a slow disk.
type slowScanEP struct {
	*endpoint.Local
	release chan struct{}
}

func (s slowScanEP) Scan(ctx context.Context, req model.ScanRequest, fn func(model.DirListing) error) error {
	req.Reading("", 1)
	if err := fn(model.DirListing{Dir: "", Entries: []model.Entry{{Name: "a", Type: model.TypeFile}}}); err != nil {
		return err
	}
	req.Reading("slow", 1)
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return fn(model.DirListing{Dir: "slow"})
}

// A slow walk must not hide what was already listed, and the controller
// must learn which folder the peer reads.
func TestScanReportsSlowFolders(t *testing.T) {
	old := scanFlushEvery
	scanFlushEvery = 20 * time.Millisecond
	t.Cleanup(func() { scanFlushEvery = old })

	root := t.TempDir()
	f, err := fsx.New([]string{root}, fsx.Options{UID: -1, GID: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	ep := slowScanEP{Local: &endpoint.Local{FS: f, Version: "test"}, release: make(chan struct{})}

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
	r := &Remote{Session: sess}

	listed := make(chan string, 4)
	reading := make(chan string, 16)
	done := make(chan error, 1)
	go func() {
		done <- r.Scan(context.Background(), model.ScanRequest{Root: root, Reading: func(dir string, n int64) { reading <- dir }},
			func(l model.DirListing) error { listed <- l.Dir; return nil })
	}()
	wait := func(ch chan string, want, what string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case got := <-ch:
				if got == want {
					return
				}
			case <-deadline:
				t.Fatalf("%s %q not reported while the peer hangs", what, want)
			}
		}
	}
	wait(listed, "", "listing")
	wait(reading, "slow", "folder")
	close(ep.release)
	wait(listed, "slow", "listing")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
