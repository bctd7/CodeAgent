# 子代理工具适配层

`New(controller)` 返回现有 `[]agent.Tool`，包含 `spawn_agent`、`send_input`、`wait`、`cancel_agent`。调用方注入绑定自身作用域的 `Controller`；本包只校验输入、调用接口，并把结构化快照转换成 JSON 文本工具结果。尚无真实子任务执行能力，也未接入 Runtime。

Spawn 的角色为 `explore` 或 `worker`；可选 `model_ref` 指向已有模型配置，省略时由未来 Runtime 继承当前模型。任务说明和不透明 ID 原样传递，不接受父会话 ID、凭据、权限升级或授权标志。运行中 Send 的安全消费及终态后的续跑均由实现负责。

Wait 支持一个或多个 ID，模式 `any` 或 `all`，`timeout_ms` 必填且为 1–300000 的整数。完成、失败和取消都算结束；返回全部 ID 的快照与 `timed_out`。实现必须合作响应 context，等待超时只结束等待，不取消任务。Cancel 只请求取消当前任务及收尾，不能关闭子 Runtime、删除 Session 或回滚文件；收尾未完成时仍可返回 `running`。

四个工具允许并行调用，Controller 必须保证并发安全、校验子任务归属，并返回稳定快照。独立子 Session、共享目录及并行修改协调、权限继承、禁止子代理继续 spawn 都属于后续 Runtime；本包不创建数据库、会话、后台运行器或默认模型。
