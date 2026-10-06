# YAML Agent 配置

Status: needs-info

## 已确认

- 一个 YAML 文件配置一个 Agent。
- `name: react` 表示代理类型，不是实例的自定义名称。
- 使用 `--config <文件路径>` 显式加载；未指定时保留现有启动方式。
- 指定文件不存在或格式错误时直接报错。
- 配置包含 AgentConfig 的长期设置；本次任务通过启动参数或交互输入提供。

## 启动与运行

- 指定 `--config` 时运行 Agent；未指定时保留现有 MCP stdio server 模式。
- Agent 模式通过 `--task` 提供任务，第一版不增加交互输入；任务必须非空。
- 工作目录通过现有 `--workspace` 指定，工具超时使用现有 `--timeout`（默认 30 秒）；不进入 YAML。
- 最大执行轮数由代码中的 const 常量固定为 100，不进入 YAML。
- Agent 完成后输出最终答案并成功退出；执行失败或达到轮数上限时报告原因，并返回非零退出码。
- 示例：`go run ./cmd --config agent.yaml --task "任务内容" --workspace /path`。

## YAML 格式

```yaml
name: react
llm:
  base_url: https://api.openai.com/v1
  api_key_env: OPENAI_API_KEY
  model: your-model
  output_schema: json_llm_output
allow_tools:
  - read_file
  - list_dir
sys_prompt: ""
```

- 顶层 `name` 选择代理类型，第一版支持 `react`。
- `llm` 映射 AgentConfig.LLMConfig，`allow_tools` 映射 AgentConfig.AllowTools，`sys_prompt` 映射 AgentConfig.SysPrompt。
- `name` 和 `llm.model` 必填。
- `llm.base_url` 默认 `https://api.openai.com/v1`。
- `llm.output_schema` 默认 `json_llm_output`，另支持 `line_protocol_llm_output`。
- 允许 `llm.api_key` 明文或 `llm.api_key_env` 指定环境变量名；两者同时填写时报错。
- 两个密钥字段均未填写时读取 `OPENAI_API_KEY`；最终密钥为空时报错。
- 未知字段、未知代理类型、未知工具及未知输出格式均报错。
- 工具列表使用 YAML 字符串序列，加载后转换为工具集合；省略或 `[]` 均禁用所有工具。
- 已知工具：`read_file`、`list_dir`、`glob`、`grep`、`apply_patch`。
- 空 `sys_prompt` 使用内置 ReAct 提示词；非空内容追加到内置提示词后。

## 实施范围

- 增加 YAML 加载、默认值处理、字段校验和配置示例。
- 增加 Agent CLI 启动流程，按代理类型创建 ReAct 执行流程。
- 接通 AgentConfig 配置到实际运行路径，包括工具列表和系统提示词。
- 补齐 ReAct 启动、内部 MCP 连接与关闭、完成/失败判定和最终结果返回，使配置能够实际运行。
- 关键行为添加注释；验证配置错误、工具禁用、运行结束和旧 MCP 入口兼容性。

## 确认状态

- Q1–Q3：用户回复 `all confirm`。
- Q4–Q7：用户回复 `其他的all confirm`。
- Q8：采用推荐的运行输入与结果语义，最大轮数改为代码 const 固定 100。
- 设计问题已收敛，等待用户确认整体理解后进入实现。

## 代码事实

- 当前 cmd/main.go 只启动 MCP stdio server，没有 Agent CLI。
- AgentConfig 包含 LLMConfig、AllowTools、SysPrompt；LLMConfig 包含 BaseUrl、ApiKey、Model、OutputSchema。
- 当前没有按 name 选择代理类型的机制，也没有 YAML 解析依赖。
- SysPrompt 当前未接入实际模型调用。
- internal/agent/react.go 存在用户未提交改动，实施时需保留并协调。
