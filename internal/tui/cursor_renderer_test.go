package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type shortCursorSink struct {
	bytes.Buffer
	limit int
}

func (s *shortCursorSink) Write(p []byte) (int, error) {
	if len(p) > s.limit {
		p = p[:s.limit]
	}
	return s.Buffer.Write(p)
}

type zeroCursorSink struct{ calls int }

func (s *zeroCursorSink) Write([]byte) (int, error) { s.calls++; return 0, nil }

type fakeCursorFile struct{ bytes.Buffer }

func (*fakeCursorFile) Close() error { return nil }
func (*fakeCursorFile) Fd() uintptr  { return 123 }

func TestIMEOutputFrameSplitsShortWritesAndCleanup(t *testing.T) {
	marker := fmt.Sprintf("%s7;19;24;80;1\a", cursorFramePrefix)
	frame := ansi.CursorHomePosition + "frame" + marker + ansi.CursorPosition(0, 24)
	want := ansi.CursorHomePosition + "frame" + ansi.CursorPosition(7, 19)
	for _, chunk := range []int{1, 2, 7, len(frame)} {
		t.Run(fmt.Sprint(chunk), func(t *testing.T) {
			sink := &shortCursorSink{limit: 3}
			w := newCursorOutput(sink)
			for start := 0; start < len(frame); start += chunk {
				part := []byte(frame[start:min(start+chunk, len(frame))])
				n, err := w.Write(part)
				if err != nil || n != len(part) {
					t.Fatalf("write %d %v", n, err)
				}
			}
			if sink.String() != want {
				t.Fatalf("wrong final physical cursor output: %q", sink.String())
			}
			inactive := "modal" + fmt.Sprintf("%s0;0;5;80;2\a", cursorFramePrefix) + ansi.CursorPosition(0, 5)
			w.Write([]byte(inactive))
			cleanup := ansi.ShowCursor + "\x1b[?1049l"
			w.Write([]byte(cleanup))
			if sink.String() != want+"modal"+ansi.CursorPosition(0, 5)+cleanup || strings.Contains(sink.String(), "CodeAgent") {
				t.Fatal("modal/cleanup output changed or private marker leaked")
			}
		})
	}
	zero := &zeroCursorSink{}
	w := newCursorOutput(zero)
	if _, err := w.Write([]byte(frame)); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero write not rejected: %v", err)
	}
	if _, err := w.Write([]byte("later")); !errors.Is(err, io.ErrShortWrite) || zero.calls != 1 {
		t.Fatal("failed output retried or error hidden")
	}
	file := &fakeCursorFile{}
	wrapped := newCursorOutput(file)
	if fd, ok := wrapped.(cursorTerminal); !ok || fd.Fd() != file.Fd() {
		t.Fatal("terminal file interface lost")
	}
}

func TestIMEOutputSerializesFrames(t *testing.T) {
	var sink bytes.Buffer
	w := newCursorOutput(&sink)
	var workers sync.WaitGroup
	for i := 0; i < 12; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			frame := fmt.Sprintf("F%d%s%d;19;24;80;%d\a%s", i, cursorFramePrefix, i+1, i+1, ansi.CursorPosition(0, 24))
			if _, err := w.Write([]byte(frame)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	workers.Wait()
	output := sink.String()
	for i := 0; i < 12; i++ {
		want := fmt.Sprintf("F%d%s", i, ansi.CursorPosition(i+1, 19))
		if !strings.Contains(output, want) {
			t.Fatalf("interleaved frame %d: %q", i, output)
		}
	}
}

type captureCursorSink struct {
	mu      sync.Mutex
	data    bytes.Buffer
	changed chan struct{}
}

func (s *captureCursorSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	n, err := s.data.Write(p)
	s.mu.Unlock()
	select {
	case s.changed <- struct{}{}:
	default:
	}
	return n, err
}
func (s *captureCursorSink) snapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.String()
}

var cupPattern = regexp.MustCompile("\x1b\\[([0-9]*);([0-9]*)H")

func lastCursorPosition(output string) (x, y int) {
	matches := cupPattern.FindAllStringSubmatch(output, -1)
	if len(matches) > 0 {
		v := matches[len(matches)-1]
		y, _ = strconv.Atoi(v[1])
		x, _ = strconv.Atoi(v[2])
		if x == 0 {
			x = 1
		}
		if y == 0 {
			y = 1
		}
	}
	return
}
func waitCursor(t *testing.T, s *captureCursorSink, offset int, predicate func(int, int) bool) {
	t.Helper()
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	for {
		text := s.snapshot()
		x, y := lastCursorPosition(text)
		if len(text) > offset && predicate(x, y) {
			return
		}
		select {
		case <-s.changed:
		case <-deadline.C:
			t.Fatalf("renderer physical cursor=%d,%d, output tail=%q", x, y, text[max(0, len(text)-180):])
		}
	}
}

func TestIMEActualBubbleTeaPhysicalCursor(t *testing.T) {
	// Run the real standard renderer. KeyMsg represents committed text, not an
	// actual Windows IME composition; the latter remains a separate manual check.
	for _, tracking := range []bool{false, true} {
		t.Run(fmt.Sprint(tracking), func(t *testing.T) {
			cfg := Config{Initial: Snapshot{Status: Status{SessionID: "s", RunID: "r", Model: "p/m", ModelName: "DeepSeek V4 Pro"}}}
			model := New(context.Background(), cfg)
			model.cursorTracking = tracking
			sink := &captureCursorSink{changed: make(chan struct{}, 128)}
			var out io.Writer = sink
			if tracking {
				out = newCursorOutput(sink)
			}
			p := tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(out), tea.WithAltScreen(), tea.WithReportFocus(), tea.WithFPS(60))
			finished := make(chan error, 1)
			go func() { _, err := p.Run(); finished <- err }()
			defer func() {
				p.Send(ExitEvent{})
				select {
				case err := <-finished:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(4 * time.Second):
					t.Error("program did not restore/exit")
				}
			}()
			p.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
			if !tracking {
				waitCursor(t, sink, 0, func(x, y int) bool { return x == 1 && y == 24 })
				return
			}
			waitCursor(t, sink, 0, func(x, y int) bool { return x == 3 && y == 19 })
			send := func(msg tea.Msg, predicate func(int, int) bool) {
				offset := len(sink.snapshot())
				p.Send(msg)
				waitCursor(t, sink, offset, predicate)
			}
			send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中文")}, func(x, y int) bool { return x == 7 && y == 19 })
			send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("abc")}, func(x, y int) bool { return x == 10 && y == 19 })
			send(tea.KeyMsg{Type: tea.KeyCtrlJ}, func(x, y int) bool { return x == 3 && y == 20 })
			send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("混合A")}, func(x, y int) bool { return x == 8 && y == 20 })
			// Unchanged input metadata must still appear in streamed repaint frames.
			send(DeltaEvent{Scope: Scope{SessionID: "s", RunID: "r"}, ItemID: "answer", Text: "持续中文输出"}, func(x, y int) bool { return x == 8 && y == 20 })
			send(PanelEvent{Title: "/status", Text: "inspect\nonly"}, func(x, y int) bool { return x == 1 })
			send(tea.KeyMsg{Type: tea.KeyEsc}, func(x, y int) bool { return x == 8 && y == 20 })
			send(tea.BlurMsg{}, func(x, y int) bool { return x == 1 && y == 24 })
			send(tea.FocusMsg{}, func(x, y int) bool { return x == 8 && y == 20 })
			send(ApprovalEvent{Scope: Scope{SessionID: "s", RunID: "r"}, Approval: Approval{ID: "a", Tool: "write", Parameters: "full approval parameters"}}, func(x, y int) bool { return x == 1 })
			send(ApprovalResolvedEvent{Scope: Scope{SessionID: "s", RunID: "r"}, ID: "a"}, func(x, y int) bool { return x == 8 && y == 20 })
			send(tea.WindowSizeMsg{Width: 8, Height: 8}, func(x, y int) bool { return x >= 1 && x <= 8 && y >= 4 && y <= 6 })
			send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中")}, func(x, y int) bool { return x >= 1 && x <= 8 && y >= 4 && y <= 6 })
			send(tea.KeyMsg{Type: tea.KeyCtrlJ}, func(x, y int) bool { return x == 3 && y >= 4 && y <= 6 })
			send(tea.WindowSizeMsg{Width: 20, Height: 12}, func(x, y int) bool { return x == 3 && y >= 7 && y <= 10 })
			send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(strings.Repeat("中文\n", 15) + "END"), Paste: true}, func(x, y int) bool { return x == 6 && y >= 7 && y <= 10 })
			if strings.Contains(sink.snapshot(), "CodeAgentCursor") || strings.Contains(sink.snapshot(), "CodeAgentProbe") {
				t.Fatal("internal cursor metadata reached terminal")
			}
		})
	}
}
