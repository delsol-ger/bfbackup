package proto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
)

// ProtocolVersion is bumped on incompatible wire changes. Version 2 binds
// the handshake to the TLS session; the controller still accepts version 1
// peers over plain connections.
const (
	ProtocolVersion    = 2
	minProtocolVersion = 1
)

const nonceSize = 32

// Errors returned by the handshake.
var (
	ErrAuth           = errors.New("authentication failed")
	ErrControllerAuth = errors.New("controller failed to authenticate (wrong token, or not the real controller)")
	ErrBadToken       = errors.New("malformed token, expected bfb1.<node-id>.<secret>")
)

var nodeIDRE = regexp.MustCompile(`^[a-f0-9]{12}$`)

// Token is a peer's credential: which node it is and the shared secret.
type Token struct {
	NodeID string
	Secret []byte
}

// NewToken creates a token with a random node ID and a 256-bit secret.
func NewToken() Token {
	id := make([]byte, 6)
	rand.Read(id)
	return NewTokenFor(hex.EncodeToString(id))
}

// NewTokenFor creates a new secret for an existing node ID.
func NewTokenFor(nodeID string) Token {
	secret := make([]byte, 32)
	rand.Read(secret)
	return Token{NodeID: nodeID, Secret: secret}
}

// String encodes the token for BFB_TOKEN.
func (t Token) String() string {
	return "bfb1." + t.NodeID + "." + base64.RawURLEncoding.EncodeToString(t.Secret)
}

// ParseToken decodes a token string.
func ParseToken(s string) (Token, error) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 3 || parts[0] != "bfb1" || !nodeIDRE.MatchString(parts[1]) {
		return Token{}, ErrBadToken
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(secret) != 32 {
		return Token{}, ErrBadToken
	}
	return Token{NodeID: parts[1], Secret: secret}, nil
}

// ValidNodeID reports whether id has the format of a generated node ID.
func ValidNodeID(id string) bool { return nodeIDRE.MatchString(id) }

// PeerInfo is what a peer tells the controller about itself.
type PeerInfo struct {
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
}

type hello struct {
	V     int      `json:"v"`
	Node  string   `json:"node"`
	Nonce []byte   `json:"nonce"`
	Info  PeerInfo `json:"info"`
}

type challenge struct {
	Nonce []byte `json:"nonce,omitempty"`
	Proof []byte `json:"proof,omitempty"`
	Err   string `json:"err,omitempty"`
}

type answer struct {
	Proof []byte `json:"proof"`
}

type result struct {
	Err string `json:"err,omitempty"`
}

// proof = HMAC-SHA256(secret, label | role | node | nonce_peer | nonce_ctrl
// [| binding]), every field length-prefixed so fields cannot be shifted into
// each other. Version 1 has no binding field; from version 2 on it is the
// TLS channel binding, empty on plain connections.
func proof(secret []byte, v int, role, node string, np, nc, bind []byte) []byte {
	fields := [][]byte{[]byte("bfbackup-auth-v1"), []byte(role), []byte(node), np, nc}
	if v >= 2 {
		fields[0] = []byte("bfbackup-auth-v2")
		fields = append(fields, bind)
	}
	m := hmac.New(sha256.New, secret)
	for _, f := range fields {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(f)))
		m.Write(l[:])
		m.Write(f)
	}
	return m.Sum(nil)
}

func nonce() []byte {
	b := make([]byte, nonceSize)
	rand.Read(b)
	return b
}

// ClientHandshake authenticates a peer to the controller over rw. The
// controller has to prove itself first; the peer never answers a controller
// that does not know its secret. Over TLS the proofs are bound to the
// session, which also authenticates the controller's end of the tunnel.
func ClientHandshake(rw io.ReadWriter, tok Token, info PeerInfo) error {
	bind, err := binding(rw)
	if err != nil {
		return err
	}
	np := nonce()
	if err := WriteFrame(rw, hello{V: ProtocolVersion, Node: tok.NodeID, Nonce: np, Info: info}); err != nil {
		return err
	}
	var ch challenge
	if err := ReadFrame(rw, &ch); err != nil {
		return err
	}
	if ch.Err != "" {
		return fmt.Errorf("%w: controller says: %s", ErrAuth, ch.Err)
	}
	if len(ch.Nonce) != nonceSize || !hmac.Equal(ch.Proof, proof(tok.Secret, ProtocolVersion, "controller", tok.NodeID, np, ch.Nonce, bind)) {
		return ErrControllerAuth
	}
	if err := WriteFrame(rw, answer{Proof: proof(tok.Secret, ProtocolVersion, "peer", tok.NodeID, np, ch.Nonce, bind)}); err != nil {
		return err
	}
	var res result
	if err := ReadFrame(rw, &res); err != nil {
		return err
	}
	if res.Err != "" {
		return fmt.Errorf("%w: %s", ErrAuth, res.Err)
	}
	return nil
}

// ServerHandshake authenticates a connecting peer. lookup returns the secret
// for a node ID, or an error if the node is unknown.
func ServerHandshake(rw io.ReadWriter, lookup func(nodeID string) ([]byte, error)) (string, PeerInfo, error) {
	bind, err := binding(rw)
	if err != nil {
		return "", PeerInfo{}, err
	}
	var h hello
	if err := ReadFrame(rw, &h); err != nil {
		return "", PeerInfo{}, err
	}
	if h.V < minProtocolVersion || h.V > ProtocolVersion || (h.V < 2 && bind != nil) {
		msg := fmt.Sprintf("protocol version %d not supported (controller speaks %d to %d); update both containers", h.V, minProtocolVersion, ProtocolVersion)
		WriteFrame(rw, challenge{Err: msg})
		return "", PeerInfo{}, errors.New(msg)
	}
	if !nodeIDRE.MatchString(h.Node) || len(h.Nonce) != nonceSize {
		WriteFrame(rw, challenge{Err: "malformed hello"})
		return "", PeerInfo{}, ErrAuth
	}
	secret, err := lookup(h.Node)
	if err != nil || len(secret) == 0 {
		WriteFrame(rw, challenge{Err: "unknown node; add the peer in the web UI and use its token"})
		return h.Node, PeerInfo{}, fmt.Errorf("%w: unknown node %s", ErrAuth, h.Node)
	}
	nc := nonce()
	if err := WriteFrame(rw, challenge{Nonce: nc, Proof: proof(secret, h.V, "controller", h.Node, h.Nonce, nc, bind)}); err != nil {
		return "", PeerInfo{}, err
	}
	var a answer
	if err := ReadFrame(rw, &a); err != nil {
		return h.Node, PeerInfo{}, err
	}
	if !hmac.Equal(a.Proof, proof(secret, h.V, "peer", h.Node, h.Nonce, nc, bind)) {
		WriteFrame(rw, result{Err: "invalid token"})
		return h.Node, PeerInfo{}, fmt.Errorf("%w: bad proof from node %s", ErrAuth, h.Node)
	}
	if err := WriteFrame(rw, result{}); err != nil {
		return "", PeerInfo{}, err
	}
	return h.Node, h.Info, nil
}

// RefuseHandshake reads a peer's hello and answers it with msg instead of a
// challenge, so the peer logs why it was turned away. It returns the node the
// peer claimed to be (unauthenticated).
func RefuseHandshake(rw io.ReadWriter, msg string) string {
	var h hello
	if ReadFrame(rw, &h) != nil {
		return ""
	}
	WriteFrame(rw, challenge{Err: msg})
	if !nodeIDRE.MatchString(h.Node) {
		return ""
	}
	return h.Node
}

// Security wraps a raw connection before the handshake: Plain, TLS, or Auto
// (controller, accepts both).
type Security interface {
	Name() string
	Client(net.Conn) (net.Conn, error)
	Server(net.Conn) (net.Conn, error)
}

// Plain sends data unencrypted (trusted LAN, maximum throughput).
type Plain struct{}

func (Plain) Name() string                        { return "plain" }
func (Plain) Client(c net.Conn) (net.Conn, error) { return c, nil }
func (Plain) Server(c net.Conn) (net.Conn, error) { return c, nil }
