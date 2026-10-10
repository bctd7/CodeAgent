// Package permission 提供独立权限判断与显式会话授权，不接入 Runtime、Agent、工具或 UI。
//
// 信任边界：Policy、Request 的来源身份、能力、完整参数和所有文件目标必须由可信
// 调用方从注册表及实际调用中构造，不能相信模型传来的 flags 或 MCP readOnlyHint。
// ReadOnly 只适用于可信内置的已知只读实现；FileWrite 只适用于普通文件写入且没有
// 隐藏外部效果，目录操作或混合效果应标为 Unknown。Orchestration 仅表示可信宿主内部任务
// 生命周期管理，不直接执行文件修改、shell 或外置工具；任务的实际工具仍独立检查权限。
// 它不承诺零副作用，也不是通用免审标记。外置工具始终标为 External。
//
// Evaluate 是纯规则判断：Plan 只允许内置只读和指定计划目录内的 .md 写入；
// Build 下内置只读和内部任务编排直接允许，Default 其余能力询问，AutoEdit 额外允许工作目录内
// 普通写入，AutoApprove 将内置 Ask 转为 Allow。外置工具在 Build 下始终 Ask，
// Plan 下 Deny。用户明确 DeniedTools 优先，任何授权均不得覆盖 Deny。
//
// 调用流程：为每个会话 NewSession；Session.Evaluate 得到 Ask 后将完整请求展示给
// 用户；把 Confirmation 与真实选择传入 Grant；执行前用当前 Policy 和同一请求
// 再次 Session.Evaluate。Once 绑定完整请求及工作目录且只消费一次，SessionTool 绑定该容器、
// 来源身份和工具而允许改变参数；Reject 只拒绝当前请求，不产生永久 deny。
// 请求结束或取消时调用方应撤回该请求的待确认（可用 Revoke 撤回同一工具），
// 会话关闭必须 Clear 并丢弃容器。Result 只用于关联的当前执行，不是可复用凭证。
//
// 路径检查使用本机 filepath 语义，不改变进程工作目录。调用方必须先解析真实
// 目录和文件目标（包括符号链接、Windows junction，以及待创建文件的已有父目录），
// 不能把未解析链接的词法路径当作真实目标。本包不检查磁盘、不提供 OS sandbox、
// 不做 shell 静态安全分析，也不保证消除判断到工具执行全过程的 TOCTOU；执行层
// 仍须对实际打开/写入的目标实施其边界约束。一次性选择需要内存确认凭据，不可
// 从序列化数据重建。调用方负责会话与请求生命周期，本包不复制任务状态机。
package permission
