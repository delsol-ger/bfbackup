package rpc

import (
	"context"
	"io"
	"path"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Compression is asked for per call (request.Z) and confirmed by the other
// side (response.Z, scanFrame.Z), so old and new nodes still talk to each
// other: a node that does not know the flag ignores it and sends plain data.
// Only file data and scan listings are compressed, with zstd at its fastest
// level, which keeps up with gigabit links on small CPUs.

type compressKey struct{}

// WithCompression marks ctx so that calls made with it compress file data
// and scan listings when the peer supports it.
func WithCompression(ctx context.Context) context.Context {
	return context.WithValue(ctx, compressKey{}, true)
}

func compressOn(ctx context.Context) bool {
	on, _ := ctx.Value(compressKey{}).(bool)
	return on
}

// incompressible lists extensions of files that are already compressed;
// compressing them again only costs CPU.
var incompressible = map[string]bool{}

func init() {
	for _, e := range strings.Fields(`
		7z gz tgz bz2 tbz xz txz zst zstd lz4 lzma lz br zip rar cab arj z
		jar war apk ipa deb rpm xpi crx whl nupkg docx xlsx pptx odt ods odp epub cbz cbr
		jpg jpeg png gif webp heic heif avif jxl jp2 cr2 cr3 nef arw raf orf rw2 dng
		mp3 aac m4a ogg oga opus flac wma ape
		mp4 m4v mkv webm avi mov wmv flv ts m2ts mts mpg mpeg vob 3gp
		iso dmg squashfs pdf swf woff woff2 pack`) {
		incompressible[e] = true
	}
}

func worthCompressing(p string) bool {
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(p)), ".")
	return !incompressible[ext]
}

var encoders = sync.Pool{New: func() any {
	enc, _ := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(1<<20),
		zstd.WithLowerEncoderMem(true))
	return enc
}}

var decoders = sync.Pool{New: func() any {
	dec, _ := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxWindow(8<<20))
	return dec
}}

// zWriter compresses into w. Close flushes the end of the stream but leaves
// w open.
type zWriter struct{ enc *zstd.Encoder }

func newZWriter(w io.Writer) *zWriter {
	enc := encoders.Get().(*zstd.Encoder)
	enc.Reset(w)
	return &zWriter{enc: enc}
}

func (z *zWriter) Write(p []byte) (int, error) { return z.enc.Write(p) }
func (z *zWriter) Flush() error                { return z.enc.Flush() }

// Close ends the stream and returns the encoder to the pool.
func (z *zWriter) Close() error {
	if z.enc == nil {
		return nil
	}
	err := z.enc.Close()
	z.release()
	return err
}

// release returns the encoder without finishing the stream (aborts).
func (z *zWriter) release() {
	if z.enc != nil {
		z.enc.Reset(nil)
		encoders.Put(z.enc)
		z.enc = nil
	}
}

// zReader decompresses from r.
type zReader struct{ dec *zstd.Decoder }

func newZReader(r io.Reader) (*zReader, error) {
	dec := decoders.Get().(*zstd.Decoder)
	if err := dec.Reset(r); err != nil {
		decoders.Put(dec)
		return nil, err
	}
	return &zReader{dec: dec}, nil
}

func (z *zReader) Read(p []byte) (int, error) {
	if z.dec == nil {
		return 0, io.ErrClosedPipe
	}
	return z.dec.Read(p)
}

func (z *zReader) Close() {
	if z.dec != nil {
		z.dec.Reset(nil)
		decoders.Put(z.dec)
		z.dec = nil
	}
}
