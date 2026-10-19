# 10: 将 sandbox 执行器注册为 MCP exec 工具

Status: ready-for-agent

## 目标

按用户后续要求，将现有内部执行器注册为 `exec`，保持 [规格](../spec.md)中的 sandbox 边界及原有文件工具行为。

## 验收条件

- [x] `tools/list` 包含 `exec`，支持字符串 `command` 和可选字符串数组 `args`，拒绝工具级 cwd、环境及挂载覆盖。
- [x] YAML `allow_tools` 接受 `exec`；未授权的 Agent 不获得该工具。
- [x] 调用现有 sandbox 执行器，返回输出、退出码和截断标记；普通非零退出为执行结果，启动和中断错误保留输出并分类。
- [x] 完整 MCP 响应遵守 1 MiB 字节上限，截断时不重跑命令；结构化结果和文本一致。
- [x] 宿主通过配置或三个 CLI 的可重复 `--runtime-dir` 参数配置只读目录，工具调用方无法增加挂载。
- [x] sandbox 不可用时仅 exec 失败，文件工具继续工作；新增真实 MCP 调用测试。

## Comments

- 用户明确要求“exec tool也注册为mcp tool”，更新此前仅提供内部执行器的范围；原有文件工具仍不接入 sandbox。
- 验证：`go test ./...` 和 `git diff --check` 通过；真实 stdio `tools/list` 返回 exec，`tools/call` 成功执行 Python、读取显式只读 runtime 并在 Workspace 写入产物。
