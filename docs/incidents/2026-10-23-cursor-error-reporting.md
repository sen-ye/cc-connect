# Cursor CLI 错误识别与飞书提醒设计

状态：实现完成，全量测试、CUJ、race、原生 CLI 故障注入及两轮恢复验证通过。

## 证据与适用范围

- cc-connect 线上版本：`1bae52a8`。
- Cursor CLI：`2026.10.19-6bbf2c4`。
- 用户提供日志：`/tmp/cursor-agent-logs-1000/session-2026-10-23T14-49-08-943Z-2953754-1.log`。
- 日志中的 `surface=interactive` 表明这是原生交互 CLI 的记录；cc-connect 实际使用 `--print --output-format stream-json`，不能把 TUI 显示字段当成 stdout 的现有协议。
- 该 CLI 的 `7000.index.js` 中 `./src/headless.ts`：成功时输出 `result/subtype=success/is_error=false`；失败分支记录 turn outcome，输出 `String(error)` 并以 1 退出。详细 displayInfo 未保证完整进入 stdout/stderr。
- 本机 `index.js` 确认默认按 PID 创建 debug log，除非设置 `CURSOR_AGENT_DISABLE_DEBUG_LOG`。路径来自 `os.tmpdir()`，文件名包含 PID；不能使用全局 `latest.log` 关联并发会话。

## 实际发生的事情（北京时间）

| 时间 | 原始记录 | 判定 |
|---|---|---|
| 22:49:49 | 第 111、118 行：outcome=cancelled、abortOrigin=owner | 主动取消，属于正常取消流程 |
| 23:00:24–23:00:31 | 第 484、490、491、495、499 行：首次失败后重试 1 次，最终 PROVIDER_ERROR，providerStatusCode=400，isRetryable=false | 模型提供方返回 400；CLI 已停止自动重试 |
| 23:04:07、23:05:07、23:06:07 | 第 537、549、558 行：stall_detector、thresholdMs=30000；第 563 行 noProgressResumeFailures=2；第 564 行 willRetry=false、THROW RetriableError | 初次尝试及两次恢复遭遇卡顿；没有产生新的保存点，触发防止无进展重放的保护 |
| 23:13:17–23:13:22 | 第 626、635、636、640、644 行：重试 1 次后 PROVIDER_ERROR，providerStatusCode=400 | 再次发生同类提供方错误 |

22:50 切换到 Gemini 4 Argon 后，相关 request.create 均记录该模型。日志仅记录提供方状态 400 和通用错误说明，没有具体上游拒绝原因，无法据此确定是代理、请求格式或其他根因。

两个易误判点：

1. 外层 Connect/gRPC `ResourceExhausted` 不足以认定 HTTP 429/额度耗尽；这里明确的提供方状态是 400。
2. `Canceled` 不等于用户取消；`abortOrigin=stall_detector` 表示 CLI 自身检测到卡顿。`willRetry=false` 表示自动重试已经停止，`isRetryable=true` 表示仍可以由用户发起一次新的恢复尝试，两者并不矛盾。

`otel.error ... end() on a span once` 和 conversationClassification 错误是辅助记录，不能直接当作当前 agent 回合失败的主要原因。

## cc-connect 当前缺口

`agent/cursor/session.go`：

- `handleEvent` 没有顶层 error 分支。
- `handleResult` 不判断 is_error/subtype，直接发出 Done=true 的 EventResult。
- `readLoop` 只在进程异常退出且 stderr 非空时发 EventError；异常退出但 stderr 为空时没有终态事件，Alive 仍为 true。
- stderr 原样上报；缺少 Cursor 错误分类、恢复建议和请求 ID。

`core/engine.go` 的专门错误映射目前仅覆盖 Session not found，没有上述 Cursor 情形。

在独立本机副本注入三类输入，确认当前适配器分别表现为：丢弃顶层 error、将 is_error=true 作为 result/done、exit 42 且空 stderr 无终态。诊断日志位于 `.agent-tmp/cursor-error-audit-20261023/audit.log`。这是兼容性缺口验证；并不宣称用户给出的 TUI 日志实际输出过前两种 JSON 消息。

## 推荐实现

### 1. 先保证每轮只有一个明确终态

在 Cursor adapter 内统一处理 stdout、stderr 和进程退出：成功结果、结构化错误、非零退出、无结果 EOF、用户取消。错误结果不得显示正常完成；每轮只发一个最终错误，CLI stderr 与日志中的重复记录不能形成多条通知。异常也保存已有 session ID，使下一次显式继续使用 --resume。

terminal 事件应与当前 Send/子进程绑定，防止旧进程退出时的错误或 stdin 清理影响下一轮。顶层错误和工具执行失败分开，工具失败不必然结束 agent 回合。

### 2. 规范化错误，再由 core 做国际化展示

Cursor adapter 输出通用错误信息：category、provider_status、retryable（可未知）、auto_retrying、retry_count、request_id、原始原因和会话 ID。core 根据通用类别渲染，遵守 core 不硬编码 agent/platform 的约束，并补全 EN/ZH/ZH-TW/JA/ES 文案。

识别优先级：结构化错误/displayInfo → 当前子进程日志中的终态与诊断字段 → 清理过的 stderr 文本 → 退出码兜底。保留原始细节供排查，公开显示内容做脱敏和长度限制。

对于本机这版 CLI，日志补充很重要，因为 headless 失败分支仅输出 String(error)。日志必须匹配启动 PID/时间，并进一步关联 conversationId、originalRequestId/requestId；从本轮起点读取，不能扫描其他会话，也不能使用全局 latest.log。

### 3. 分类与显示

| 识别条件 | 提醒 |
|---|---|
| PROVIDER_ERROR / ERROR_PROVIDER_ERROR，附 providerStatusCode | 模型服务请求失败；显示状态码和 CLI 重试次数；按 retryable 信息决定恢复建议 |
| transportFailureKind=stall 或 abortOrigin=stall_detector，且 willRetry=true | 在现有进度卡更新“连接无进展，Cursor 正在恢复（第 N 次）”，不提前显示最终失败 |
| noProgressResumeFailures 达到停止条件，willRetry=false | 显示“Cursor 连续恢复仍未保存新进度，已停止本轮；可以从最后保存状态继续” |
| 明确 HTTP 429 / RATE_LIMITED | 限流；有 Retry-After 时显示建议等待时间 |
| 明确鉴权失败 | 提示检查 Cursor 登录/对应凭据；不当作普通网络重试 |
| 用户触发取消 / owner abort | 已停止，不弹 provider 故障提醒 |
| 非零退出/缺失终态但无可识别原因 | 明确提示异常退出并附退出码/本轮诊断位置，避免一直显示执行中 |

只匹配控制通道里的错误与终态，不匹配用户提示词、assistant 正文或工具输出中的相同英文句子。以一个回合及 originalRequestId 聚合更新；单次 retry 更新状态，停止后只发一次终态。

### 4. 建议的飞书文案

> ⚠️ Cursor 本轮已停止：连续恢复未取得新进展。
> CLI 已自动恢复 2 次，但没有保存到新的进度。可以在当前会话发送“继续”，从最后保存状态尝试恢复。
> 展示当前模型、Session ID，详情中保留 request ID。

> ❌ Cursor 模型服务请求失败（Provider Error，HTTP 400）。
> CLI 已重试 1 次并停止。可稍后显式重试或切换模型；若持续返回 400，应检查具体请求错误。
> 详情中保留请求 ID 和原始错误。

不在 cc-connect 外层无限自动重发整条任务，避免绕过 Cursor 的无进展保护。第一阶段完成状态提醒及“同会话继续”的明确指引；若加按钮，按钮需绑定发生错误的会话/回合，避免操作到后来切换的会话。

## 验收

- 重放本日志的两种失败、主动取消、辅助 telemetry 错误；只产生正确的回合状态提醒。
- 同一请求的 retry、THROW、outcome、stderr 去重；真实重试计数和终止状态正确。
- 两个 Cursor 会话同时运行时，不能串读日志；旧日志不能污染新回合。
- CLI 日志不可用/截断/关闭时仍由 stderr 和进程退出提供终态。
- 非零退出无 stderr、成功退出但无 result、is_error=true、未知结构化错误均覆盖回归测试。
- 下一条“继续”使用原 session ID；失败后不自动重发用户任务。
- 使用实际 --print --output-format stream-json 模式验证失败路径；TUI 日志不能替代该验证。
- 全量 Go 测试、相关 race tests，以及错误→提醒→继续的 CUJ 场景通过后再部署。

## 实现与验证结果

- 新增 core.AgentFailure / EventRetry 通用契约及五种语言文案；Cursor 负责识别原因，core 负责平台展示。
- stdout 错误、错误 result、进程退出及 stderr 汇总为一个终态；等待本轮清理完成后才发布终态，防止旧进程关闭下一轮 stdin。
- 日志按 PID、启动时间、请求链和已知会话 ID 归属；持续恢复更新进度，最终错误附模型、会话、提供方状态及请求 ID。没有证据的重试次数显示未知。
- 支持从本轮日志补回在 system/init 前建立的 session ID；用户停止不会产生 provider 故障提醒。
- 三个独立回归用例已在修改前的 1bae52a8 上验证失败：顶层错误丢失、错误结果变成功、空 stderr 异常退出没有终态。
- 脱敏日志 fixture 验证 Provider 400 与 no-progress，两进程并发验证诊断隔离，后续继续验证原 session ID。
- 原生 agent-us 非交互失败通过本地拒绝接口和测试代理注入；该 CLI 将此阶段的拒绝汇总成连接失败，适配器正确结束回合并保留提示。正常 Gemini 4 Argon High 两轮响应及 session ID 恢复通过。
- go test ./...、显式 CUJ、Cursor/core/Feishu race、go vet ./...、go build ./... 全部通过。

完整原始日志未提交。testdata 只保留错误识别所需字段，已替换请求标识。
