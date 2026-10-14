# Codex：每个会话绑定 API key 和固定代理

在会话的第一条任务消息之前，用 `/provider switch <名称>` 或 `/provider` 卡片选择 API 配置。
绑定只作用于当前 cc-connect 会话，不改变其他会话或项目默认值。

```text
/provider list
/provider switch api-a
/provider current
```

随后直接发送任务。另一个话题可以同时选择 `api-b`。`/stop`、进程退出或 cc-connect
重启后，再次恢复同一会话会沿用其绑定。Codex 原生子 agent 继承父会话的 API 配置；
timer 复用会话时也使用目标会话的绑定。

会话开始后，重复选择相同配置不会重置会话。切换到其他配置或清除绑定会被拒绝，并提示
先 `/new` 再选择。原会话的 session ID 和历史保留。绑定的配置被删除或恢复失败时，
不会静默改用其他 key，也不会因为恢复失败而自动开一个空白对话。

## 配置

以下片段放在已有 Codex 项目的 `[projects.agent.options]` 后面。实际 key 通过运行
cc-connect 的服务环境提供；配置只记录变量名。`env_key` 指的是环境变量名，不是 key 本身。

```toml
[[projects.agent.providers]]
name = "api-a"
base_url = "https://api-a.example/v1"

[projects.agent.providers.codex]
env_key = "CODEX_API_A_KEY"
wire_api = "responses"

[[projects.agent.providers]]
name = "api-b"
base_url = "https://api-b.example/v1"

[projects.agent.providers.codex]
env_key = "CODEX_API_B_KEY"
wire_api = "responses"
proxy_url = "${CODEX_API_B_PROXY}"
# 可选：完整模型目录地址；配置了 proxy_url 时也走同一代理。
model_catalog_url = "https://api-b.example/v1/models"
```

两种地址分别控制不同的连接：

- `base_url`：接收模型请求的 API 服务或中转站，类似 Claude 的 `ANTHROPIC_BASE_URL`。
  两把 key 可以使用相同地址，也可以各自使用不同的中转站。
- `codex.proxy_url`：可选的额外 HTTP/HTTPS 网络代理，例如 `http://proxy.example:8118`。
  每份配置固定对应自己的代理；不配置时保留通常的连接方式。
  此设置只作用于模型 API 请求，不修改全局代理，也不替其他会话或工具命令切换网络。

`api_key = "${CODEX_API_A_KEY}"` 也受支持；`codex.env_key` 更适合让配置文件只保存变量名。
已有全局 `[[providers]]` 和 `provider_refs` 同样可以使用上述 Codex 字段。

若 API 中转需要从进程环境读取固定会话标识，可以增加请求头映射。例如，已有启动脚本
提供 `AIDP_SESSION_ID` 时，CSJ proxy 配置可以使用：

```toml
[projects.agent.providers.codex.env_http_headers]
x-session-id = "AIDP_SESSION_ID"
```

`http_headers` 仍可配置固定请求头；值在启动时通过子进程环境传递，不会放进启动参数。
额外网络代理的地址也不会进入 Codex 启动参数。

## 默认值与兼容性

- 未选择配置的新会话在首次启动时记录项目默认 provider；没有配置项目默认时使用 CLI 原生配置。
- 只添加 `providers` 而不设置项目 `provider`，不会自动把第一项设成默认，也不用复制已有默认 key。
- 已有原生会话但没有 cc-connect provider 记录时，保留 CLI 的恢复路径，不覆盖其认证文件。
- 修改管理界面的项目默认 provider 只影响之后开始的会话，已经绑定的 Codex 会话不会被重置。
- 本功能不写共享的 `~/.codex/auth.json` 或 `config.toml`。provider 参数由每个进程单独携带。
- 更换 provider 配置中的实际 key 或地址仍可能改变上游账号；旧加密历史是否能恢复取决于上游。
  多账号的正常用法是给不同配置分配不同别名，在新会话开始时选择。

## 验证

普通单测包含命令/卡片绑定、重启持久化、缺失配置保护、两种 Codex 后端的并发子进程，
以及代理透传、SSE 和取消连接测试。原生 CLI 验证使用本机模拟接口，不访问真实模型：

```bash
CC_CODEX_NATIVE_TEST_BIN=/path/to/native/codex go test ./agent/codex \
  -run '^TestNativeCodex_SessionProviderIsolation$' -count=1
go test ./core -run TestCUJ_F1 -count=1
```
