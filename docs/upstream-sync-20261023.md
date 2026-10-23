# 2026-10-23 上游同步

本次将上游从 `2dec7173` 合并到 `04f946bc`，包含 41 个 commit（25 个非 merge commit、17 个 PR）。重叠部分优先采用上游实现，同时保留本地独有功能。

## 用户可见变化

| 变化 | 生效条件 / 上游依据 |
| --- | --- |
| Codex 异步提问可显示问题卡片，通过点选或文字回答。仍在运行时追加到同一轮，结束后则作为下一轮消息；支持逐题回答。 | Codex `backend="app_server"`；本机已使用该后端，无需新配置。[#2048](https://github.com/chenhg5/cc-connect/pull/2048) |
| Claude CLI 自行切换模型时，显示原模型、目标模型和影响范围，并更新卡片的实际模型/context，给出切回命令。 | CLI 发出 `model_refusal_fallback` 时生效；此项是通知和状态同步，不新增自动切换策略。[#2054](https://github.com/chenhg5/cc-connect/pull/2054) |
| Claude 恢复会话时忽略空后台任务通知，避免它提前结束当前回复；新一轮到来时补发已收到的后台文字。 | 默认生效。[#2069](https://github.com/chenhg5/cc-connect/pull/2069) |
| Codex exec 模式等待旧进程退出、释放会话写锁后再开始追问。 | `exec` 后端。[#2063](https://github.com/chenhg5/cc-connect/pull/2063) |
| 已失效的问题按钮不再作为普通消息或权限回答提交。 | 默认生效。[#2061](https://github.com/chenhg5/cc-connect/pull/2061) |
| 飞书富文本 `@机器人 /命令` 正确去除机器人 mention 占位符。 | 默认生效，保留其他人的 @。[#2029](https://github.com/chenhg5/cc-connect/pull/2029) |
| Codex 会话列表去重，默认标题使用首次真实输入。 | 默认生效。[#2060](https://github.com/chenhg5/cc-connect/pull/2060) |
| 跨项目 relay 超时不再把工具输出当作回答；部分答案有明确超时提示及长度上限。 | 默认生效。[#2028](https://github.com/chenhg5/cc-connect/pull/2028) |

其他更新包括：Claude 按项目 `CLAUDE_CONFIG_DIR` 查找会话；Slack 表格和转发内容提取；微信正常回答不占主动推送额度；ACP 会话恢复和分段工具名称；Gemini 工作区保留选项；CC-Switch 拒绝歧义导入；压缩完成日志和 Windows 进程退出兼容性。

本轮没有新增 terminal、图片生成/发送、timer 或 reasoning 档位。

## 合并中的兼容处理

- Codex 的异步问题处理与本地父线程/当前 turn 隔离并存，普通正文仍及时显示，兼容 `content[]` 文本。
- Codex exec 的追问锁保留上游实现，关闭时继续释放本地每会话 provider proxy。
- Claude 的空后台任务通知在本地 UUID 归属判断之前过滤，避免无效通知消耗前台请求或污染去重状态。
- 压缩完成日志在过滤中间结果和后台结果之后记录，仍先于排队的下一轮。
- 保留每会话 provider/API key/固定代理绑定、reasoning 持久化、`/ps` 图片和文件追加、完整 session ID 页脚、工具显示与飞书卡片恢复逻辑。

新增交叉回归测试：

- `TestAppServerSession_AsyncQuestionsPreserveTurnIsolationAndProgress`
- `TestResultScope_EmptyTaskNotificationDoesNotMutateCorrelation`
- `TestProcessCompressEvents_IntermediateResultsDoNotLogCompletion`

三个测试均在独立副本模拟错误合并时失败，在正确合并版本通过。

## 构建与发布记录

BN 挂载上的 Go 文件锁返回 `function not implemented`，因此保留本仓库作为代码来源，在 `/opt/tiger/yesen/tmp/cc-connect/upstream-20261023/src` 编译和测试。Go 1.25.0，使用 `goolm` build tag；发布二进制采用 `CGO_ENABLED=0`。Web 使用 pnpm 10.24.0 按现有 lockfile 安装，再执行 TypeScript/Vite 构建。

全量 Go 测试、显式 CUJ 测试、相关包 race 测试、原生 Codex 对本地模拟 API 的并发/provider/固定代理/恢复测试、`go vet ./...`、`go build ./...` 和 Web 构建均通过。部署 worker 的 17 个针对性测试也通过。

测试子进程使用 `umask 0022`。第一次全量运行继承了服务的 `0077`，导致上游 systemd 测试创建的旧文件实际为 `0600`，不满足该测试预设的 `0644`；只调整测试子进程的 umask 后重跑，没有更改线上权限设置或 daemon 代码。

发布产物和验证日志归档：`/mnt/hdfs/user_wl/senye.eason/exps/cc-connect/20261023-upstream-04f946bc/`。

线上通过独立 worker 等待回复送达及所有会话空闲后，仅替换 cc-connect 二进制。重启后核验二进制、两个飞书连接、API、会话历史与 provider 绑定，以及原配置和代理进程保持一致。当前 AIDP 凭据沿用 10 月 23 日已完成的轮换配置；本次不变更凭据。最终部署状态以归档中的 `deployment.json` 为准。
