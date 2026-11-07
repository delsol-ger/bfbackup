package proto

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := map[string]any{"a": "b", "n": 3.0}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := ReadFrame(&buf, &out); err != nil {
		t.Fatal(err)
	}
	if out["a"] != "b" || out["n"] != 3.0 {
		t.Fatalf("got %v", out)
	}
	// Oversized length header is rejected before allocating.
	if err := ReadFrame(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff}), &out); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v", err)
	}
	// Truncated frame.
	if err := ReadFrame(bytes.NewReader([]byte{0, 0, 0, 10, '{'}), &out); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v", err)
	}
}

func TestChunks(t *testing.T) {
	var buf bytes.Buffer
	data := bytes.Repeat([]byte("0123456789"), MaxChunk/5) // 2 MiB, several chunks
	cw := &ChunkWriter{W: &buf}
	cw.Write(data)
	cw.Write([]byte("tail"))
	cw.Close()
	WriteFrame(&buf, "after")

	cr := &ChunkReader{R: &buf}
	got, err := io.ReadAll(cr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, append(data, "tail"...)) {
		t.Fatal("data mismatch")
	}
	var s string
	if err := ReadFrame(&buf, &s); err != nil || s != "after" {
		t.Fatalf("frame after chunks: %q %v", s, err)
	}
	// Truncated stream is an error, not a clean EOF.
	cr = &ChunkReader{R: bytes.NewReader([]byte{0, 0, 0, 5, 'a'})}
	if _, err := io.ReadAll(cr); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v", err)
	}
}

func TestToken(t *testing.T) {
	tok := NewToken()
	back, err := ParseToken(" " + tok.String() + "\n")
	if err != nil || back.NodeID != tok.NodeID || !bytes.Equal(back.Secret, tok.Secret) {
		t.Fatalf("round trip: %v", err)
	}
	for _, bad := range []string{"", "bfb1.x.y", "bfb2." + tok.NodeID + ".AAAA", "bfb1." + tok.NodeID + ".short"} {
		if _, err := ParseToken(bad); !errors.Is(err, ErrBadToken) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func handshake(t *testing.T, peerTok Token, serverSecrets map[string][]byte) (clientErr, serverErr error, node string) {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		clientErr = ClientHandshake(a, peerTok, PeerInfo{Hostname: "peer1", Version: "test"})
		a.Close()
	}()
	node, info, serverErr := ServerHandshake(b, func(id string) ([]byte, error) {
		s, ok := serverSecrets[id]
		if !ok {
			return nil, errors.New("unknown")
		}
		return s, nil
	})
	b.Close()
	<-done
	if serverErr == nil && info.Hostname != "peer1" {
		t.Fatalf("info = %+v", info)
	}
	return clientErr, serverErr, node
}

func TestHandshake(t *testing.T) {
	tok := NewToken()
	cErr, sErr, node := handshake(t, tok, map[string][]byte{tok.NodeID: tok.Secret})
	if cErr != nil || sErr != nil || node != tok.NodeID {
		t.Fatalf("ok handshake: %v %v %q", cErr, sErr, node)
	}

	// Unknown node.
	cErr, sErr, _ = handshake(t, tok, map[string][]byte{})
	if !errors.Is(cErr, ErrAuth) || !errors.Is(sErr, ErrAuth) {
		t.Fatalf("unknown node: %v %v", cErr, sErr)
	}

	// Controller with a different secret: the peer must refuse to continue,
	// i.e. never send its own proof.
	other := NewTokenFor(tok.NodeID)
	cErr, sErr, _ = handshake(t, tok, map[string][]byte{tok.NodeID: other.Secret})
	if !errors.Is(cErr, ErrControllerAuth) || sErr == nil {
		t.Fatalf("fake controller: %v %v", cErr, sErr)
	}
}

func TestHandshakeVersionMismatch(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		WriteFrame(a, hello{V: 99, Node: "aaaaaaaaaaaa", Nonce: make([]byte, nonceSize)})
		var ch challenge
		ReadFrame(a, &ch)
		a.Close()
	}()
	_, _, err := ServerHandshake(b, func(string) ([]byte, error) { return []byte("x"), nil })
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("err = %v", err)
	}
}
