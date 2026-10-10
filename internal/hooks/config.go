// 本文件负责显式来源配置的 JSON 解析、验证与匹配规则编译。
package hooks

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SourceKind 区分设置来源和插件来源。
type SourceKind string

const (
	SettingsSource SourceKind = "settings"
	PluginSource   SourceKind = "plugin"
)

// Source 保存调用方明确授权的 settings/hooks.json 文档。
// 按切片顺序合并文档；本包不自行搜索磁盘上的配置。
type Source struct {
	// ID 必须非空且唯一，用于诊断定位；Kind 空值按设置来源处理。
	ID   string
	Kind SourceKind
	// JSON 是来源文档；插件目录用于构建命令中的路径占位符。
	JSON                   []byte
	PluginRoot, PluginData string
}

// Options 声明进程限制和钩子数量上限。
type Options struct {
	Runner RunnerOptions
	// MaxHandlers 限制全部来源的钩子总数，零值使用默认上限。
	MaxHandlers int
}

// handler 对应单个命令钩子的协议配置。
type handler struct {
	Type          string   `json:"type"`
	Command       string   `json:"command"`
	Args          []string `json:"args"`
	Shell         string   `json:"shell"`
	Timeout       *float64 `json:"timeout"`
	StatusMessage string   `json:"statusMessage"`
	Async         bool     `json:"async"`
	AsyncRewake   bool     `json:"asyncRewake"`
	If            string   `json:"if"`
	Once          bool     `json:"once"`
}

type group struct {
	Matcher string            `json:"matcher"`
	Hooks   []json.RawMessage `json:"hooks"`
}

// compiledHook 保存已验证配置和预编译匹配器，供事件分发复用。
type compiledHook struct {
	source   Source
	id       string
	event    Event
	matcher  *regexp.Regexp
	handler  handler
	timeout  time.Duration
	dedupKey string
}

// compileSource 验证来源文档并按事件编译钩子。
func compileSource(source Source, inputLimit, remaining int) ([]compiledHook, error) {
	bad := func(message string, kind error) error { return configError(source.ID, message, kind) }
	if source.Kind != SettingsSource && source.Kind != PluginSource {
		return nil, bad("source kind", ErrUnsupported)
	}
	if len(source.JSON) > inputLimit {
		return nil, bad("configuration too large", ErrConfiguration)
	}
	for _, path := range []string{source.PluginRoot, source.PluginData} {
		if path != "" && !filepath.IsAbs(path) {
			return nil, bad("plugin paths must be absolute", ErrConfiguration)
		}
	}
	if source.Kind == PluginSource && source.PluginRoot == "" {
		return nil, bad("plugin source requires PluginRoot", ErrConfiguration)
	}
	var root map[string]json.RawMessage
	if err := decodeJSON(source.JSON, &root, false); err != nil || root == nil {
		return nil, bad("expected settings/hooks.json object", ErrConfiguration)
	}
	if value, ok := root["disableAllHooks"]; ok {
		var disabled bool
		if err := json.Unmarshal(value, &disabled); err != nil || isJSONNull(value) {
			return nil, bad("invalid disableAllHooks", ErrConfiguration)
		}
		if disabled {
			return nil, bad("resolve disableAllHooks in caller before constructing Engine", ErrUnsupported)
		}
	}
	raw, exists := root["hooks"]
	if !exists {
		return nil, nil
	}
	var events map[Event][]json.RawMessage
	if err := json.Unmarshal(raw, &events); err != nil || events == nil {
		return nil, bad("hooks must be an event object", ErrConfiguration)
	}
	keys := make([]string, 0, len(events))
	for event := range events {
		keys = append(keys, string(event))
	}
	sort.Strings(keys)
	source.JSON = nil
	var result []compiledHook
	for _, key := range keys {
		event := Event(key)
		if !supportedEvent(event) {
			return nil, bad("event "+key, ErrUnsupported)
		}
		if events[event] == nil {
			return nil, bad("event value must be an array", ErrConfiguration)
		}
		compiled, err := compileGroups(source, event, events[event], remaining-len(result))
		if err != nil {
			return nil, err
		}
		result = append(result, compiled...)
	}
	return result, nil
}

func compileGroups(source Source, event Event, groups []json.RawMessage, remaining int) ([]compiledHook, error) {
	var result []compiledHook
	for gi, data := range groups {
		var g group
		// 来源入口已经检查 JSON 语法和重复键，此处直接解码嵌套数据。
		if err := unmarshalJSON(data, &g, true); err != nil || g.Hooks == nil {
			return nil, configError(source.ID, "invalid matcher group", ErrConfiguration)
		}
		matcher, err := compileMatcher(event, g.Matcher)
		if err != nil {
			return nil, configError(source.ID, "matcher: "+err.Error(), ErrConfiguration)
		}
		for hi, data := range g.Hooks {
			if len(result) == remaining {
				return nil, configError(source.ID, "too many handlers", ErrConfiguration)
			}
			h, timeout, err := compileHandler(event, data)
			if err != nil {
				return nil, fmt.Errorf("%s/%s/%d/%d: %w", source.ID, event, gi, hi, err)
			}
			canonical, _ := json.Marshal(h)
			result = append(result, compiledHook{source: source, id: fmt.Sprintf("%s/%s/%d/%d", source.ID, event, gi, hi), event: event, matcher: matcher, handler: h, timeout: timeout, dedupKey: string(canonical)})
		}
	}
	return result, nil
}

// compileHandler 只接受当前支持的同步命令配置，并检查超时范围。
func compileHandler(event Event, data []byte) (handler, time.Duration, error) {
	var h handler
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return h, 0, fmt.Errorf("%w: handler must be an object", ErrConfiguration)
	}
	for key, value := range fields {
		if isJSONNull(value) {
			return h, 0, fmt.Errorf("%w: null handler field %s", ErrConfiguration, key)
		}
	}
	if err := unmarshalJSON(data, &h, true); err != nil {
		return h, 0, fmt.Errorf("%w: handler: %v", ErrConfiguration, err)
	}
	if h.Type != "command" || h.Async || h.AsyncRewake || h.If != "" {
		return h, 0, fmt.Errorf("%w: only synchronous command hooks without if are supported", ErrUnsupported)
	}
	if strings.TrimSpace(h.Command) == "" || strings.IndexByte(h.Command, 0) >= 0 {
		return h, 0, fmt.Errorf("%w: command must be nonempty without NUL", ErrConfiguration)
	}
	if h.Args == nil && !oneOf(h.Shell, "", "bash", "powershell") {
		return h, 0, fmt.Errorf("%w: shell %q", ErrUnsupported, h.Shell)
	}
	for _, a := range h.Args {
		if strings.IndexByte(a, 0) >= 0 {
			return h, 0, fmt.Errorf("%w: NUL in argument", ErrConfiguration)
		}
	}
	timeout := defaultTimeout(event)
	if h.Timeout != nil {
		v := *h.Timeout
		if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) || v > 86400 {
			return h, 0, fmt.Errorf("%w: timeout must be in (0, 86400] seconds", ErrConfiguration)
		}
		timeout = time.Duration(v * float64(time.Second))
		if timeout <= 0 {
			return h, 0, fmt.Errorf("%w: timeout below timer precision", ErrConfiguration)
		}
	}
	return h, timeout, nil
}

// compileMatcher 为可匹配事件编译正则；不支持匹配的事件以及空值、通配符不限制匹配。
func compileMatcher(event Event, pattern string) (*regexp.Regexp, error) {
	if !matchable(event) || pattern == "" || pattern == "*" {
		return nil, nil
	}
	exact := true
	for _, c := range pattern {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("_- ,|", c)) {
			exact = false
			break
		}
	}
	if exact {
		names := strings.FieldsFunc(pattern, func(c rune) bool { return c == ',' || c == '|' })
		for i, name := range names {
			names[i] = regexp.QuoteMeta(strings.TrimSpace(name))
		}
		pattern = "^(?:" + strings.Join(names, "|") + ")$"
	}
	return regexp.Compile(pattern)
}

func configError(source, message string, kind error) error {
	return fmt.Errorf("%w: %s: %s", kind, source, message)
}

func supportedEvent(e Event) bool {
	switch e {
	case SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, PostToolUseFailure, PostToolBatch, Stop, SessionEnd:
		return true
	}
	return false
}

func matchable(e Event) bool {
	return e == SessionStart || e == SessionEnd || e == PreToolUse || e == PostToolUse || e == PostToolUseFailure
}

func defaultTimeout(e Event) time.Duration {
	switch e {
	case UserPromptSubmit:
		return 30 * time.Second
	case SessionEnd:
		return 1500 * time.Millisecond
	}
	return 600 * time.Second
}
