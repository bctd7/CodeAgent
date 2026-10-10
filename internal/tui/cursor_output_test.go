package tui

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

type frameWriteSink struct {
	bytes.Buffer
	writes []string
}

func (s *frameWriteSink) Write(p []byte) (int, error) {
	s.writes = append(s.writes, string(p))
	return s.Buffer.Write(p)
}

func TestCursorFrameDoesNotFlushIntermediateBottomPosition(t *testing.T) {
	marker := fmt.Sprintf("%s7;19;24;80;1\a", cursorFramePrefix)
	bottom := ansi.CursorPosition(0, 24)
	frame := ansi.CursorHomePosition + "中文 abc" + marker + ansi.EraseLineRight + bottom
	want := ansi.CursorHomePosition + "中文 abc" + ansi.EraseLineRight + ansi.CursorPosition(7, 19)
	for _, size := range []int{1, 2, 7, len(frame)} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			sink := &frameWriteSink{}
			out := newCursorOutput(sink)
			for i := 0; i < len(frame); i += size {
				part := []byte(frame[i:min(i+size, len(frame))])
				if n, err := out.Write(part); err != nil || n != len(part) {
					t.Fatalf("write: %d %v", n, err)
				}
			}
			if sink.String() != want {
				t.Fatalf("output = %q, want %q", sink.String(), want)
			}
			if strings.Contains(sink.String(), bottom) {
				t.Fatal("intermediate bottom-row position reached terminal")
			}
			if size == len(frame) && len(sink.writes) != 1 {
				t.Fatalf("complete frame uses %d terminal writes", len(sink.writes))
			}
			inactive := "menu" + fmt.Sprintf("%s0;0;24;80;2\a", cursorFramePrefix) + bottom
			cleanup := ansi.ShowCursor + "\x1b[?1049l"
			if _, err := out.Write([]byte(inactive)); err != nil {
				t.Fatal(err)
			}
			if _, err := out.Write([]byte(cleanup)); err != nil {
				t.Fatal(err)
			}
			if sink.String() != want+"menu"+bottom+cleanup {
				t.Fatal("menu or lifecycle output changed")
			}
		})
	}
}
