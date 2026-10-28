# Seed / Maso 并发与退避

统计时间：2026-10-29 01:16:28（北京时间）。服务基线为 `42bf1013`。
来源：cc-connect `tmux.log`、原生 Codex `logs_2.sqlite`、`state_5.sqlite` 与 rollout 的任务起止事件。
未压测上游，也未输出 API key、请求正文或用户会话正文。

## 限流与并发

最近一小时（00:16:28–01:16:28）的原生日志记录 250 次 HTTP 响应：200 次 HTTP 200、
50 次 HTTP 429（20%）。这 50 次触发原生 sampling 重试，错误均为 429；本机网关同时
记录的限流维度均为 `rpm_key`。HTTP 200 指收到成功响应头，不等于独立验证每条 SSE
均完成。该窗口内没有触发 cc-connect 的整轮失败续跑。

最近 15 分钟原生日志记录 154 次 HTTP 响应，其中 41 次 429（26.6%）。其中有一次来自
独立 Seed CLI，不在 cc-connect 网关日志内；不同日志层不能相加当成更多请求。

任务生命周期显示峰值为 6 个活跃 agent 任务（01:03:13 起短暂重叠）：robosim 主会话、
其 3 个子 agent、R3 主会话和一个随后取消的独立 CLI 会话。到统计时刻，robosim 主会话
与 2 个子 agent、R3 主会话仍活跃，共 4 个。该值是任务并发，不是已测出的上游并发额度
或精确的同时在途 HTTP 请求数。两个主进程的 Seed key 与同一本地保存的 key 一致。

原生 HTTP 日志缺少完整 SSE 持续时间；当前保留数据也不足以完整还原部署后的全部请求。
因此不据此估算 HTTP 在途峰值或把 24 小时错误数除以不完整的原生请求数。

## 能否取得具体 RPM

只读查询 `GET https://seed-code-router.bytedance.net/api/v1/quota/self`，该接口仍仅返回
`daily_key_model` 和 `daily_key_total`。截至约 01:16，GPT-6 Astra 日额度状态为 `within`，
使用比例 4%。响应没有绝对 RPM/TPM 数值；错误头也只有 `X-RateLimit-Dimension`、
`Retry-After` 和 reset，没有 limit/remaining。

因此目前能确认“按 key 的分钟请求数限流”，不能确认配置究竟是每分钟多少次。
日额度使用比例低不意味着分钟请求不受限；观测到的吞吐也不是配置额度。实际数值
需要 Code Router 服务端的 key 配额配置或有权限的管理接口。

## 三层重试的区别

1. 本地 provider proxy 不额外重放 429/5xx，保留原响应和等待头。
2. 原生 Codex 持有 HTTP/sampling 重试。上述一小时内采样重试等待中位数约 26 秒、
   最大约 54 秒，跟随服务端 Retry-After。日志里的 50 条 `1/5` 属于不同请求/agent 的
   分散重试，不能当成同一次请求重试 50 次。
3. cc-connect 只有在原生已宣告整轮失败后才启动同会话续跑；上述窗口触发 0 次。

本次按用户要求，为 Seed/Maso 增加共享冷却，并单独提供第 3 层的上限覆盖：

```toml
[projects.agent.providers.codex]
auto_retry_max_delay_seconds = 32
```

该字段默认 0，表示继承默认 10 秒。它在启动/恢复会话时作用于该 provider 的策略副本。
32 秒时指数序列为 2、4、8、16、32、32…秒，抖动也受 32 秒上限约束。沿用一小时
累计等待预算，理论最多 115 次（基础合计 3582 秒，抖动至多另加 3 秒）；显示上限相应
降低。成功推进后从 2 秒/第 1 次重新开始，其他 provider 保持原策略。

如果整轮失败时服务端最短等待仍大于配置上限，外层提示等待，不能为了满足上限提前请求。
原生 Codex 的独立重试仍可按服务端要求等待超过 32 秒。此选项不改变 `codex-seed` 的原生
sampling 策略，不改 key、模型或 provider 绑定，也不改变 Claude/Cursor 行为。

## 同 key 共享冷却

为了降低当前原生重试的碰撞，Seed 的 cc-connect、codex-seed 和 code_router CLI
统一连接本机 19618 入口，在其无凭据的 provider 配置上启用：

```toml
[provider.codex]
rate_limit_backoff_max_seconds = 32
```

开关默认 0（关闭）。代理收到普通 Responses POST 的可重试 HTTP 429 后，以 API key
的 SHA-256 摘要定位独立冷却状态。后续同 key 请求在发出前等待；冷却期为
2、4、8、16、32、32…秒与服务端 Retry-After/Retry-After-Ms 最短等待的较大值。
因此指数部分封顶 32 秒，上游明确要求 55 秒时仍等待至少 55 秒。

冷却后一次放行一个试探请求，成功后逐个处理积压请求，队列清空后恢复正常并发。
冷却之前已发出的请求不会重放，旧请求的成功响应也不会提前清空新的冷却。
不同 key 不互相等待；取消会移除等待者。日额度耗尽、X-Should-Retry=false、非 Responses
端点不启动这套自动冷却。HTTP 200 内的 SSE 错误不在此层解析范围，仍由原生客户端处理。

这层只控制下一次发送时机，不增加一次 HTTP 重试，不读取/改写 SSE，也不修改鉴权、
会话 affinity 或历史。状态不保存原始 key，健康接口仅提供冷却 key 数、排队数和剩余等待。
超过 32 MiB、压缩或不能完整缓冲的请求保持直接转发，以保留上传/取消语义。
这不是猜测配置 RPM 后强行套入一个限额，不能保证以后完全不出现 429。

## 验证

回归测试覆盖 provider/key 隔离、串行试探、取消排队、不重放请求、SSE 保真、服务端最短等待、
配置/管理接口往返、2–32 秒序列、115 次预算、成功后清零，
以及原生 Codex 对本地模拟 429 的续跑、取消和历史保持。测试不向真实模型发压测请求。
本次发布结果以 `~/.cc-connect/deployments/20261029-seed-retry32/deployment.json` 为准。
