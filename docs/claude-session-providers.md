# Claude Code：按会话选择 provider

Claude Code 可与 Codex 一样，在会话的第一条任务之前选择 provider。选中后绑定当前 cc-connect 会话；另一个话题可以使用不同账号，重启或恢复会话时继续原来的绑定。

```text
/new
/provider list
/provider switch Mojo
```

随后发送任务。已开始的会话不能切到另一 provider；需要另一个账号时先 `/new`，原会话及历史保留。

## 复用已有 Claude settings

`settings_file` 指向该 provider 的 Claude Code settings JSON/JSONC 文件，启动时以 `--settings` 传给 CLI。可以保留 `apiKeyHelper`、模型映射等原生设置，无需将凭据复制到 cc-connect 配置中。

在对应 Claude 项目的 agent 配置中添加：

```toml
[projects.agent.options]
provider = "Default"
# 其余已有选项保持原值。

[[projects.agent.providers]]
name = "Default"
# An empty provider uses the original CLI configuration.

[[projects.agent.providers]]
name = "Mojo"
settings_file = "~/.claude/settings.mojo.json"
```

支持绝对路径、`~/` 路径，以及相对于该会话工作目录的路径。显式 `model` 优先于文件里的 `model`；文件和账号配置缺失、不可读或 JSON 无效时会报错，不会悄悄换成默认账号。

provider 也可沿用 `base_url`、`api_key`、`env` 和 `thinking` 配置。需要网络代理时在该 provider 的 `env` 中配置 `HTTP_PROXY` / `HTTPS_PROXY` 等；本机 Mojo 网关应通过 `NO_PROXY` 直连。

## 隔离与兼容

- 不修改共享的 `~/.claude/settings.json` 或认证文件。
- 每次启动使用独立的 provider 环境和 thinking 重写代理；更改项目默认值或关闭其他会话不会改动已启动的会话。
- 旧会话没有显式 provider 时，继续使用原 CLI 默认配置，不自动迁到后来设置的新默认 provider。
- Claude 子 agent 在同一 Claude CLI 会话内继承其 provider 设置；cc-connect 的 timer 和恢复路径也使用已保存的绑定。

验证包含并发账号、代理生命周期、默认/原生恢复隔离、settings 文件持久化和 core provider CUJ。原生多轮/关闭后恢复测试可显式运行：

```bash
CC_CLAUDE_NATIVE_TEST_BIN=/path/to/claude \
CC_CLAUDE_PROVIDER_SETTINGS=/path/to/settings.provider.json \
go test ./agent/claudecode -run TestNativeClaude_SettingsProviderMultiTurnAndResume -count=1
```

该可选测试会调用指定 provider，禁用工具，使用临时会话目录，不修改输入 settings。
