# 03: 按 glob 模式查找工作目录文件

**What to build:** MCP 客户端按 glob 模式查找文件，默认遵循忽略规则，按稳定路径顺序分页获取结果。

**Blocked by:** 01 — 启动 MCP 服务并分页读取工作目录文件。

**Status:** ready-for-agent

- [ ] 在注册函数和 stdio 服务中提供 glob；参数为必填 pattern、path="."、include_ignored=false、offset=0、limit=200，无 cwd。
- [ ] path 必须指向工作目录内的目录；执行统一路径和分页参数校验。
- [ ] 使用参数数组调用 rg，模式以指定搜索目录为基准，只返回文件，不跟随符号链接。
- [ ] 默认遵循 .gitignore，包含隐藏文件且排除 .git；include_ignored=true 允许搜索被忽略的文件，仍排除 .git。
- [ ] items 包含相对工作目录的文件路径；按路径排序后分页，返回 truncated 和 next_offset，结束时 next_offset=null。
- [ ] 无匹配或偏移超出结果返回成功的空列表；命令失败返回 MCP 工具错误。
- [ ] 复用统一超时、取消和完整响应 1 MiB 上限；字节截断可继续分页，单条记录过大返回明确错误。
- [ ] 通过 MCP 调用验证嵌套 glob、子目录搜索、隐藏文件、忽略规则切换、链接、排序、分页和执行错误。
