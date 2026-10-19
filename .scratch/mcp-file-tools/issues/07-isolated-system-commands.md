# 07: 在隔离工作区执行系统命令

**What to build:** 内部调用方能够通过命令名和参数数组，在独立的 bwrap sandbox 内执行已安装的系统工具，获得执行结果。整个进程树只能读写授权的工作目录（Workspace），必要系统文件只读可见，网络不可用。

**Blocked by:** None (can start immediately).

Status: ready-for-agent

父任务：[06：新增基于 bwrap 的内部命令执行器](06-bwrap-command-executor.md)。依据：[完整规格](../spec.md)中的“新增命令 sandbox”及[安全边界 ADR](../../../docs/adr/0002-command-sandbox-boundary.md)。

- [x] 提供独立内部执行能力，接受 context、Workspace、命令名和参数数组，固定在 Workspace 根目录执行；不接入现有文件工具，不新增 MCP 工具，不接受 shell 字符串或独立 cwd。
- [x] 入口名称精确限制为 find、git、grep、npm、poetry、python、python3、rg、sed、ts-node、tsx；拒绝可执行文件路径。白名单仅约束入口，允许子进程执行其他程序，整个进程树遵守同一隔离边界。
- [x] 系统命令从初始化时确定的受控 PATH 目录解析，不搜索 Workspace，不继承宿主 PATH；缺失入口在实际调用时明确报错，不要求一次安装全部入口。
- [x] Workspace 可读写，包括新增、修改、删除文件及修改 Git 元数据；外部用户文件不可见，必要系统运行文件只读，工作区外的符号链接不能扩大授权范围。
- [x] 规范化目录并解析符号链接后，拒绝根文件系统作为 Workspace，拒绝 Workspace 与系统运行目录重叠；目录校验失败时不启动命令。
- [x] 使用独立的进程、网络、IPC、主机名和用户命名空间，移除 capabilities；既不能连接外部网络，也不能连接宿主机网络服务。
- [x] 仅提供隔离的进程文件系统、最小设备集合、临时目录和必需系统配置；不泄漏宿主 socket 或额外文件描述符。
- [x] 清空继承环境，仅设置必要 PATH、语言环境及隔离 HOME、缓存目录；不继承 token、SSH agent 和代理配置。Workspace 内已有敏感文件仍属于完整读写授权。
- [x] 非交互执行，标准输入关闭并呈现 EOF；能够分别捕获标准输出、标准错误和退出码，输出捕获使用有界内存。
- [x] 默认超时 30 秒，支持 context 取消；命令正常退出、超时或取消均清理完整进程树，不允许后台子进程跨调用存活。高输出及管道持有场景的完整返回契约由 09 补齐。
- [x] 非 Linux、bwrap 缺失、隔离能力不足或 sandbox 创建失败时明确报错，绝不回退宿主执行，且不影响原有文件工具。
- [x] 可通过系统工具完成一次工作区读写调用，并验证外部文件、网络、环境变量及正常退出后的后台进程边界。
- [x] 在命令解析、挂载边界和进程清理的关键位置添加注释；明确此版本没有 CPU、内存、进程数及磁盘配额。

## Comments

- 已实现 `NewCommandExecutor()` 与 `CommandExecutor.Execute(ctx, workspace, name, args)`；执行器位于 `internal/mcp/exec.go`，未注册 MCP 工具。
- 按已确认的两个公开测试边界进行 red → green：入口拒绝、真实工作区读写、命令 stderr 与 sandbox 启动错误区分、系统目录拒绝。其余隔离边界通过真实 bwrap 集成测试验证。
- 仅使用固定 `/usr/bin:/bin` 解析入口；额外运行时目录仍由 ticket 08 承接。ticket 09 的完整高输出及中断返回契约仍单独处理。
- 默认超时 30 秒，两路输出合计最多保留 1 MiB；没有 CPU、内存、进程数或磁盘配额。
- 验证：`go test ./...` 通过；另对命令执行器运行 race 检查。
