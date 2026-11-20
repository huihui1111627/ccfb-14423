package layer

import (
	"archive/tar"
	"bytes"
	"io"
	"time"
)

type tarWriter struct{ w *tar.Writer }

func newTarWriter(b *bytes.Buffer) *tarWriter { return &tarWriter{w: tar.NewWriter(b)} }

func (t *tarWriter) add(name string, data []byte) error {
	h := &tar.Header{
		Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data)),
		ModTime: time.Unix(1700000000, 0), Format: tar.FormatPAX,
	}
	if err := t.w.WriteHeader(h); err != nil {
		return err
	}
	_, err := t.w.Write(data)
	return err
}

func (t *tarWriter) close() error { return t.w.Close() }

func readAll(r io.Reader) ([]byte, error) { return io.ReadAll(r) }
