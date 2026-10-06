# codex-go

## 用 YAML 运行 ReAct Agent

完整示例见 [`examples/react-agent.yaml`](examples/react-agent.yaml)。它使用 `name: react`、环境变量密钥、LLM 设置、工具允许列表和 `sys_prompt`；示例中的工具实际执行依赖票据 02。

启动 Agent 模式：

```sh
export OPENAI_API_KEY='your-key'
go run ./cmd \
  --config examples/react-agent.yaml \
  --task '检查项目并完成目标' \
  --workspace /path/to/workspace \
  --timeout 30s
```

指定 `--config` 时，`--task` 必须非空，`--workspace` 必须存在，`--timeout` 必须为正值（默认 30 秒）。未指定 `--config` 时仍启动原有 MCP stdio 服务模式；该模式仍需要 `--workspace`。

YAML 字段规则：

- 必填：`name: react` 和 `llm.model`。
- `llm.base_url` 默认 `https://api.openai.com/v1`；`llm.output_schema` 默认 `json_llm_output`，也可用 `line_protocol_llm_output`。
- `llm.api_key` 与 `llm.api_key_env` 互斥。两者都省略时读取 `OPENAI_API_KEY`；最终密钥不能为空。示例使用 `api_key_env`，不包含真实密钥。
- `allow_tools` 省略或设为空列表 `[]` 时禁用全部工具；工具名称必须是 `read_file`、`list_dir`、`glob`、`grep` 或 `apply_patch`。
- `sys_prompt` 省略或为空时保留内置 ReAct prompt；非空内容追加在内置 prompt 后，不会替换 ReAct 的执行约束。

`json_llm_output` 要求模型返回一个 JSON 值；`line_protocol_llm_output` 要求返回 Line Protocol。Agent 完成时输出最终答案并以 0 退出；模型或任务失败、配置错误、请求超时，以及固定最大轮数耗尽时报告错误并以非 0 退出。最大轮数在代码中固定为 100，不提供 `max_turns` 配置项。

`--timeout` 仅限制单次工具调用，不限制整个 Agent 任务的总运行时间。每个配置文件只允许一个 YAML 文档，尾随文档会报错。最终结果必须包含 `status`（`completed` 或 `failed`）和非空 `answer`；缺失或未知状态、完成状态下的空答案会报错。
