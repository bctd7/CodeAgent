package tui

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Private frame metadata is consumed by cursorOutput, never sent to the terminal.
// A fresh sequence forces the renderer to include it even when only other rows
// change. This binds the anchor to the flushed frame rather than shared UI state.
const cursorFramePrefix = "\x1b]777;CodeAgentCursor;"
const cursorProbe = "\x1b]777;CodeAgentProbe\a"

type cursorFrame struct{ x, y, rows, width int }

func (m *Model) cursorFrameView(view string) string {
	frame := cursorFrame{rows: strings.Count(view, "\n") + 1, width: m.width}
	if m.windowFocused && m.input.Focused() && m.dialog == nil && len(m.approvals) == 0 && m.height > 1 {
		// Ask the existing textarea to render its actual scrolled/wrapped cursor.
		// No duplicate wrapping algorithm or access to component-private fields.
		probe := m.input
		probe.Cursor.Blink = false
		probe.Cursor.Style = lipgloss.NewStyle().Transform(func(s string) string { return cursorProbe + s })
		rendered := probe.View()
		if index := strings.Index(rendered, cursorProbe); index >= 0 {
			before := strings.Split(rendered[:index], "\n")
			footerRows := 2
			if m.height == 2 {
				footerRows = 1
			}
			frame.x = ansi.StringWidth(before[len(before)-1]) + 1
			frame.y = m.height - m.input.Height() - footerRows + len(before)
			if frame.x > m.width || frame.y < 1 || frame.y > frame.rows {
				frame.x, frame.y = 0, 0
			}
		}
		// The textarea copy shares its viewport; remove probe metadata from it.
		_ = m.input.View()
	}
	if m.windowFocused && len(m.approvals) == 0 && m.dialog != nil && m.dialog.form != nil {
		d := m.dialog
		if len(d.fields) > 0 && d.selected < len(d.fields) && !d.form.Fields[d.selected].Boolean && len(d.form.Fields[d.selected].Options) == 0 {
			row := 0
			for i := 0; i < d.selected; i++ {
				label := ansi.Hardwrap(safeText(d.form.Fields[i].Label), max(1, d.body.Width), true)
				row += strings.Count(label, "\n") + strings.Count(d.fields[i].View(), "\n") + 3
			}
			label := ansi.Hardwrap(safeText(d.form.Fields[d.selected].Label), max(1, d.body.Width), true)
			probe := d.fields[d.selected]
			probe.Cursor.Blink = false
			probe.Cursor.Style = lipgloss.NewStyle().Transform(func(s string) string { return cursorProbe + s })
			rendered := probe.View()
			if index := strings.Index(rendered, cursorProbe); index >= 0 {
				before := strings.Split(rendered[:index], "\n")
				frame.x = ansi.StringWidth(before[len(before)-1]) + 1
				frame.y = 2 + row + strings.Count(label, "\n") + len(before) - d.body.YOffset
				if frame.y < 2 || frame.y > 1+d.body.Height || frame.y > frame.rows || frame.x > m.width {
					frame.x, frame.y = 0, 0
				}
			}
		}
	}
	m.cursorFrame++
	return view + fmt.Sprintf("%s%d;%d;%d;%d;%d\a", cursorFramePrefix, frame.x, frame.y, frame.rows, frame.width, m.cursorFrame)
}

// Bubble Tea's alt-screen renderer writes a complete frame ending with CUP to
// column 1 of its last row. This adapter consumes the private frame annotation
// and replaces that suffix with the input anchor before writing the frame.
// It does not draw or diff the screen, and lifecycle writes remain unchanged.
type cursorOutput struct {
	mu      sync.Mutex
	out     io.Writer
	carry   []byte
	tail    string
	pending *cursorFrame
	err     error
}

type cursorTerminal interface {
	io.ReadWriteCloser
	Fd() uintptr
}
type cursorTerminalOutput struct {
	*cursorOutput
	file cursorTerminal
}

func (w *cursorTerminalOutput) Read(p []byte) (int, error) { return w.file.Read(p) }
func (w *cursorTerminalOutput) Close() error               { return w.file.Close() }
func (w *cursorTerminalOutput) Fd() uintptr                { return w.file.Fd() }
func newCursorOutput(out io.Writer) io.Writer {
	writer := &cursorOutput{out: out}
	if file, ok := out.(cursorTerminal); ok {
		return &cursorTerminalOutput{cursorOutput: writer, file: file}
	}
	return writer
}

func (w *cursorOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	data := append(w.carry, p...)
	w.carry = nil
	var clean bytes.Buffer
	markerRead, afterMarker := false, 0
	for len(data) > 0 {
		index := bytes.Index(data, []byte(cursorFramePrefix))
		if index < 0 {
			keep := 0
			for size := 1; size < len(cursorFramePrefix) && size <= len(data); size++ {
				if bytes.Equal(data[len(data)-size:], []byte(cursorFramePrefix[:size])) {
					keep = size
				}
			}
			clean.Write(data[:len(data)-keep])
			w.carry = append([]byte(nil), data[len(data)-keep:]...)
			break
		}
		clean.Write(data[:index])
		payload := data[index+len(cursorFramePrefix):]
		end := bytes.IndexByte(payload, '\a')
		if end < 0 {
			if len(payload) > 128 {
				w.err = fmt.Errorf("invalid TUI cursor frame")
				return 0, w.err
			}
			w.carry = append([]byte(nil), data[index:]...)
			break
		}
		w.pending = parseCursorFrame(string(payload[:end]))
		markerRead, afterMarker = true, clean.Len()
		data = payload[end+1:]
	}
	output := clean.String()
	if markerRead {
		w.tail = ""
	}
	if frame := w.pending; frame != nil {
		// Hold the annotation's trailing bytes across split writes. Never flush
		// the renderer's bottom-row CUP separately from the input anchor:
		// Windows IME can observe that intermediate physical cursor position.
		w.tail += output[afterMarker:]
		output = output[:afterMarker]
		suffix := ansi.CursorPosition(0, frame.rows)
		if strings.HasSuffix(w.tail, suffix) {
			if frame.x > 0 && frame.y > 0 {
				w.tail = strings.TrimSuffix(w.tail, suffix) + ansi.CursorPosition(frame.x, frame.y)
			}
			output += w.tail
			w.pending, w.tail = nil, ""
		} else if len(w.tail) > 256 {
			// Malformed/incomplete annotations must not retain lifecycle output
			// indefinitely. Valid renderer trailers are only a few ANSI bytes.
			output += w.tail
			w.pending, w.tail = nil, ""
		}
	}
	if err := w.writeAll([]byte(output)); err != nil {
		w.err = err
		return 0, err
	}
	return len(p), nil
}
func parseCursorFrame(payload string) *cursorFrame {
	fields := strings.Split(payload, ";")
	if len(fields) != 5 {
		return nil
	}
	values := make([]int, len(fields))
	for i, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil || n < 0 {
			return nil
		}
		values[i] = n
	}
	f := &cursorFrame{x: values[0], y: values[1], rows: values[2], width: values[3]}
	if f.rows < 1 || f.width < 1 || f.x > f.width || f.y > f.rows || (f.x == 0) != (f.y == 0) {
		return nil
	}
	return f
}
func (w *cursorOutput) writeAll(p []byte) error {
	for len(p) > 0 {
		n, err := w.out.Write(p)
		if n < 0 || n > len(p) {
			return io.ErrShortWrite
		}
		p = p[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
