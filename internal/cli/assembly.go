package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goRuntime "runtime"
	"sort"
	"strings"
	"time"

	"CodeAgent/internal/config"
	"CodeAgent/internal/hooks"
	"CodeAgent/internal/instructions"
	"CodeAgent/internal/mcp"
	"CodeAgent/internal/memory/compact"
	"CodeAgent/internal/memory/longterm"
	"CodeAgent/internal/permission"
	"CodeAgent/internal/runtime"
	"CodeAgent/internal/session"
	sc "CodeAgent/internal/session/config"
	"CodeAgent/internal/session/store"
	"CodeAgent/internal/skills"
)

// hostSettings contains no credentials and is loaded only from an explicit trusted
// --settings path. Path values resolve relative to that file. CLI skill/hook paths
// resolve relative to the selected project. Sources are appended in declared order.
type hostSettings struct {
	DefaultModel    string               `json:"default_model"`
	Instructions    instructions.Options `json:"instructions"`
	Skills          []skills.Source      `json:"skills"`
	HookFiles       []string             `json:"hook_files"`
	Policy          permission.Policy    `json:"policy"`
	Compact         *compact.Settings    `json:"compact"`
	MemoryDirectory string               `json:"memory_directory"`
	MemoryScope     string               `json:"memory_scope"`
	MemoryLimits    longterm.Limits      `json:"memory_limits"`
	Subagents       *bool                `json:"subagents"`
}

type application struct {
	options      Options
	invocation   invocation
	streams      IO
	store        *store.Store
	service      *session.Service
	host         *runtime.Host
	current      *runtime.Runtime
	selectedRef  string
	defaultModel string
	modelRefs    []string
	manager      *mcp.Manager
	ownsManager  bool
	capabilities Capabilities
	secrets      []string
	hint         chan struct{}
	notices      chan string
	input        *inputReader
	configFile   string
	settingsFile string
	entries      []config.ModelEntry
	rawEvents    chan runtimeEnvelope
	rawOverflow  chan struct{}
	skillCatalog skills.Result
}

func (a *application) loadSettings() error {
	p := a.invocation.settingsPath
	if p == "" {
		return nil
	}
	a.settingsFile, _ = filepath.Abs(p)
	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("cannot open host settings")
	}
	defer f.Close()
	var s hostSettings
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&s); err != nil {
		return fmt.Errorf("invalid host settings")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return fmt.Errorf("host settings must contain one JSON object")
	}
	a.defaultModel = s.DefaultModel
	base, err := filepath.Abs(filepath.Dir(p))
	if err != nil {
		return err
	}
	path := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	if s.Instructions.ProjectRoot != "" {
		return fmt.Errorf("instructions ProjectRoot comes from --directory, not settings")
	}
	if s.Instructions.FileName != "" {
		a.options.Instructions.FileName = s.Instructions.FileName
	}
	if s.Instructions.GlobalDir != "" {
		a.options.Instructions.GlobalDir = path(s.Instructions.GlobalDir)
	}
	if s.Instructions.TargetDir != "" {
		a.options.Instructions.TargetDir = s.Instructions.TargetDir
	}
	if s.Instructions.MaxBytes != 0 {
		a.options.Instructions.MaxBytes = s.Instructions.MaxBytes
	}
	for _, source := range s.Skills {
		source.Path = path(source.Path)
		a.options.Skills = append(a.options.Skills, source)
	}
	for _, p := range s.HookFiles {
		if err = a.addHookFile(path(p)); err != nil {
			return err
		}
	}
	if s.Policy.WorkMode != "" {
		a.options.Policy.WorkMode = s.Policy.WorkMode
	}
	if s.Policy.ApprovalMode != "" {
		a.options.Policy.ApprovalMode = s.Policy.ApprovalMode
	}
	if s.Policy.WorkingDir != "" {
		return fmt.Errorf("policy WorkingDir comes from the selected session")
	}
	if s.Policy.PlanDir != "" {
		a.options.Policy.PlanDir = path(s.Policy.PlanDir)
	}
	a.options.Policy.DeniedTools = append(a.options.Policy.DeniedTools, s.Policy.DeniedTools...)
	if s.Compact != nil {
		a.options.Compact = *s.Compact
	}
	if s.MemoryDirectory != "" || s.MemoryScope != "" {
		a.options.MemoryDirectory = path(s.MemoryDirectory)
		a.options.MemoryScope = s.MemoryScope
		a.options.MemoryLimits = s.MemoryLimits
	}
	if s.Subagents != nil {
		a.options.Subagents = *s.Subagents
	}
	return nil
}

func (a *application) addHookFile(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("cannot open hooks file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return fmt.Errorf("hooks file is unreadable or too large")
	}
	absolute, err := filepath.Abs(p)
	if err != nil {
		return err
	}
	a.options.HookSources = append(a.options.HookSources, hooks.Source{ID: absolute, Kind: hooks.SettingsSource, JSON: data})
	return nil
}

func (a *application) openStore(ctx context.Context) error {
	dir := a.invocation.dataDir
	if dir == "" {
		dir = a.options.DataDir
	}
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(base, "CodeAgent")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	a.options.DataDir = absolute
	if err = os.MkdirAll(absolute, 0700); err != nil {
		return err
	}
	db, err := store.Open(ctx, filepath.Join(absolute, "sessions.sqlite"))
	if err != nil {
		return err
	}
	a.store = db
	if err = db.Init(ctx); err != nil {
		return err
	}
	a.service = session.NewService(db)
	return nil
}

func (a *application) isChild(ctx context.Context, id string) (bool, error) {
	var child bool
	err := a.store.WithReadTx(ctx, func(tx *store.Tx) error { var e error; child, e = tx.IsChildSession(ctx, id); return e })
	return child, err
}

func (a *application) assemble(ctx context.Context) error {
	if err := a.loadSettings(); err != nil {
		return err
	}
	i := a.invocation
	root := i.directory
	ref := i.modelRef
	var info sc.SessionInfo
	if i.sessionID != "" {
		child, err := a.isChild(ctx, i.sessionID)
		if err != nil {
			return err
		}
		if child {
			return fmt.Errorf("child sessions can be viewed, but cannot be opened as main sessions")
		}
		info, err = a.service.GetSession(ctx, i.sessionID)
		if err != nil {
			return err
		}
		if root != "" {
			given, err := canonicalDir(root)
			if err != nil {
				return err
			}
			saved, err := canonicalDir(info.WorkingDir)
			if err != nil {
				return err
			}
			if given != saved {
				return fmt.Errorf("directory differs from saved session")
			}
		}
		root = info.WorkingDir
		if ref == "" && info.ModelSelection != nil {
			ref = info.ModelSelection.Ref
		}
		if ref == "" {
			return fmt.Errorf("saved session has no model REF; use --model REF")
		}
	}
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	var err error
	root, err = canonicalDir(root)
	if err != nil {
		return err
	}
	entries := a.options.ModelEntries
	if entries == nil {
		p, pathErr := resolveConfig(i.configPath, a.options.ConfigPath)
		if pathErr != nil {
			return pathErr
		}
		a.configFile = p
		cfg, e := config.Load(p)
		if e != nil {
			return fmt.Errorf("model/MCP configuration could not be loaded or validated")
		}
		entries = cfg.ModelEntries()
		if a.options.Instructions.FileName == "" {
			a.options.Instructions.FileName = cfg.Instructions.FileName
		}
		if a.options.MCP == nil {
			specs, e := cfg.MCPServers()
			if e != nil {
				return fmt.Errorf("invalid MCP configuration")
			}
			if len(specs) > 0 {
				a.manager, e = mcp.NewManager(specs)
				if e != nil {
					return e
				}
				a.ownsManager = true
			}
		}
	}
	a.entries = entries
	if a.options.MCP != nil {
		a.manager = a.options.MCP
	}
	models := map[string]runtime.ModelConfig{}
	for _, entry := range entries {
		if _, ok := models[entry.Ref]; ok {
			return fmt.Errorf("duplicate model REF")
		}
		models[entry.Ref] = runtime.ModelConfig{Model: entry.Model, Options: entry.Options}
		a.modelRefs = append(a.modelRefs, entry.Ref)
		if entry.Options.APIKey != "" {
			a.secrets = append(a.secrets, entry.Options.APIKey)
		}
		for _, v := range entry.Options.Headers {
			if v != "" {
				a.secrets = append(a.secrets, v)
			}
		}
	}
	sort.Strings(a.modelRefs)
	if len(a.modelRefs) == 0 {
		return fmt.Errorf("model configuration contains no configured model REFs")
	}
	if ref == "" {
		ref = a.defaultModel
		if _, configured := models[ref]; !configured {
			ref = a.modelRefs[0]
		}
	}
	if _, ok := models[ref]; !ok {
		return fmt.Errorf("saved or selected model REF %q is not configured", ref)
	}
	for _, p := range i.skills {
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		a.options.Skills = append(a.options.Skills, skills.Source{Path: p, Name: "cli"})
	}
	a.skillCatalog = skills.Load(a.options.Skills)
	for _, p := range i.hooks {
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		if err = a.addHookFile(p); err != nil {
			return err
		}
	}
	var engine *hooks.Engine
	if len(a.options.HookSources) > 0 {
		engine, err = hooks.New(a.options.HookSources, a.options.HookOptions)
		if err != nil {
			return err
		}
	}
	if a.options.Policy.WorkMode == "" {
		a.options.Policy.WorkMode = permission.Build
	}
	if a.options.Policy.ApprovalMode == "" {
		a.options.Policy.ApprovalMode = permission.Default
	}
	if a.options.Policy.WorkMode != permission.Plan && a.options.Policy.WorkMode != permission.Build {
		return fmt.Errorf("invalid work mode")
	}
	switch a.options.Policy.ApprovalMode {
	case permission.Default, permission.AutoEdit, permission.AutoApprove:
	default:
		return fmt.Errorf("invalid approval mode")
	}
	if a.options.Compact == (compact.Settings{}) {
		a.options.Compact = compact.DefaultSettings()
	}
	a.capabilities = Capabilities{Skills: append([]skills.Source{}, a.options.Skills...), Instructions: a.options.Instructions, DefaultTools: a.options.Tools == nil, CustomTools: len(a.options.Tools), MCP: a.manager != nil, ScriptHooks: engine != nil, ModelHooks: a.options.ModelHooks != nil, Compact: a.options.Compact, Memory: a.options.MemoryDirectory != "", MemoryScope: a.options.MemoryScope, Subagents: a.options.Subagents, WebFetch: a.options.WebFetch != nil, WorkMode: a.options.Policy.WorkMode, ApprovalMode: a.options.Policy.ApprovalMode}
	a.capabilities.Instructions.ProjectRoot = root
	if a.capabilities.Memory {
		a.capabilities.MemoryShutdown = "cancel_and_join_on_close; run completion does not await consolidation"
	}
	host, err := runtime.NewHost(ctx, runtime.Options{Store: a.store, CloseStore: true, OwnershipDir: filepath.Join(a.options.DataDir, "ownership"), ProjectDir: root, Models: models, ModelRef: ref, Stream: a.options.Stream, SystemPrompt: a.options.SystemPrompt, Instructions: a.options.Instructions, Skills: a.options.Skills, Hooks: engine, ModelHooks: a.options.ModelHooks, Policy: a.options.Policy, Compact: a.options.Compact, MemoryDirectory: a.options.MemoryDirectory, MemoryScope: a.options.MemoryScope, MemoryLimits: a.options.MemoryLimits, MCP: a.manager, CloseMCP: false, MCPShared: a.manager != nil && !a.ownsManager, Tools: a.options.Tools, WebFetch: a.options.WebFetch, Subagents: a.options.Subagents, OnEvent: a.observe})
	if err != nil {
		return err
	}
	a.host = host
	if i.sessionID == "" {
		info, err = host.Create(ctx, "")
		if err != nil {
			return err
		}
	}
	a.current, err = host.Open(ctx, info.ID)
	if err != nil {
		return err
	}
	a.selectedRef = ref
	if i.modelRef != "" && info.ModelSelection != nil && i.modelRef != info.ModelSelection.Ref {
		if err = a.current.SetModel(ctx, i.modelRef); err != nil {
			return err
		}
		a.selectedRef = i.modelRef
	}
	return nil
}

func canonicalDir(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("session has no working directory")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working directory must be a directory")
	}
	real = filepath.Clean(real)
	if goRuntime.GOOS == "windows" {
		real = strings.ToLower(real)
	}
	return real, nil
}

func (a *application) close() error {
	if a.input != nil {
		a.input.stop()
	}
	return a.closeResources()
}
func (a *application) closeResources() error {
	timeout := a.options.CleanupTimeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if a.host != nil {
		if err := a.host.Close(ctx); err != nil {
			if a.ownsManager && a.manager != nil {
				host, manager := a.host, a.manager
				go func() {
					if host.Close(context.Background()) == nil {
						_ = manager.Close()
					}
				}()
			}
			return err
		}
		if a.ownsManager && a.manager != nil {
			return a.manager.Close()
		}
		return nil
	}
	// Never close borrowed resources or close the Store before Host finished using it.
	if a.ownsManager && a.manager != nil {
		if err := a.manager.Close(); err != nil {
			return err
		}
	}
	if a.store != nil {
		return a.store.Close()
	}
	return nil
}
func (a *application) safe(s string) string {
	for _, secret := range a.secrets {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	return s
}
