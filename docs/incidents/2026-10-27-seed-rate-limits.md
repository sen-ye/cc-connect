# Seed 429 与 Maso 网关对照

## 实际故障

2026-10-27 18:03:43–18:04:06（北京时间），工作目录 `machine-migrations` 的
Seed / `gpt-6-astra` 会话 `01a17cd0-ccc6-7a31-9361-254ba60d9421` 持续约 23.6 秒后失败。
证据来自原生 Codex `logs_2.sqlite` 和 rollout，而非仅依据飞书摘要推测。

1. 18:03:44，HTTP 200 流中返回 Azure 错误：`deployment-gpt-6-astra-platform-global`
   在 `westus` 超出 token rate limit。原生 Codex开始重试，日志显示最大 sampling retries 为 5。
2. 18:03:59，Code Router 返回 HTTP 429，`X-RateLimit-Dimension: rpm_key`、
   `Retry-After: 1`、`X-RateLimit-Reset: 1793095440`。
   请求追踪号：`20261027180359FCDF5D0BB5E22F3A6EC6`。
3. 18:04:06，后续请求再次返回 429，最终呈现 `exceeded retry limit`。

18:03:30 的另一条 proxy cancellation 对应用户中止上一轮，不是这次限流原因。
Seed 的进程使用独立 loopback 地址和 Azure `api-key` 环境映射，实测凭据匹配 Seed
私有凭据文件。本调试会话保留旧 `csj` 原生绑定及原 CSJ 路由；其他 OpenAI 会话保存
的 provider 仍为 OpenAI。未发现跨供应商混用。终端 Seed 和飞书 Seed 使用同一 key，
可以共享该 key 的限额；不同入口名称不意味着不同配额。

## 能查到哪些额度

Maso 使用 `GET https://seed-code-router.bytedance.net/api/v1/quota/self`，以实际消费
key 作 Bearer 认证。2026-10-27 查询返回的日额度维度为 `daily_key_model` 和
`daily_key_total`：`gpt-6-astra` 为 `within`，显示比例 0%；`es1-grape-g61s` 为
`no_limit`。这是日额度状态，不说明分钟限流或后端资源池无限制。

此接口及 `POST https://maso.bytedance.net/cowork/api/v1/llm-configs` 均未返回该 key
的绝对 RPM/TPM 数字；故障响应也未包含 limit/remaining 数值。`Retry-After: 1`
表示建议等待 1 秒，不代表每分钟只有一次请求。确认具体分钟额度和 Azure 资源池
阈值仍需 Router/ModelHub 服务端信息。未通过高并发压测反推额度。

## Maso 行为与本地采用范围

审查源码：[`seed/maso`](https://code.byted.org/seed/maso)，commit
`0ec649dbcc9070d01c51a923470cef5fb0caa4ba`。

| 情况 | Maso 实现 | cc-connect / codex-seed |
|---|---|---|
| 未传输出预算 | 黑盒代理注入配置的 max_tokens；当前两模型为 64000 | 可配置 `codex.max_output_tokens`，只补缺失值 |
| 400 加密历史失效 | 黑盒代理剥离加密 reasoning，重试一次 | 同等有限恢复，保留正文、工具 ID 和原始历史 |
| 400 continuation identity 冲突 | Python 客户端仅在含加密 reasoning 时修复 | 采用同样的请求前提；compaction 不修复 |
| 429 / 5xx | 黑盒代理直接返回；Python SDK max_retries=5 | 原生 Codex 负责重试，代理不叠加请求重试 |
| 限流诊断 | Python hook 记录 dimension / Retry-After | 代理记录脱敏头信息，保留原响应供 CLI 使用 |
| 限流响应头 | 黑盒 host proxy 只保留 content-type/cache-control | 保留 Retry-After、X-RateLimit-*、X-Should-Retry |
| CLI 整轮失败 | 外层最多继续 10 次，2、4…1024 秒指数等待 | 不在 HTTP 代理里复制整轮恢复或工具执行 |
| Python KeepAlive | 默认 5 秒指数等待，单次上限 600 秒，累计 sleep 预算 86400 秒 | 不引入隐式长时间重试或自动换供应商 |
| 有备用供应商 | KeepAlive 可在连续失败后切换 provider | 每条会话继续保持自己的 provider 绑定 |
| 取消 / 流中断 | 流式转发并释放上游连接 | 取消传播；HTTP 200 后不由代理自动重放 |
| 异常或大请求 | 黑盒 host proxy 最大请求 64 MiB | 转换预算 32 MiB；超过预算原样转发 |

源码定位：

- `packages/agents/src/maso_agents/agents/blackbox/llm_proxy.py:127,287`：预算注入和转发。
- `packages/agents/src/maso_agents/agents/blackbox/options.py:23`、`agent.py:188`：CLI 外层继续。
- `packages/agents/src/maso_agents/agents/blackbox/adapters/codex.py:234`：非重试错误判断。
- `packages/core/src/maso_core/llm/azure_responses.py:201`：加密续接判断。
- `packages/core/src/maso_core/llm/config.py:13`：SDK timeout=300 秒/max_retries=5。
- `packages/core/src/maso_core/llm/rate_limit_logging.py:13`：限流元数据白名单。
- `packages/core/src/maso_core/runtime_modules/keep_alive/plugin.py:249,1170`：KeepAlive 设置和等待。
- `packages/cowork/src/maso_cowork/server/api/routes/llm_quota.py:33,277`：quota/self，8 秒超时、60 秒缓存。
- `packages/cowork/src/maso_cowork/server/router_credentials.py:31`：消费 key 的解析。

连接池容量 5000、可选 agent 数限制和 CLI 输出预算 64000 都不是 Router 的 RPM/TPM。
把所有错误无条件重试也不能增加上游容量。认证失败、无效请求、上下文溢出以及明确的
额度耗尽需要修正原因；流已开始后的恢复应由持有原生会话状态的 CLI 判断。

## 验证

使用真实原生 Codex 和本机模拟网关验证并发供应商、分别多轮、退出后恢复和独立代理；
两个后端（exec / app_server）及直连/固定代理组合通过 race 检查。网关回归覆盖预算
注入与密文修复组合、HTTP 429 头透传、敏感信息不写日志、取消、SSE、大请求及非目标
endpoint 透传。测试不使用真实模型压测，不修改用户会话历史或共享认证。
