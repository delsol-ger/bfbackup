package proto

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"time"
)

// TLS encrypts the connection with TLS 1.3. Certificates are not checked:
// the handshake mixes keying material exported from the TLS session into its
// proofs (channel binding). A man in the middle has a different TLS session
// with each side, so the proofs he relays do not match and the peer stops
// before it answers. Peers need nothing but their token.
type TLS struct {
	cert *tls.Certificate // controller only
}

// NewTLS creates the controller side with a fresh self-signed certificate.
// It lives only in memory; nobody verifies it, so it may change on restart.
func NewTLS() (*TLS, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject:      pkix.Name{CommonName: "bfbackup controller"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &TLS{cert: &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}}, nil
}

func (*TLS) Name() string { return "tls" }

func (*TLS) Client(c net.Conn) (net.Conn, error) {
	tc := tls.Client(c, &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: "bfbackup",
		// The controller is authenticated by the channel bound handshake,
		// see the TLS type.
		InsecureSkipVerify: true,
	})
	if err := tc.Handshake(); err != nil {
		return nil, err
	}
	return tc, nil
}

func (t *TLS) Server(c net.Conn) (net.Conn, error) {
	if t == nil || t.cert == nil {
		return nil, errors.New("tls: no certificate, use NewTLS")
	}
	tc := tls.Server(c, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{*t.cert}})
	if err := tc.Handshake(); err != nil {
		return nil, err
	}
	return tc, nil
}

// Auto is the controller side that accepts both TLS and unencrypted peers
// (older versions, or BFB_TRANSPORT=plain). The first byte tells them apart:
// a TLS connection starts with a handshake record (0x16), a plain one with
// the length of the hello frame, whose first byte is 0 (frames < 16 MiB).
type Auto struct {
	TLS *TLS
}

func (Auto) Name() string { return "tls+plain" }

func (a Auto) Client(c net.Conn) (net.Conn, error) { return a.TLS.Client(c) }

func (a Auto) Server(c net.Conn) (net.Conn, error) {
	br := bufio.NewReader(c)
	b, err := br.Peek(1)
	if err != nil {
		return nil, err
	}
	pc := &peekedConn{Conn: c, r: br}
	if b[0] == 0x16 {
		return a.TLS.Server(pc)
	}
	return pc, nil
}

// peekedConn reads the bytes Auto peeked at before the rest of the stream.
type peekedConn struct {
	net.Conn
	r io.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// Transport names the transport of a connection returned by a Security.
func Transport(c net.Conn) string {
	if _, ok := c.(*tls.Conn); ok {
		return "tls"
	}
	return "plain"
}

// binding returns the channel binding of rw: keying material exported from
// its TLS session, or nil for plain connections.
func binding(rw io.ReadWriter) ([]byte, error) {
	tc, ok := rw.(*tls.Conn)
	if !ok {
		return nil, nil
	}
	cs := tc.ConnectionState()
	return cs.ExportKeyingMaterial("EXPORTER-bfbackup-auth", nil, 32)
}
