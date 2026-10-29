# Timer 检查误入其他项目

2026-10-29，Faster-R3 话题的 03:46 检查回复变成了 cc-connect 构建与服务检查。
已对照飞书原消息、cc-connect 历史、原生 Codex rollout 和 scheduler 日志确认。

## 发生了什么

- 01:41 创建 timer `bfd9d95c`，两小时后执行“检查一下”，采用默认 reuse 模式。
- 定时任务保存了原飞书话题 session key，没有显式 work_dir。
- 03:41 触发时，回复目标仍是正确的 Faster-R3 话题，但执行端使用全局工作区。
- 新开原生会话 `01a18407-e910-7b01-b49f-3ae533de0cb4`，cwd 为 cc-connect 仓库，provider 为 AIDP。
- 原 R3 会话 `01a18284-535b-7b50-b2ea-ce7658275e0d` 的 Seed 绑定、工作区和训练历史未被覆盖。
  错误检查记录在另一个全局 session 中。这次 timer 没有检查 R3 训练。

## 原因与修复

飞书话题 session key 的格式是 `feishu:<chat>:root:<root>`，workspace binding 使用
`feishu:<chat>:topic:<root>`。普通交互已使用平台的 ChannelScopedSessionKey 能力，
但 timer/cron 仍用 extractChannelID，仅保留群 ID，丢失话题作用域；随后退回群默认或全局
agent/session manager。该代码路径早于本次 Maso 共享冷却改动。

定时执行改用共同的 scheduledWorkspaceContext：由平台恢复完整作用域，查询持久化绑定，
使用对应 workspace 的 session manager，并沿用其 active session/provider。显式 work_dir
仍有最高优先级，话题的 /dir 覆盖也得到保留。已绑定工作区不可用或 agent 创建失败时，
任务失败而不执行另一个项目；调度查找也不会删除不可用的绑定。

原生历史和已有消息保留，不靠清空 R3 会话来处理此问题。已触发的一次性 timer 不自动重放，
以免把另一个检查动作混入当前用户任务。

## 验证

新增回归在修复前失败，覆盖 timer 和 cron 的 topic/session/provider 路由以及错误回退。
在现有 H4 工作区隔离 CUJ 中增加完整路径：选择 provider → 正常对话 → 创建 timer →
持久化并重启 → 真实 scheduler 执行 → 查看原会话历史。另覆盖 /dir 与显式 work_dir 优先级。
测试使用本地模拟 agent/platform，不向用户话题发送测试消息，不调用真实模型或训练服务。

本次部署状态以 `~/.cc-connect/deployments/20261029-timer-workspace/deployment.json` 为准。
