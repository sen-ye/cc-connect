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

## 网关切换账号后的加密 reasoning 恢复

对于可能在后端账号之间路由的 Responses 网关，可以为单个 provider 启用：

```toml
[[projects.agent.providers]]
name = "Gateway"
base_url = "https://gateway.example/openai?api-version=2024-03-01-preview"

[projects.agent.providers.codex]
wire_api = "responses"
reasoning_replay_fallback = true

[projects.agent.providers.codex.env_http_headers]
api-key = "GATEWAY_API_KEY"
```

此选项默认关闭。开启后，每个原生会话使用独立的本地代理；不配置 `proxy_url` 时，
该代理直接连接网关，不继承 cc-connect 的公司 HTTP 代理。配置里的查询参数和请求
认证头会保留，不需要把 key 写入 URL 或启动参数。

第一份请求始终原样发送。仅当 `POST .../responses` 返回 HTTP 400，错误含
`invalid_encrypted_content`，且请求中确有可清理的 reasoning 项时，代理才补发一次：
保留 reasoning summary、用户/助手正文及工具调用，删除 reasoning 的加密状态、
`content` 和 `id`；没有 summary 的加密 reasoning 项整体省略。该恢复会失去旧的
隐藏推理连续性，但不修改原生 rollout 或切换 provider。

第二份响应直接返回，不递归重试。HTTP 200/SSE 内失败、流中断、429/5xx、compaction
密文、`previous_response_id` 等不在此修复范围。压缩请求、超过 32 MiB 的请求、超过
1 MiB 的错误响应也仅透传。实际额外请求是这一层的一次重试；CLI 自己可能另有重试。

建议配置该 provider 的显式 `models` 列表；模型配置服务和推理接口可能采用不同的
URL 与鉴权，不能把 `/models` 返回 404 当成推理 key 失效。新增 provider 不会自动
替换默认项，也不会改变已有会话的账号绑定。

## 独立终端入口

`cmd/codex-provider` 可复用同一套代理启动交互 CLI，不依赖 cc-connect 服务进程。
它读取独立 TOML，执行原生 Codex 并保持终端输入输出直通；进程结束后关闭代理。
使用 `--no-daemon`，避免短生命周期的代理被共享后台服务继续使用。

```bash
go build -o codex-provider ./cmd/codex-provider
./codex-provider --config /path/to/provider.toml -- -m model-id
./codex-provider --config /path/to/provider.toml -- resume SESSION_UUID
```

配置格式如下；`provider` 的字段与 cc-connect 单个 provider 一致，凭据通过环境提供。
`binary` 建议指向原生 Codex 可执行文件，让 TERM/HUP 直接传递给 CLI。

```toml
binary = "/path/to/native/codex"
model_catalog_json = "/path/to/provider-models.json" # 可选，Codex 格式的专用模型目录
[provider]
name = "Gateway"
base_url = "https://gateway.example/openai?api-version=2024-03-01-preview"
model = "model-id"
models = [{model = "model-id"}, {model = "another-model-id"}]
[provider.codex]
reasoning_replay_fallback = true
[provider.codex.env_http_headers]
api-key = "GATEWAY_API_KEY"
```

启动时的 `-m` 必须属于配置的模型列表。恢复时不覆盖已保存的模型；`resume`/`fork`
要求明确的原生 session UUID，并读取本地历史确认它属于同一 provider。无参数 picker
和 `--last` 不受支持；其他 provider 的历史请使用原入口恢复。此检查不会修改历史。
终端 Ctrl-C 由原生 Codex 处理，不会因取消一轮任务而提前关闭代理。

本机的 `codex-seed` 使用上述入口，凭据读取脚本为 `tools/codex-seed.py`：

```bash
codex-seed                         # gpt-6-astra
codex-seed -m es1-grape-g61s
codex-seed resume SESSION_UUID
```

脚本优先读取 `CC_CODE_ROUTER_API_KEY` / `SEED_CODE_ROUTER_API_KEY`，否则读取
`~/.codex/code-router-auth.json`，不会将 key 写到 argv 或配置。独立配置、模型目录和
固定 affinity 保存在 `~/.config/codex-seed/`，程序在 `~/.local/share/codex-seed/`。
它与普通 Codex 共用原生历史和用户设置；全局默认 provider 保持不变。

Maso 在 `packages/utils/src/maso_utils/providers.py` 中通过带 Bearer 认证的
`POST /cowork/api/v1/llm-configs` 取得配置，然后按协议和 endpoint 选择模型。
该响应包括凭据，不应原样保存或输出。它不是 Codex 的标准 `/models` 目录；
模型配置中的 high/xhigh/max 项是同一 model ID 的不同预设。本机 Seed 仅列出
`gpt-6-astra` 和 `es1-grape-g61s`。后者的本地 CLI 元数据基于现有 GPT-6.1 Sol
模板，实际请求保留 `es1-grape-g61s` 名称，不构成网关能力或上下文上限保证。

## 每条会话独立设置 Fast

使用 Codex `app_server` 后端时，可在当前飞书话题中发送：

```text
/fast off       # 本会话使用标准速度
/fast on        # 本会话使用 Fast
/fast status    # 查看当前设置；/fast 也可查看
```

命令只修改当前 cc-connect 会话，不改全局 `~/.codex/config.toml`、provider、模型或
reasoning。未使用此命令的会话继续继承原默认值；新建会话也重新继承默认值。

设置在第一条任务前和会话运行期间都可选择。已经发出的模型请求保持原档位；新的
设置用于后续轮次。无需取消当前任务、创建新的原生会话或重启 cc-connect。
`/stop` 后恢复、进程重建、服务重启和定时任务都会恢复该会话保存的设置。
不支持此功能的后端会明确提示，不会把命令作为普通任务提交给模型。

实现使用 Codex `thread/settings/update` 的 `serviceTier`：`off` 对应 `default`，
`on` 对应 `fast`。若恢复保存的档位失败，会关闭新进程并报错，不提交模型请求；若保存失败，
会尝试恢复原运行档位并报告错误。实际计费以 API 服务商返回的服务档位与账单为准。

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
