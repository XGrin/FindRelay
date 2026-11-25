package diagnostics

import (
	"bytes"
	"strings"
	"testing"
)

func TestLineWriterEmitsWholeLinesAndFlushesTheRemainder(t *testing.T) {
	var lines []string
	w := NewLineWriter(func(line string) { lines = append(lines, line) })

	text := &bytes.Buffer{}
	text.WriteString("time=now level=WARN msg=first\nfind")
	text.WriteString("relay run: sec")
	text.WriteString("ond line\n\nlast")
	if n, err := w.Write(text.Bytes()); err != nil || n != text.Len() {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, text.Len())
	}
	if strings.Join(lines, "|") != "time=now level=WARN msg=first|findrelay run: second line|" {
		t.Fatalf("lines before Close = %q", lines)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if strings.Join(lines, "|") != "time=now level=WARN msg=first|findrelay run: second line||last" {
		t.Fatalf("lines after Close = %q", lines)
	}
}
