package loopguard

import (
	"encoding/hex"
	"strings"
	"sync"
)

type operation struct {
	toolName      string
	argumentsHash [32]byte
}

type outcome struct {
	operation
	status     Status
	format     ResultFormat
	resultHash [32]byte
}

type observation struct {
	key         outcome
	sequence    uint64
	sourceID    string
	allowRepeat bool
}

// Detector 只绑定一个 Run，不持有其他会话、任务或全局注册表。
// 必须使用 New 创建，使用后不得复制。方法可并发调用，但匹配顺序是获得锁的顺序；
// 调用方应串行提交事件，避免并行工具完成通知的调度差异影响结果。
type Detector struct {
	mu       sync.Mutex
	runID    string
	config   Config
	sequence uint64
	recent   []observation
}

func New(runID string, config Config) (*Detector, error) {
	if err := validIdentifier("RunID", runID, false); err != nil {
		return nil, err
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &Detector{
		runID: strings.Clone(runID), config: config,
		recent: make([]observation, 0, config.WindowSize),
	}, nil
}

// Observe 不改变输入；无效事件（包括其他 Run）返回错误且不改变任何检测状态。
// 只在新事件延续匹配时返回建议；不执行取消，也不会自动发送警告给模型或用户。
func (d *Detector) Observe(event Event) (Decision, error) {
	if d == nil {
		return Decision{}, ErrUninitialized
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.runID == "" {
		return Decision{}, ErrUninitialized
	}
	if event.RunID != d.runID {
		return Decision{}, ErrRunMismatch
	}
	item, err := normalizeEvent(event)
	if err != nil {
		return Decision{}, err
	}
	d.sequence++
	item.sequence = d.sequence
	if event.Progress {
		d.clear()
		return Decision{Level: Normal, Reason: ProgressReset}, nil
	}
	d.append(item)
	if event.AllowRepeat {
		return Decision{Level: Normal, Reason: AllowedRepeat}, nil
	}

	// 轮询占用最近事件窗口，但不计入匹配，也不自行证明进展。
	var candidates [MaxWindowSize]observation
	n := 0
	for _, entry := range d.recent {
		if !entry.allowRepeat {
			candidates[n] = entry
			n++
		}
	}
	entries := candidates[:n]
	last := entries[n-1]

	// 同一操作连续失败，即使错误内容变化也会提示；依据显式状态，不凭工具名推断。
	if isFailure(last.key.status) {
		start := n - 1
		for start > 0 && entries[start-1].key.operation == last.key.operation && isFailure(entries[start-1].key.status) {
			start--
		}
		if n-start >= d.config.RepeatThreshold {
			return d.decision(RepeatedFailure, entries[start:], d.config.RepeatThreshold), nil
		}
	}

	// 成功调用必须连结果也相同才可能命中；同名调用或不同输出不构成本规则的重复。
	start := n - 1
	for start > 0 && entries[start-1].key == last.key {
		start--
	}
	if n-start >= d.config.RepeatThreshold {
		return d.decision(RepeatedResult, entries[start:], d.config.RepeatThreshold), nil
	}

	// A/B 是两个不同的完整（操作、状态、结果）键，按相隔两项相等检测后缀。
	if n >= 2 && entries[n-2].key != last.key {
		start = n - 2
		for start > 0 && entries[start-1].key == entries[start+1].key {
			start--
		}
		if n-start >= d.config.AlternatingThreshold {
			return d.decision(Alternating, entries[start:], d.config.AlternatingThreshold), nil
		}
	}
	return Decision{Level: Normal, Reason: NoPattern}, nil
}

// Reset 用于用户补充指令等显式边界，清空证据但保留 Run 绑定和单调序号。
// 新 Run 必须 New 一个 Detector，不能用 Reset 把同一实例改绑到其他 Run。
func (d *Detector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clear()
}

func (d *Detector) clear() {
	clear(d.recent)
	d.recent = d.recent[:0]
}

func (d *Detector) append(item observation) {
	if len(d.recent) == d.config.WindowSize {
		copy(d.recent, d.recent[1:])
		d.recent[len(d.recent)-1] = item
		return
	}
	d.recent = append(d.recent, item)
}

func (d *Detector) decision(reason Reason, entries []observation, threshold int) Decision {
	level := Warning
	if len(entries) > threshold {
		level = StopSuggested
	}
	evidence := &Evidence{
		RunID: d.runID, WindowSize: d.config.WindowSize,
		Count: len(entries), WarningAt: threshold, StopAt: threshold + 1,
		FirstSequence: entries[0].sequence, LastSequence: entries[len(entries)-1].sequence,
		Events: make([]EventEvidence, len(entries)),
	}
	for i, entry := range entries {
		evidence.Events[i] = EventEvidence{
			Sequence: entry.sequence, SourceID: entry.sourceID,
			ToolName:      entry.key.toolName,
			ArgumentsHash: hex.EncodeToString(entry.key.argumentsHash[:]),
			Status:        entry.key.status, ResultFormat: entry.key.format,
			ResultHash: hex.EncodeToString(entry.key.resultHash[:]),
		}
	}
	return Decision{Level: level, Reason: reason, Evidence: evidence}
}

func isFailure(status Status) bool {
	return status == Failure || status == Exception
}
