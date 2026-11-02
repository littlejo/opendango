package build

import (
	"bytes"
	"io"
	"sync"
)

// prefixWriter writes each line to out with a prefix. stdout and stderr of
// a command share one, hence the lock.
type prefixWriter struct {
	mu     sync.Mutex
	out    io.Writer
	prefix string
	buf    bytes.Buffer
}

func newPrefixWriter(out io.Writer, prefix string) *prefixWriter {
	return &prefixWriter{out: out, prefix: prefix}
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadBytes('\n')
		if err != nil {
			// Incomplete line: keep it for the next write.
			w.buf.Write(line)
			return len(p), nil
		}
		if _, err := io.WriteString(w.out, w.prefix+string(line)); err != nil {
			return 0, err
		}
	}
}

// Flush writes the last line if it has no trailing newline.
func (w *prefixWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		io.WriteString(w.out, w.prefix+w.buf.String()+"\n")
		w.buf.Reset()
	}
}
