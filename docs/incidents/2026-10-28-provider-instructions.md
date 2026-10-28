# Codex 四个 provider 的输入指令对照与补充

日期：2026-10-28。基线 cc-connect `01ec8763a874f26401ce84cf254a8efa3d141616`，原生 Codex 0.164.0。
脱敏证据：`/mnt/hdfs/user_wl/senye.eason/exps/cc-connect/20261028-provider-autonomy`。

## 已证实的差异

用户的 OpenAI provider 是账号经 Sub2API 转发的入口。抓取实际安装的 Codex `exec`
和与 cc-connect 同样初始化的 `app-server` 请求，二者均没有顶层 `instructions`。
首个 input 项为 developer 角色的 `additional_tools`；其后的普通 developer 消息
已经包含客户端模型基础模板。

把同一份完整 developer 模板与相同用户请求发送到四个 provider，观察到：

| Provider | input tokens | 返回 instructions 字符数 |
|---|---:|---:|
| OpenAI / Sub2API | 8238 | 21260 |
| AIDP / CSJ | 4124 | null |
| Maso / Seed | 4124 | null |
| Mojo | 4124 | null |

额外指令的 attribution 为 4114 tokens，trimmed SHA-256 为
`be213cc3a9566255f6d43f61c54cbc33cafbc3461680ad6513afa0850050c7d6`。
这证明了有效输入有差异，不能单凭它证明自主性差异全部由此造成。
没有通过模型自述来判断是否注入提示词。

## Sub2API 的模板来源

核查源码版本 `bf02c1a6e0f78816b728e1e16854afa5c8fabe6b`：

- [openai_codex_transform.go](https://github.com/Wei-Shaw/sub2api/blob/bf02c1a6e0f78816b728e1e16854afa5c8fabe6b/backend/internal/service/openai_codex_transform.go)：`applyInstructions` 在普通 OAuth/Codex 路径补充空缺指令；兼容分支可以跳过默认模板。
- [constants.go](https://github.com/Wei-Shaw/sub2api/blob/bf02c1a6e0f78816b728e1e16854afa5c8fabe6b/backend/internal/pkg/openai/constants.go)：Astra 模板注释指向 `openai/codex` 的 `121f91fd5d9d`、`codex-rs/models-manager/models.json`；GPT-6.1 Sol 描述符指向 `b1e72963c3b71a9265a551e54beff078384efed9`。
- [instructions_gpt6_astra.txt](https://github.com/Wei-Shaw/sub2api/blob/bf02c1a6e0f78816b728e1e16854afa5c8fabe6b/backend/internal/pkg/openai/instructions_gpt6_astra.txt)：trim 后与上述实测返回值完全匹配。

该 revision 对普通 `gpt-6-sol` 没有单独分支，会落到 GPT-5.5 默认模板，因此不能复制其
模型分发逻辑到本机。官方内部端点的字段兼容要求也不等于必须重复整份 21K 字符模板；
显式短指令在 OpenAI 与 AIDP 均成功。OpenAI 官方文档网页在此次环境中返回 403，
没有将第三方源码注释当作已独立核验的官方网页说明。

## 实现

新增 provider 选项 `codex.mirror_developer_instructions`，按用户后续要求默认开启。
本机 OpenAI 官方入口显式设置 false，保留上游原行为；其他 provider 无需逐个开启。
仅普通 Responses POST 的 instructions 缺失、null 或空白时，复制第一条普通 developer
消息的纯文本。跳过 `additional_tools`；不以权限消息替代不可用的首条 developer 正文。
保留原 input、item ID、call ID、tools、reasoning 密文、缓存 ID 和显式 instructions。
模板来自客户端已选定的当前模型，Astra/Sol/Seed 别名无需由网关猜测。
这会重复输入并增加 token 计量；缓存命中和能力提升都不是保证。

压缩请求、32 MiB 以上请求、1 MiB 以上模板、模型目录、compaction、混合非文本内容不补。
默认值转换采用 RawMessage，避免 JSON 大整数精度变化。Go 代理透传原始 SSE、HTTP 状态
和端到端头；不使用 SDK 重新拼装事件，不新增限流重试循环。
共享 relay 不保存或注入 key/affinity，各请求仍携带自己的身份。

Mojo 在现有 `runNative` adapter 的上游请求副本中实现相同的默认补充和显式关闭规则；原始客户端历史
继续用于原有任务复用校验。模型一致性检查和禁止切换来源的约束保持有效。

## 本机入口与首次启用记录

下表记录独立 CLI 启用时的状态。后续 cc-connect 部署状态以
`~/.cc-connect/deployments/20261028-provider-instructions/deployment.json` 为准。

| 入口 | 补充位置 | 当前状态 |
|---|---|---|
| 普通 Codex 默认 AIDP | 本地 19617 relay → canonical ModelHub | 新启动进程已启用 |
| 直接选择 `model_provider=code_router` | 本地 19618 relay → 原 Code Router | 新启动进程已启用 |
| `codex-seed` | 独立 codex-provider 进程内代理 | 启动器与配置已更新 |
| `codex-mojo` / 原生 mojo provider | Mojo gateway 的 native adapter | 空闲确认后已重启并健康 |
| cc-connect 默认 AIDP | 原生 Codex 读取同一 AIDP 配置 | 新启动的原生进程使用新出口 |
| cc-connect 显式 provider | 默认补充；OpenAI 显式 false | 通过独立部署流程替换，见 deployment.json |
| OpenAI / Sub2API | 上游现有补充 | 不再本地重复添加 |
| 历史 `csj` provider | 19619 补指令 → 原 19516 CSJ | 原 key、provider ID 与 CSJ 后端保留 |

原生默认 provider 仍是 aidp；更新前后已核对 key 引用、affinity 头、query_params、
retry 参数未改变；历史 csj 只在原 CSJ 入口前增加无凭据转发层。运行中的 Codex 不会中途热切换请求出口。
普通 Codex wrapper 与机器启动脚本调用 `ensure-codex-proxies`，服务独立于 cc-connect，
并在 tmux pane 内清空继承环境。显式 provider 的失败不会自动改用其他 provider。

安装、私有备份与候选程序：`~/.local/share/codex-provider/20261028-instructions/`。
状态文件：`state.json`。部署时重新对照当前 cc-connect 配置，仅合入 OpenAI 的显式 false 与 Seed 的 true，
不能使用旧候选快照覆盖期间用户新增的设置。替换在发起部署的回复送达、活动任务空闲后
执行，并核验会话历史、provider 绑定、凭据、代理进程、飞书连接与本地 API；失败时保留
备份并尝试回滚。用户原有 AGENTS.md 和其他 incident 工作区改动未纳入本次范围。

## 验证与限制

- Go 全量 `go test ./...`、`go test ./core/ -run TestCUJ`、`go build ./...`、`go vet ./...` 通过。
  首次全量测试的 daemon 权限断言受 shell umask 077 影响；用 umask 022 重跑通过，未改产品代码绕过。
- provider instructions 与 CLI 服务模式的 race 测试通过；并发请求使用不同身份、affinity、模板，核验无串用。
- 使用真实安装的 CLI 和模拟上游，完成两轮及进程重启后 resume，核验每次注入和身份保持。
- AIDP 新 relay、Seed 新 relay：真实上游各完成三轮（强制工具 → 工具结果 → 回忆），每轮返回正确模板。
- AIDP、Seed：真实 Codex 各通过一次 shell 读取文件，再退出进程、resume 原 session、正确回忆内容。
  这些简单测试没有产生 encrypted reasoning，不能称为此轮已验证真实密文重放；密文保真由回归测试覆盖。
- 修改前的独立 direct AIDP 算术实验另有三轮真实密文重放成功证据，仅代表短期同账号成功，不代表跨日恢复。
- Mojo 130 项离线测试通过，包括原始历史不变、显式覆盖、按客户端模型模板、默认启用及显式关闭的 adapter。
  此前真实多轮实验受上游 `upstream_model_mismatch` 阻断，不能将离线通过称为上游多轮通过；未放宽校验。

修改前，CSJ 的强制工具请求两次复现 HTTP 200、零字节 SSE（原 affinity 和隔离 affinity）。
同样的 canonical ModelHub 直连三轮成功；对 CSJ 精确上游 URL 做 SDK 原始响应读取也成功。
因此具体 CSJ 内部故障函数尚未锁定；AIDP 新出口绕开 SDK 重组流的路径，并保留旧 CSJ 供历史 provider 使用。

## 缓存与账号绑定背景

[ModelHub Prompt Cache](https://bytedance.larkoffice.com/wiki/ODGmwD8MliLaaMkQUgJcb1W1nIi)、
[Responses 接入](https://bytedance.larkoffice.com/wiki/JyLQwnrJFiczHyklYiFccYebnFf)、
[cache best practices](https://bytedance.larkoffice.com/wiki/DBaTwhzpsiuFBmkxDSNckvnHnHg)
说明了稳定 session affinity 的重要性。`extra.session_id` 的账号路由与 `prompt_cache_key`
不是同一机制，固定 ID 也不保证永久缓存或后端永不切账号。
此前按正确产品名 `azure-gpt-6-astra` 查询已确认账号绑定有效；使用 model ID 查询的否定
结果不构成未启用绑定的证据。本次未修改 ModelHub 的绑定时长或平台配置。
