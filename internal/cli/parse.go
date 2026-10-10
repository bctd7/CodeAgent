package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

type invocation struct {
	action, task, sessionID, modelRef, directory, configPath, settingsPath, dataDir string
	memoryDisabled                                                                  bool
	stdin, json                                                                     bool
	timeout                                                                         time.Duration
	limit, offset                                                                   int
	archive                                                                         string
	skills, hooks                                                                   stringsFlag
}
type stringsFlag []string

func (s *stringsFlag) String() string { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("empty path")
	}
	*s = append(*s, v)
	return nil
}

func parse(args []string) (invocation, error) {
	i := invocation{limit: 50, archive: "active"}
	if len(args) == 0 {
		args = []string{"chat"}
	} else if strings.HasPrefix(args[0], "-") && args[0] != "--help" && args[0] != "-h" && args[0] != "--version" {
		args = append([]string{"chat"}, args...)
	}
	switch args[0] {
	case "help", "--help", "-h":
		i.action = "help"
	case "version", "--version":
		i.action = "version"
	case "run", "chat":
		i.action = args[0]
	case "sessions":
		if len(args) < 2 || (args[1] != "list" && args[1] != "show") {
			return i, fmt.Errorf("sessions requires list or show")
		}
		i.action = "sessions:" + args[1]
		args = args[1:]
	default:
		return i, fmt.Errorf("unknown command %q", args[0])
	}
	if i.action == "help" || i.action == "version" {
		if len(args) != 1 {
			return i, fmt.Errorf("unexpected arguments")
		}
		return i, nil
	}
	fs := flag.NewFlagSet(i.action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&i.configPath, "config", "", "model/MCP TOML file")
	fs.StringVar(&i.settingsPath, "settings", "", "host JSON settings (default: next to executable)")
	fs.StringVar(&i.dataDir, "data-dir", "", "session database and ownership directory")
	fs.StringVar(&i.sessionID, "session", "", "main session ID")
	fs.StringVar(&i.modelRef, "model", "", "configured provider/model REF")
	fs.StringVar(&i.directory, "directory", "", "project directory")
	fs.BoolVar(&i.memoryDisabled, "no-memory", false, "disable long-term memory for a new session")
	fs.BoolVar(&i.stdin, "stdin", false, "read prompt or chat lines from stdin")
	fs.BoolVar(&i.json, "json", false, "JSON output")
	format := fs.String("format", "", "text or json")
	fs.DurationVar(&i.timeout, "timeout", 0, "per-run timeout, e.g. 30s")
	fs.IntVar(&i.limit, "limit", 50, "main session limit")
	fs.IntVar(&i.offset, "offset", 0, "main session offset")
	fs.StringVar(&i.archive, "archive-filter", "active", "active, archived, or all")
	fs.Var(&i.skills, "skills", "skill source directory (repeatable)")
	fs.Var(&i.hooks, "hooks", "trusted hooks JSON file (repeatable)")
	// Accept flags before or after positional prompt/ID; -- preserves literal tokens.
	var flags, pos []string
	valueFlags := map[string]bool{"config": true, "settings": true, "data-dir": true, "session": true, "model": true, "directory": true, "format": true, "timeout": true, "limit": true, "offset": true, "archive-filter": true, "skills": true, "hooks": true}
	remaining := args[1:]
	for n := 0; n < len(remaining); n++ {
		a := remaining[n]
		if a == "--" {
			pos = append(pos, remaining[n+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		name, val, has := strings.Cut(strings.TrimLeft(a, "-"), "=")
		_ = val
		if valueFlags[name] && !has {
			n++
			if n == len(remaining) {
				return i, fmt.Errorf("--%s needs a value", name)
			}
			flags = append(flags, remaining[n])
		}
	}
	if err := fs.Parse(flags); err != nil {
		return i, err
	}
	if *format != "" {
		if *format != "text" && *format != "json" {
			return i, fmt.Errorf("format must be text or json")
		}
		if i.json && *format == "text" {
			return i, fmt.Errorf("conflicting output formats")
		}
		i.json = *format == "json"
	}
	if i.timeout < 0 || i.limit < 1 || i.limit > 200 || i.offset < 0 {
		return i, fmt.Errorf("invalid timeout or pagination")
	}
	if i.archive != "active" && i.archive != "archived" && i.archive != "all" {
		return i, fmt.Errorf("invalid archive filter")
	}
	switch i.action {
	case "run":
		if i.stdin {
			if len(pos) != 0 {
				return i, fmt.Errorf("--stdin cannot be combined with a positional prompt")
			}
		} else {
			if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
				return i, fmt.Errorf("run requires one quoted prompt or --stdin")
			}
			i.task = pos[0]
		}
	case "chat":
		if len(pos) != 0 {
			return i, fmt.Errorf("chat takes no positional prompt")
		}
	case "sessions:show":
		if len(pos) != 1 || i.sessionID != "" {
			return i, fmt.Errorf("sessions show requires one ID")
		}
		i.sessionID = pos[0]
	case "sessions:list":
		if len(pos) != 0 || i.sessionID != "" {
			return i, fmt.Errorf("sessions list takes no ID")
		}
	}
	if strings.HasPrefix(i.action, "sessions:") && (i.stdin || i.timeout != 0 || i.modelRef != "" || i.directory != "" || len(i.skills) > 0 || len(i.hooks) > 0) {
		return i, fmt.Errorf("execution flags are not valid for sessions queries")
	}
	if i.memoryDisabled && (i.sessionID != "" || strings.HasPrefix(i.action, "sessions:")) {
		return i, fmt.Errorf("--no-memory only applies when creating a new session")
	}
	return i, nil
}

const Help = `codeagent [execution flags] (interactive TUI)
codeagent run "prompt" [--model provider/alias] [--session ID] [--directory DIR]
  [--stdin] [--timeout 30s] [--format text|json]
codeagent chat [--stdin] [same execution flags]
codeagent sessions list [--limit 50] [--offset 0] [--archive-filter active|archived|all]
codeagent sessions show ID
codeagent help | version
Common: --config FILE --settings FILE --data-dir DIR --json
Execution: --skills DIR (repeatable) --hooks FILE (repeatable) --no-memory (new sessions only)
Chat: /help lists session commands; no codeagent prefix inside chat.
Approval: once | session | reject; /status /help /exit are also available.
New model: --model > configured settings default_model > first sorted REF. Restore: --model > saved REF.
Default config: config.toml next to codeagent.exe; working-directory TOML is ignored.
  Default runtime settings: settings.json next to executable; missing file uses built-in defaults.
Explicit --config/host override selects one file and never merges configurations.
Interactive chat uses TUI; --json and --stdin keep headless output.
JSON chat writes one JSON value per line. Noninteractive approval exits with code 3.
Exit: 0 success, 1 failure, 2 usage, 3 approval required, 4 deferred, 124 timeout, 130 cancelled.
Background memory is cancelled and safely joined on close; completion is not promised.
`
