# 08: 支持显式配置的项目运行时

**What to build:** 宿主能够配置额外的只读运行时目录，让安装在用户目录中的 Poetry、Python 和 Node 相关工具在 sandbox 内执行项目任务。命令调用方无法扩大文件可见范围，项目可在 Workspace 中产生文件并使用其中的虚拟环境。

**Blocked by:** 07：在隔离工作区执行系统命令。

Status: ready-for-agent

父任务：[06：新增基于 bwrap 的内部命令执行器](06-bwrap-command-executor.md)。依据：[完整规格](../spec.md)中的“新增命令 sandbox”及[安全边界 ADR](../../../docs/adr/0002-command-sandbox-boundary.md)。前置任务：[07](07-isolated-system-commands.md)。

- [x] 额外运行时目录仅由宿主配置；普通命令调用不能增加挂载目录或改变初始化时确定的 PATH 目录集合。
- [x] 额外目录只读挂载，其全部内容对 sandbox 内程序可读；不自动暴露整个宿主 HOME。
- [x] 配置目录经过规范化和符号链接解析，拒绝将整个宿主根目录或 HOME 作为额外运行时目录；不允许通过额外挂载绕过 Workspace 及系统运行目录的既定边界。
- [x] 入口可从必要系统目录和显式配置的运行时目录解析；不要求所有白名单工具同时安装，缺失命令仅使对应调用失败。
- [x] 子进程获得同一初始受控 PATH；允许解释器、工具启动器及项目脚本启动其他程序，仍遵守原 sandbox 的禁网、权限和生命周期约束。
- [x] Workspace 内的虚拟环境与项目程序按工作区权限访问，可由项目脚本显式运行，不因新增只读运行时挂载而意外变成只读。
- [x] 可通过代表性的 Poetry、Node/npm 入口及 Workspace Python launcher 验证运行时可访问；运行时目录内的写操作失败，Workspace 中的任务产物写入成功。
- [x] 用户级工具不能读取未配置的外部用户数据；token、SSH agent、代理和宿主配置仍不通过环境或 HOME 泄漏。
- [x] 离线可用的项目任务能够完成；需要在线下载的任务按默认禁网策略失败，不引入联网例外。
- [x] 在运行时目录验证和 PATH 构造的关键位置添加注释，说明宿主显式授权目录的可读范围。

## Comments

- 通过 `NewCommandExecutor(runtimeDirs ...string)` 配置宿主显式授权的运行时目录；构造时规范化并解析符号链接，拒绝根目录、整个 HOME、非目录、缺失目录及系统运行目录。
- 运行时目录加入固定 PATH 并以 `--ro-bind` 挂载；`Execute` 仍只接受白名单入口，调用方无法追加挂载或修改 PATH。真实 bwrap 集成测试验证了 Poetry 入口可执行、运行时目录不可写且 Workspace 可写。
- 覆盖了 `/tmp` 下 runtime（挂载顺序确保不被隔离 tmpfs 隐藏）、相对路径和安全符号链接规范化；Node/npm 任务及由显式 runtime launcher 启动的 Workspace Python 任务均可完成。
- 现有执行器隔离测试继续验证清空继承环境、禁网、隔离 HOME、外部数据不可读及 Workspace 写权限；runtime 配置不会扩大这些边界。
- 验证：`go test ./internal/mcp -count=1` 通过。
