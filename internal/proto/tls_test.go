package proto

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

func newTLS(t *testing.T) *TLS {
	t.Helper()
	s, err := NewTLS()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// connect runs client and server Security over a pipe, then the handshake.
func connect(t *testing.T, client, server Security, tok Token, secret []byte) (cErr, sErr error, transport string) {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := client.Client(a)
		if err != nil {
			cErr = err
			a.Close()
			return
		}
		cErr = ClientHandshake(c, tok, PeerInfo{Hostname: "peer1"})
		a.Close()
	}()
	s, err := server.Server(b)
	if err != nil {
		b.Close()
		<-done
		return cErr, err, ""
	}
	transport = Transport(s)
	_, _, sErr = ServerHandshake(s, func(string) ([]byte, error) { return secret, nil })
	b.Close()
	<-done
	return cErr, sErr, transport
}

func TestTLSHandshake(t *testing.T) {
	tok := NewToken()
	auto := Auto{TLS: newTLS(t)}
	cErr, sErr, tr := connect(t, &TLS{}, auto, tok, tok.Secret)
	if cErr != nil || sErr != nil || tr != "tls" {
		t.Fatalf("tls: %v %v %q", cErr, sErr, tr)
	}
	// Auto also takes plain peers.
	cErr, sErr, tr = connect(t, Plain{}, auto, tok, tok.Secret)
	if cErr != nil || sErr != nil || tr != "plain" {
		t.Fatalf("plain via auto: %v %v %q", cErr, sErr, tr)
	}
	// Wrong secret over TLS is still refused.
	other := NewTokenFor(tok.NodeID)
	cErr, _, _ = connect(t, &TLS{}, auto, tok, other.Secret)
	if !errors.Is(cErr, ErrControllerAuth) {
		t.Fatalf("wrong secret: %v", cErr)
	}
}

// oldPeer speaks protocol version 1, as peers before TLS did.
func oldPeer(rw io.ReadWriter, tok Token) error {
	np := nonce()
	if err := WriteFrame(rw, hello{V: 1, Node: tok.NodeID, Nonce: np}); err != nil {
		return err
	}
	var ch challenge
	if err := ReadFrame(rw, &ch); err != nil {
		return err
	}
	if ch.Err != "" {
		return errors.New(ch.Err)
	}
	if string(ch.Proof) != string(proof(tok.Secret, 1, "controller", tok.NodeID, np, ch.Nonce, nil)) {
		return ErrControllerAuth
	}
	if err := WriteFrame(rw, answer{Proof: proof(tok.Secret, 1, "peer", tok.NodeID, np, ch.Nonce, nil)}); err != nil {
		return err
	}
	var res result
	if err := ReadFrame(rw, &res); err != nil {
		return err
	}
	if res.Err != "" {
		return errors.New(res.Err)
	}
	return nil
}

func TestOldPlainPeerAccepted(t *testing.T) {
	tok := NewToken()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	errc := make(chan error, 1)
	go func() { errc <- oldPeer(a, tok) }()
	s, err := Auto{TLS: newTLS(t)}.Server(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ServerHandshake(s, func(string) ([]byte, error) { return tok.Secret, nil }); err != nil {
		t.Fatalf("server: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("old peer: %v", err)
	}
}

// A man in the middle terminates the peer's TLS with his own certificate
// and relays the bytes to the real controller. The peer must notice before
// it sends its proof.
func TestTLSManInTheMiddle(t *testing.T) {
	for _, upstream := range []string{"plain", "tls"} {
		t.Run(upstream, func(t *testing.T) {
			tok := NewToken()
			peerSide, mitmIn := net.Pipe()
			mitmOut, ctrlSide := net.Pipe()
			defer peerSide.Close()
			defer ctrlSide.Close()
			mitm, ctrl := newTLS(t), newTLS(t)

			go func() {
				in, err := mitm.Server(mitmIn)
				if err != nil {
					mitmIn.Close()
					return
				}
				var out net.Conn = mitmOut
				if upstream == "tls" {
					if out, err = (&TLS{}).Client(mitmOut); err != nil {
						in.Close()
						return
					}
				}
				go func() { io.Copy(out, in); out.Close() }()
				io.Copy(in, out)
				in.Close()
			}()
			serverDone := make(chan error, 1)
			go func() {
				s, err := Auto{TLS: ctrl}.Server(ctrlSide)
				if err != nil {
					serverDone <- err
					return
				}
				_, _, err = ServerHandshake(s, func(string) ([]byte, error) { return tok.Secret, nil })
				ctrlSide.Close()
				serverDone <- err
			}()

			c, err := (&TLS{}).Client(peerSide)
			if err != nil {
				t.Fatal(err)
			}
			if err := ClientHandshake(c, tok, PeerInfo{}); !errors.Is(err, ErrControllerAuth) {
				t.Fatalf("peer accepted a relayed handshake: %v", err)
			}
			peerSide.Close()
			if err := <-serverDone; err == nil {
				t.Fatal("controller accepted the relayed peer")
			}
		})
	}
}

func TestVersion1OverTLSRefused(t *testing.T) {
	tok := NewToken()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	errc := make(chan error, 1)
	go func() {
		c, err := (&TLS{}).Client(a)
		if err != nil {
			errc <- err
			return
		}
		errc <- oldPeer(c, tok)
	}()
	s, err := newTLS(t).Server(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ServerHandshake(s, func(string) ([]byte, error) { return tok.Secret, nil }); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("server: %v", err)
	}
	if err := <-errc; err == nil {
		t.Fatal("v1 over TLS accepted")
	}
}

func TestRefuseHandshake(t *testing.T) {
	tok := NewToken()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	errc := make(chan error, 1)
	go func() { errc <- ClientHandshake(a, tok, PeerInfo{}) }()
	if node := RefuseHandshake(b, "only encrypted connections"); node != tok.NodeID {
		t.Fatalf("node = %q", node)
	}
	if err := <-errc; !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "only encrypted") {
		t.Fatalf("client: %v", err)
	}
}
