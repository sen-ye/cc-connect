# 上游同步与当前发布

2026-11-04：将上游从 `0c5141da` 合并到 `10442325`，包含 20 个提交（13 个非 merge 提交）。
发布前线上基线为 `ddb63d91`；当前发版状态以本次部署记录为准。

## 变化与兼容处理

- Codex 仅由匹配的 `turn/completed` 结束当前轮次。`thread/status/changed: idle` 可能早于最终文字或错误，也可能属于上一轮，不能据此提前结束。采用上游 [#2127](https://github.com/chenhg5/cc-connect/pull/2127)，保留本地自动重试、父子线程隔离及 provider/model/Fast 绑定。
- Claude 用量探针的 stderr 缓冲区增加同步，修复读取和子进程写入之间的数据竞争。
- 支持按钮的平台减少重复授权确认；取消且没有输出时不再发送空回复占位。额外修复取消标记跨轮残留：上一轮有部分回答或结果被丢弃时，不能静默吞掉下一轮正常的空结果。
- 新增真实二进制、桥接平台和模拟模型的黑盒发布检查，覆盖对话、工具、授权、会话恢复、图片和平台能力组合。模拟模型支持 Anthropic Messages 与 OpenAI Responses。
- Codex 大行读取测试调整超时，减少繁忙机器上解码 11 MB 响应时的误报。

仅有一处文本冲突，位于 Codex idle 处理。更新原子会话隔离测试以等待明确完成事件；在现有 C5 CUJ 增加“开始任务 → /stop → 下一轮空结果仍可见”的回归。

新黑盒检查还测出了合并前已存在的 `/new` 恢复问题：旧会话的原生 ID 和历史被清空，随后 `/switch` 新建内部记录而丢失 provider 绑定。现在只创建新会话，保留旧记录；现有 B3 CUJ 增加真实命令入口、两个 provider、切回与历史检查。旧线上 `ddb63d91` 二进制在同一黑盒用例下也失败，见 `baseline-smoke.log`。

Claude 的黑盒恢复失败来自测试隔离目录接线：CLI 继承的 `CLAUDE_CONFIG_DIR` 与 adapter 查找目录不一致。模拟模型测试改为通过项目 env 显式设置独立配置目录，使 CLI 和会话查找共用同一路径；未修改线上 Claude 配置。

## 验证

- Codex 三项上游 idle 回归和父子会话组合回归在合并前逻辑上失败，修复后通过。
- 取消标记残留在未修正的上游逻辑上复现；`/new` 丢失绑定在修复前的 B3 CUJ 上复现。相关 CUJ 与 race 检查覆盖修复后的行为。
- 全量 `go test ./...`、显式 CUJ、相关模块 race、`go build ./...` 和 `go vet ./...` 通过。
- 原生 Codex 对本地模拟接口的多轮、恢复、provider/model/Fast 保留及自动重试验证通过。
- 候选二进制使用 `tests/blackbox/smoke` 进行发布检查；结果记录在归档中的 `release-smoke.log`，部署状态以 `deployment.json` 为准。

采用 Go 1.25.0 与 `goolm` build tag，测试子进程使用 `umask 0022`。BN 挂载不支持 Go 所需文件锁，构建和测试使用本机副本，仓库仍是代码真值。Web 代码与已验证的 `eca216e0` 一致，复用其已归档 Web 产物。

## 发布与清理

发布记录、验证日志和候选程序归档：
`/mnt/hdfs/user_wl/senye.eason/exps/cc-connect/20261104-upstream/`。

线上部署入口：`/home/tiger/.cc-connect/deployments/upstream-20261104/deployment.json`。
独立部署进程等本次回复送达及其他会话空闲后替换程序，核验版本、三个飞书连接、Unix API、会话历史和 provider/model/Fast 绑定；启动验证失败时回滚。

只保留当前版本与直接上一版的回滚程序。四份已被替代的旧二进制副本在新版本验证成功后清理，并将清理结果写入部署记录；会话原始历史、配置及排障证据保留。本次构建和测试临时目录在发布前清理。
