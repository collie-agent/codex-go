# 04: 搜索工作目录文件内容

**What to build:** MCP 客户端搜索单个文件或目录中的文本，通过正则或字面模式获得包含路径、行号和匹配行文本的分页结果。

**Blocked by:** 01 — 启动 MCP 服务并分页读取工作目录文件。

**Status:** ready-for-agent

- [ ] 在注册函数和 stdio 服务中提供 grep；参数为必填 pattern、path="."、可选 glob、literal=false、ignore_case=false、include_ignored=false、offset=0、limit=200，无 cwd。
- [ ] path 可以指向工作目录内的普通文件或目录，执行统一路径和分页参数校验；不跟随符号链接。
- [ ] 使用参数数组调用 rg，默认正则搜索且区分大小写；支持字面搜索、忽略大小写和 glob 文件过滤。
- [ ] 默认遵循 .gitignore，包含隐藏文件且排除 .git；include_ignored=true 允许搜索被忽略的文件，仍排除 .git。
- [ ] items 包含相对工作目录的路径、行号和匹配行文本；按路径、行号排序后分页，返回 truncated 和 next_offset，结束时 next_offset=null。
- [ ] 无匹配或偏移超出结果返回成功的空列表；无效正则和其他执行失败返回 MCP 工具错误。
- [ ] 复用统一超时、取消和完整响应 1 MiB 上限；字节截断可继续分页，单条匹配记录过大返回明确错误。
- [ ] 通过 MCP 调用验证文件与目录搜索、各搜索选项、隐藏文件、忽略规则、链接、排序、分页、无匹配和无效正则。
