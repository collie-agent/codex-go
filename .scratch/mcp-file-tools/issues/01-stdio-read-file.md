# 01: 启动 MCP 服务并分页读取工作目录文件

**What to build:** 用户指定工作目录后可启动 stdio MCP 服务，通过 read_file 获取带行号的文件内容；嵌入方也能通过注册函数使用该工具。此任务交付首条完整可运行路径，并建立后续工具共用的工作目录、执行和响应约定。

**Blocked by:** None (can start immediately).

**Status:** ready-for-agent

- [ ] 使用 Go MCP SDK 提供注册函数和 stdio 启动入口；--workspace 必填，--timeout 默认 30s。
- [ ] 启动检查工作目录及 find、sed、rg；无效目录或缺少命令则退出，日志只写 stderr。
- [ ] read_file 接受必填 path、offset=1、limit=200；拒绝 offset<1 或 limit<1，工具不提供 cwd。
- [ ] 路径相对启动工作目录，拒绝绝对路径和越界路径，允许解析后仍在目录内的 ..；符号链接最终目标必须在工作目录内。
- [ ] 只读取 UTF-8 非二进制普通文件，通过参数数组执行 sed，不拼接 shell；支持超时和请求取消。
- [ ] 返回带行号的 lines、truncated、next_offset；超出 EOF 返回成功的空结果，结束时 next_offset=null。
- [ ] 结构化结果为唯一数据来源，可读文本从其生成；完整 MCP 工具响应 JSON 不超过 1 MiB，按完整行截断并给出下一行；单行过大返回明确错误。
- [ ] 参数和执行失败返回 MCP 工具错误；共享执行和响应约定可供后续工具复用。
- [ ] 可通过真实 stdio MCP 请求演示分页读取；验证默认值、边界路径、链接、无效文件、超时、取消和字节截断。
