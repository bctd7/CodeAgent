# 局部重复失败与循环检测

`loopguard` 是独立的 Go 子包，只接收可信调用方提供的已完成工具事件摘要，返回 `Normal`、`Warning` 或 `StopSuggested`。没有自动注册、agent loop 调用、模型调用、工具执行、取消、任务修改、持久化或全局任务管理。当前没有上层接入，因此不会自动节省 token 或停止任务；后续接入须另行审核。

## Hook ownership

Current package path: `internal/hooks/loopguard` (Go import: `CodeAgent/internal/hooks/loopguard`). This independent subpackage is the detection implementation for a concrete Hook. It preserves the existing detection API and behavior. Automatic integration and lifecycle handling require subsequent review; it is not registered or running as a Hook. Historical validation records below retain their original paths and results.


## API 与作用域

```go
func DefaultConfig() Config
func New(runID string, config Config) (*Detector, error)
func (d *Detector) Observe(event Event) (Decision, error)
func (d *Detector) Reset()
```

每个 Run 创建一个 `Detector`。`Event.RunID` 必须精确匹配构造时的值，其他 Run 返回 `ErrRunMismatch`，不清空、不追加、也不改绑状态。`Reset()` 只清空本 Run 的检测窗口，保留 Run 绑定和单调序号。调用方必须使用在相关会话中唯一的 RunID（必要时包含会话命名空间），不能把同一个实例或同一个 RunID 用于多个会话。

方法有互斥保护，实例使用后不能复制。调用方应按确定的顺序串行提交，每次完成恰好一次；本包不重建并行调用的因果顺序、不用 `SourceID` 去重，也不识别事件重放。零值和 nil Detector 的 `Observe` 返回 `ErrUninitialized`。

```go
d, err := loopguard.New("session-7/run-12", loopguard.DefaultConfig())
if err != nil {
    return err
}
decision, err := d.Observe(loopguard.Event{
    RunID:     "session-7/run-12",
    SourceID:  "tool-call-8",
    ToolName:  "read_file",
    Arguments: json.RawMessage(`{"path":"missing.go"}`),
    Result: loopguard.Result{
        Status: loopguard.Failure,
        Format: loopguard.TextResult,
        Data:   []byte("file not found"),
    },
})
// 检查 err 后，由未来的调用方策略解释 decision；本包不执行任何控制动作。
_ = decision
```

## 精确阈值

默认 `WindowSize=20`、`RepeatThreshold=3`、`AlternatingThreshold=4`。窗口最多保存最近 20 个已接受的完成事件，允许轮询的事件也占窗口位置；进展事件会清空窗口。匹配时排除允许轮询的事件，再检查剩余事件的连续后缀。只检查以本次新事件结尾的模式。

规则按下列顺序判定，第一条满足阈值的规则返回建议：

| 原因 | 匹配条件 | 默认 Warning | 默认 StopSuggested |
| --- | --- | --- | --- |
| `repeated_failure` | 相同工具名、相同规范参数，连续为 `Failure` 或 `Exception`；错误内容可以不同，两种失败状态可以混合 | 第 3 次 | 第 4 次及以后 |
| `repeated_result` | 相同工具名、规范参数、状态、结果格式及完整结果指纹 | 第 3 次 | 第 4 次及以后 |
| `alternating` | 两个不同的完整事件键 A/B 交替，每个位置的操作、状态和结果均稳定；A/B 可以是同一操作的不同结果 | `ABAB`（4 个事件） | `ABABA`（5 个事件）及后续延续 |

这里“完整事件键”不含 RunID、SourceID 或提交序号。RunID 由实例边界验证，SourceID 与序号只用于证据定位。

停止建议阈值固定为对应警告阈值加 1。每段模式首次到达阈值为 `Warning`，再增加一个同类事件为 `StopSuggested`，不要求再完成一个 AB 周期；继续延续时保持停止建议。模式被不同事件打断后重新计数。窗口滚动时计数最多为窗口大小，不代表历史总次数；大量轮询也会让旧证据被淘汰。

失败判断仅依据可信调用方提供的状态，不从工具名或结果中的“error”等文字推断。成功事件只有完整结果也重复，才命中 `repeated_result`；相同工具、相同参数但持续产生不同输出不会按重复调用停止。相同成功结果仍可能是合理的读取、幂等操作，调用方应明确标记允许重复的事件，不能把启发式建议当成绝对无进展证明。

配置为严格校验：`WindowSize` 在 5..64，`RepeatThreshold` 至少为 2，`AlternatingThreshold` 是至少为 4 的偶数，两个阈值都必须小于窗口大小，以容纳首次停止建议的证据。`Config{}` 无效，应从 `DefaultConfig()` 修改。

## 可信输入与重置

- `AllowRepeat=true`：用于允许的轮询等行为。本次返回 `Normal/allowed_repeat`，不触发或推进检测；不自行证明进展。夹在失败之间的轮询不打断仍在窗口中的失败序列。
- `Progress=true`：可信调用方已确认真实进展，清空所有当前序列，返回 `Normal/progress_reset`；即便同时设置 `AllowRepeat`，也以进展为准。
- 用户补充指令：调用方显式调用 `Reset()`，不把用户文字塞入比较键。
- 跳过、取消而未执行、尚未完成的工具，以及模型 thinking：不提交为此 API 的完成事件。工具抛出的真实异常可以提交 `Exception`，显式业务失败提交 `Failure`。

状态、两个标志和规范内容必须由可信执行层决定，不能从工具返回文字中解析“请重置”“这是 polling”等命令。单纯收到工具进度回调，不等于已取得语义进展。调用方应提供经过参数覆盖后真正执行的参数，并将它与对应的完成结果配对。同一 Run 内的 ToolName 必须稳定且唯一地标识工具；来自不同服务器的同名工具应由调用方使用稳定限定名区分。

## 归一化与内存

- `Arguments` 只接受 JSON 对象；nil/零长度表示 `{}`。参数与 `JSONResult` 共用 `canonicalJSON`：标准库 `Decoder.UseNumber` 保留数字原文，`json.Marshal` 稳定排序对象键，数组顺序保留。递归拒绝重复对象键、多值尾随、无效 JSON、无效 UTF-8 和过深嵌套。
- 大整数不会经过 float64，例如 `9007199254740992` 与 `9007199254740993` 不等价。调用方先转成 float64 后丢失的精度无法恢复。为避免猜测工具的数值语义，`1`、`1.0`、`1e0` 保守地视为不同写法。
- `TextResult` 对完整字节计算 SHA-256，不截断、不折叠空白，不根据文字内容猜测 JSON。`JSONResult` 先进行上述规范化，再哈希。`FingerprintResult` 接受调用方提供的稳定指纹，再哈希存储；建议带算法和版本前缀。同一操作应始终使用同一种格式和指纹算法，三种格式互不等价。
- 没有时间戳、耗时、调用 ID 或 thinking 字段进入比较键。参数/结果若含波动元数据，由调用方在提交前剔除；本包不随意删除名为 `timestamp` 的业务字段。
- 只保留参数/结果哈希、短标识和序号，不保留原始参数、完整结果或无界文本摘要。文本可很大，哈希读取完整输入但不复制或长期保存它。结构化 JSON 每份最多 64 KiB、嵌套最多 64 层，大 JSON 结果应由调用方先生成稳定指纹。RunID、工具名、SourceID、外部结果指纹最多各 256 字节；SourceID 可省略，其他标识/指纹不能空白。
- 最多保留 `WindowSize` 条小记录，内部状态为 O(WindowSize)，每次扫描也是 O(WindowSize)；JSON 临时解析受字节数和深度限制。哈希比较存在理论碰撞可能。调用方若永久保存每次返回的 Evidence，其存储预算由调用方负责。

输入在调用期间不得被调用方并发修改。本包不修改输入，也不保留其可变引用；标识字符串会复制，返回的证据切片也是独立副本。非法配置或事件返回错误；非法事件不推进序号，不改变已有窗口，即使其带有进展/轮询标志也不会清空状态。

## 证据与能力边界

`Decision.Reason` 是稳定原因码。非 Normal 结果携带 `Evidence`：RunID、实际配置窗口大小、窗口内匹配数、警告/停止阈值、首尾序号，以及每项的 SourceID、工具名、参数哈希、显式状态、结果格式和结果哈希。序号从 1 开始，允许的轮询会造成证据序号间隙。可以据此定位上层真实调用并解释为什么命中；不在建议中暴露完整参数或工具文本。

此检测只覆盖连续重复和周期为 2 的交替模式。没有仓库地图、文件过期检测、模型独白检测、上下文窗口循环检测、任意周期搜索或进展语义推理。漏报与误报都可能发生，应由上层结合用户意图决定是否调整策略。

窗口、重复和交替模式的思路参考已读取的 [OpenHands stuck_detector.py](https://github.com/OpenHands/software-agent-sdk/blob/main/openhands-sdk/openhands/sdk/conversation/stuck_detector.py)。这里是独立 Go 实现，不复用其状态管理，不纳入 thought equality，也不实现源码中仍为 TODO 的 context-window-loop。只使用 Go 标准库，不复用父 agent/Runtime/Session 类型，避免为局部比较引入上层依赖；规范化与证据构造分别在本子包内集中复用。

## 本次验证记录

在 Go 1.26.3 / Windows amd64 下，11 组仅使用假事件的临时测试通过，语句覆盖率 98.7%。覆盖 JSON 键序与大整数、AAA 失败警告后建议停止、ABAB/ABABA、不同成功输出、结果格式、轮询排除与窗口淘汰、进展/用户 Reset、跨 Run 隔离、1 万事件窗口上限、2 MiB 文本完整哈希、输入不变、证据副本隔离、非法配置/事件原子拒绝、自定义阈值与并发调用。

临时测试执行命令为 `go test -mod=readonly ./internal/agent/loopguard -count=1 -v -cover`。按本次任务约束，验证后已删除本任务临时测试源码；这里的覆盖率记录对应删除前的运行。

删除临时测试后，以下检查均通过：

```text
go build -mod=readonly ./...
go vet -mod=readonly ./...
go test -mod=readonly ./... -run '^$' -count=1
```

最后一项只验证全仓包和测试源码可编译，不执行既有测试用例。验证使用 `GOPROXY=off`、`GOSUMDB=off` 和本任务专属临时构建/缓存目录，没有真实模型、工具调用或依赖修改；专属临时目录在完成验证后删除。
