package watch

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteFrameUsesCRAndClearsEachLine(t *testing.T) {
	var buf bytes.Buffer
	writeFrame(&buf, "hello\nworld")
	got := buf.String()
	want := homeCursor + "hello" + clearEOL + "\r\n" + "world" + clearEOL + clearEOS
	if got != want {
		t.Errorf("writeFrame:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "hello\n") {
		t.Errorf("raw frame must not emit a bare LF (ONLCR is off): %q", got)
	}
}

func TestWriteFrameClearsLeftoverRowsWithoutScrollingLastLine(t *testing.T) {
	var buf bytes.Buffer
	writeFrame(&buf, "only")
	got := buf.String()
	if !strings.HasPrefix(got, homeCursor) {
		t.Errorf("frame should home the cursor: %q", got)
	}
	if !strings.HasSuffix(got, clearEOL+clearEOS) {
		t.Errorf("last line should erase to EOL then EOS, with no trailing LF: %q", got)
	}
	if strings.Count(got, "\n") != 0 {
		t.Errorf("single-line frame must not LF (that scrolls a full-height view): %q", got)
	}
}
