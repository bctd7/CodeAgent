package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/runtime"
)

// Main is a ready-to-wire process entrypoint. It returns an exit code and never calls
// os.Exit. The root executable calls it after which resource cleanup has finished.
func Main(args []string) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	info, err := os.Stdin.Stat()
	interactive := err == nil && info.Mode()&os.ModeCharDevice != 0
	return Execute(ctx, args, IO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Interactive: interactive, CloseInput: os.Stdin.Close}, Options{})
}

func execute(ctx context.Context, args []string, streams IO, options Options) (code int) {
	if ctx == nil {
		ctx = context.Background()
	}
	if streams.In == nil {
		streams.In = strings.NewReader("")
	}
	if streams.Out == nil {
		streams.Out = io.Discard
	}
	if streams.Err == nil {
		streams.Err = io.Discard
	}
	i, err := parse(args)
	if err != nil {
		fmt.Fprintln(streams.Err, err)
		return ExitUsage
	}
	if i.action == "help" {
		if _, err = fmt.Fprint(streams.Out, Help); err != nil {
			return ExitFailure
		}
		return ExitOK
	}
	if i.action == "version" {
		version := options.Version
		if version == "" {
			version = "1.0.0"
		}
		if _, err = fmt.Fprintln(streams.Out, version); err != nil {
			return ExitFailure
		}
		return ExitOK
	}
	if options.CleanupTimeout < 0 {
		fmt.Fprintln(streams.Err, "negative cleanup timeout")
		return ExitUsage
	}
	a := &application{options: options, invocation: i, streams: streams, hint: make(chan struct{}, 1), notices: make(chan string, 64)}
	useTUI := i.action == "chat" && streams.Interactive && !i.stdin && !i.json
	if useTUI {
		a.rawEvents = make(chan runtimeEnvelope, 256)
		a.rawOverflow = make(chan struct{}, 1)
	}
	defer func() {
		if closeErr := a.close(); closeErr != nil {
			fmt.Fprintln(streams.Err, a.safe("cleanup: "+closeErr.Error()))
			if code == ExitOK {
				code = ExitFailure
			}
		}
		a.drainNotices()
	}()
	fail := func(err error) int {
		c := contextCode(ctx)
		if c == ExitOK {
			c = ExitFailure
		}
		message := a.safe(err.Error())
		if i.action == "run" {
			output := RunOutput{SessionID: i.sessionID, State: agent.RunStateError, Error: message, ExitCode: c}
			if c == ExitCancelled || c == ExitTimeout {
				output.State = agent.RunStateAborted
			}
			_ = a.writeResult(output)
		} else {
			fmt.Fprintln(streams.Err, message)
		}
		return c
	}
	if err = a.openStore(ctx); err != nil {
		return fail(err)
	}
	if strings.HasPrefix(i.action, "sessions:") {
		if err = a.sessions(ctx); err != nil {
			return fail(err)
		}
		return ExitOK
	}
	if err = a.assemble(ctx); err != nil {
		return fail(err)
	}
	if _, err = fmt.Fprintf(streams.Err, "Model: %s\n", a.safe(a.selectedRef)); err != nil {
		return fail(err)
	}
	if i.action == "chat" {
		if useTUI {
			return a.terminalChat(ctx)
		}
		return a.chat(ctx)
	}
	if i.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, i.timeout)
		defer cancel()
	}
	if i.stdin {
		a.input = newInput(streams)
		i.task, err = a.input.read(ctx, true)
		if err != nil {
			return fail(err)
		}
		if strings.TrimSpace(i.task) == "" {
			fmt.Fprintln(streams.Err, "stdin prompt is empty")
			return ExitUsage
		}
	}
	output := a.run(ctx, i.task)
	// Close before final JSON: close failures must be represented in its exit_code.
	if err = a.close(); err != nil {
		if output.ExitCode == ExitOK {
			output.ExitCode = ExitFailure
		}
		output.Error = a.safe(strings.TrimSpace(output.Error + "; cleanup: " + err.Error()))
	}
	// Resources are idempotent except Store.Close; mark ownership consumed.
	a.store = nil
	a.host = nil
	a.ownsManager = false
	if err = a.writeResult(output); err != nil {
		fmt.Fprintln(streams.Err, err)
		return ExitFailure
	}
	return output.ExitCode
}

func contextCode(ctx context.Context) int {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ExitTimeout
	}
	if ctx.Err() != nil {
		return ExitCancelled
	}
	return ExitOK
}
func (a *application) writeResult(out RunOutput) error {
	out.Text = a.safe(out.Text)
	out.Error = a.safe(out.Error)
	if a.invocation.json {
		return json.NewEncoder(a.streams.Out).Encode(out)
	}
	if out.Error != "" {
		fmt.Fprintln(a.streams.Err, out.Error)
	}
	if out.Text != "" {
		_, err := fmt.Fprintln(a.streams.Out, out.Text)
		return err
	}
	return nil
}
func finalText(messages []llm.Message) string {
	for n := len(messages) - 1; n >= 0; n-- {
		m, ok := messages[n].(*llm.AssistantMessage)
		if !ok || m == nil {
			continue
		}
		var parts []string
		for _, block := range m.Content {
			if block.Type == llm.TypeToolCall {
				return ""
			}
			if block.Type == llm.TypeText {
				parts = append(parts, block.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
func (a *application) observe(sessionID string, event runtime.Event) {
	if a.rawEvents != nil {
		select {
		case a.rawEvents <- runtimeEnvelope{sessionID, event}:
		default:
			select {
			case a.rawOverflow <- struct{}{}:
			default:
			}
		}
	}
	select {
	case a.hint <- struct{}{}:
	default:
	}
	notify := func(s string) {
		select {
		case a.notices <- a.safe(sessionID + ": " + s):
		default:
		}
	}
	if event.Diagnostic != nil {
		notify(event.Diagnostic.Error())
	}
	if event.Notices != nil {
		for _, message := range event.Notices.SystemMessages {
			notify(message)
		}
		for _, report := range event.Notices.Reports {
			if report.StatusMessage != "" {
				notify(report.StatusMessage)
			}
			if report.Error != nil {
				notify(fmt.Sprintf("hook %s: %v", report.Event, report.Error))
			}
		}
	}
}
func (a *application) drainNotices() {
	for {
		select {
		case notice := <-a.notices:
			fmt.Fprintln(a.streams.Err, notice)
		default:
			return
		}
	}
}
func (a *application) cleanupContext() (context.Context, context.CancelFunc) {
	timeout := a.options.CleanupTimeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	return context.WithTimeout(context.Background(), timeout)
}
