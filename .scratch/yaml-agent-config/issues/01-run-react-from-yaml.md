# 01: 通过 YAML 运行无工具的 ReAct 任务

**What to build:** 用户通过显式指定 YAML 配置、任务和工作目录，运行代理类型为 ReAct 的无工具任务，获得最终答案或明确失败原因。建立能够实际运行的最小完整路径，同时保留未指定配置时的 MCP 服务行为。

**Blocked by:** None (can start immediately).

**Status:** resolved

- [x] `--config` 指定单个 Agent 的 YAML 配置；`name: react` 选择代理类型，而非实例名称。不指定配置时保持原有 MCP stdio 服务行为。
- [x] Agent 模式通过非空 `--task` 接收任务，不增加交互输入；工作目录使用 `--workspace`，工具超时沿用 `--timeout`，默认 30 秒。
- [x] YAML 支持 `name`、`llm`、`allow_tools` 和 `sys_prompt`；后两者的实际工具执行和非空自定义提示词行为分别由后续票据接通。
- [x] `name` 和 `llm.model` 必填；`llm.base_url` 默认 `https://api.openai.com/v1`；`llm.output_schema` 默认 `json_llm_output`，并可通过配置选择 `line_protocol_llm_output`，两种格式均可完成任务。
- [x] 支持 `llm.api_key` 明文密钥或 `llm.api_key_env` 指定环境变量，两者同时填写时报错；均未填写时读取 `OPENAI_API_KEY`；最终密钥为空时报错，错误信息不泄露密钥。
- [x] 配置文件不存在、YAML 格式错误、必填项缺失、未知字段、未知代理类型、未知工具名或未知输出格式均报告明确错误并返回非零退出码。
- [x] `allow_tools` 使用字符串序列并转换为工具集合；已知名称为 `read_file`、`list_dir`、`glob`、`grep`、`apply_patch`。省略或空序列均禁用全部工具。
- [x] 补齐 ReAct 初始化与内部 MCP 连接及关闭，保证无工具任务可真正调用模型并返回结果，不出现启动挂起或资源泄漏。
- [x] 最大执行轮数使用代码中的 `const` 固定为 100，不提供 YAML 或 CLI 配置项；完成后立即输出最终答案并成功退出，执行失败或耗尽轮数时报告原因并返回非零退出码。
- [x] 通过可控模型响应验证两种输出格式、成功完成、模型失败及轮数耗尽；验证配置错误与旧 MCP 入口兼容性，测试不依赖真实 API 密钥。
- [x] 保留既有用户改动，关键行为添加注释。

## Answer

已实现 YAML 配置加载、严格校验、密钥解析、Agent CLI 分支、ReAct 内部 MCP 生命周期、两种输出格式和失败退出语义。`go test ./...` 通过。
