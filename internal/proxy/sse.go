package proxy

import (
	"bufio"
	"bytes"
	"io"
)

// sseReader reads the data payloads of a server-sent event stream.
type sseReader struct {
	sc *bufio.Scanner
}

func newSSEReader(r io.Reader) *sseReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	return &sseReader{sc: sc}
}

// next returns the next event's data (its data: lines joined by newlines).
// Events without data, comments and other fields are skipped. At the end of
// the stream it returns io.EOF, or the read error.
func (r *sseReader) next() ([]byte, error) {
	var data [][]byte
	for r.sc.Scan() {
		line := r.sc.Bytes()
		if len(line) == 0 {
			if data != nil {
				return bytes.Join(data, []byte("\n")), nil
			}
			continue
		}
		if v, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.Clone(bytes.TrimPrefix(v, []byte(" "))))
		}
	}
	if err := r.sc.Err(); err != nil {
		return nil, err
	}
	if data != nil {
		// A final event without its blank line.
		return bytes.Join(data, []byte("\n")), nil
	}
	return nil, io.EOF
}

func writeEvent(w io.Writer, data []byte) error {
	_, err := w.Write(append(append([]byte("data: "), data...), "\n\n"...))
	return err
}
