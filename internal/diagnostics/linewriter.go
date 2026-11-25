package diagnostics

import "bytes"

// LineWriter splits written bytes into lines and calls emit once per complete line; Close emits the trailing
// partial line that has no newline.
type LineWriter struct {
	emit    func(string)
	pending []byte
}

// NewLineWriter creates a writer that hands each line to emit.
// LineWriter itself takes no lock and does not guarantee that emit is concurrency-safe: multiple writers must first
// be serialized through the same lock before forwarding here.
func NewLineWriter(emit func(string)) *LineWriter {
	return &LineWriter{emit: emit}
}

func (w *LineWriter) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			break
		}
		w.emit(string(w.pending[:i]))
		w.pending = w.pending[i+1:]
	}
	// The remainder points at the tail of the append buffer; copy it into a new slice so it does not hold on to the
	// memory of lines already emitted.
	w.pending = append([]byte(nil), w.pending...)
	return len(p), nil
}

func (w *LineWriter) Close() error {
	if len(w.pending) > 0 {
		w.emit(string(w.pending))
		w.pending = nil
	}
	return nil
}
