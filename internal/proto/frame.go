// Package proto contains the wire format shared by controller and peer:
// length-prefixed JSON frames, chunked binary data, and the authentication
// handshake.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxFrame limits a single JSON frame; protects against memory exhaustion.
const MaxFrame = 16 << 20

// MaxChunk is the largest binary chunk.
const MaxChunk = 1 << 20

// ErrFrameTooLarge is returned for frames above MaxFrame.
var ErrFrameTooLarge = errors.New("proto: frame too large")

// WriteFrame writes v as a 4-byte big-endian length followed by JSON.
func WriteFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

// ReadFrame reads one frame into v.
func ReadFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return ErrFrameTooLarge
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return unexpected(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("proto: bad frame: %w", err)
	}
	return nil
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// ChunkWriter frames binary data as chunks: 4-byte length + data. Close
// writes the zero-length terminator; it does not close the underlying writer.
type ChunkWriter struct {
	W      io.Writer
	closed bool
}

func (c *ChunkWriter) Write(p []byte) (int, error) {
	if c.closed {
		return 0, errors.New("proto: write after close")
	}
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > MaxChunk {
			n = MaxChunk
		}
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(n))
		if _, err := c.W.Write(hdr[:]); err != nil {
			return written, err
		}
		m, err := c.W.Write(p[:n])
		written += m
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

// Close writes the terminator.
func (c *ChunkWriter) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	var hdr [4]byte
	_, err := c.W.Write(hdr[:])
	return err
}

// ChunkReader reads data written by ChunkWriter and returns io.EOF at the
// terminator, leaving the underlying reader positioned after it.
type ChunkReader struct {
	R    io.Reader
	left uint32
	done bool
}

func (c *ChunkReader) Read(p []byte) (int, error) {
	if c.done {
		return 0, io.EOF
	}
	if c.left == 0 {
		var hdr [4]byte
		if _, err := io.ReadFull(c.R, hdr[:]); err != nil {
			return 0, unexpected(err)
		}
		c.left = binary.BigEndian.Uint32(hdr[:])
		if c.left == 0 {
			c.done = true
			return 0, io.EOF
		}
		if c.left > MaxChunk {
			return 0, ErrFrameTooLarge
		}
	}
	if uint32(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.R.Read(p)
	c.left -= uint32(n)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// Drain discards the remaining chunks up to the terminator.
func (c *ChunkReader) Drain() error {
	_, err := io.Copy(io.Discard, c)
	return err
}
