# codex-go

## 用 YAML 配置运行 ReAct Agent

完整的新格式示例见 [`examples/react-agent.yaml`](examples/react-agent.yaml)。顶层 `name` 选择启动类型，`base` 提供共享配置，`agents` 可为 `react`、`task_analyzer` 和 `observe` 定义独立配置。CLI 当前只启动 `react`；任务分析器和观察器仍由现有流程按需调用，不增加 CLI 调度。

示例展示环境变量密钥、独立 LLM 与提示词、显式空工具列表和显式空提示词。ReAct 使用 base 中的工具列表；分析器配置中的 `allow_tools: []` 展示显式空列表，观察器配置中的 `sys_prompt: ""` 展示清除自定义提示词。

设置示例中用到的环境变量后启动：

```sh
export OPENAI_API_KEY='your-key'
export ANALYZER_API_KEY='your-analyzer-key'
go run ./cmd --config examples/react-agent.yaml \
  --task '检查项目并完成目标' \
  --workspace /path/to/workspace \
  --timeout 30s
```

也可以分别构建 MCP server 和 agent server（`-s -w` 去除符号表和调试信息，减小二进制体积）：

```sh
make build
```

`./bin/agent-server` 接受相同的 `--config`、`--task`、`--workspace` 和 `--timeout` 参数，并可用 `--mcp-command` 指定 MCP 二进制路径（默认 `codex-mcp`）。入口通过 `ProcessorOrchestrator` 从 `TaskAnalyzerAgentType` 开始执行，ReAct 节点通过 MCP stdio transport 连接子进程。单独启动 MCP stdio 服务运行 `./bin/codex-mcp --workspace /path/to/workspace --timeout 30s`。主 CLI 未指定 `--config` 时也保留原有 MCP stdio server 模式。

`agent-server` 还支持可选参数 `--mode react` 或 `--mode plan-execute`：指定后跳过 task analyzer，分别直接进入 ReAct 或 planner，并优先于 YAML 的 `name` 选择运行模式；不指定时保留原有分析流程。其他模式值会报错并以退出码 2 退出。例如：

```sh
./bin/agent-server --config examples/react-agent.yaml \
  --task '检查项目并完成目标' --workspace /path/to/workspace \
  --mode plan-execute
```

## 配置规则

- 顶层仅支持 `name`、`base`、`agents`。`name` 必填且当前只能是 `react`；`base` 必须提供完整有效的 `llm`。`agents` 可省略或为空。
- `agents` 只接受 `react`、`task_analyzer`、`observe`。每种类型分别从自身设置和 `base` 解析，不会继承其他类型的独立设置；未设置某项时逐项回退到 base。
- `llm` 是整块替换。提供独立 `llm` 后，base 中未列出的 LLM 字段不会回退；`model` 必填，`base_url` 默认 `https://api.openai.com/v1`，`output_schema` 默认 `json_llm_output`，也支持 `line_protocol_llm_output`。
- `allow_tools` 列表整体替换，不与 base 合并。省略时回退到 base，显式 `[]` 禁用工具。合法工具为 `read_file`、`list_dir`、`glob`、`grep`、`apply_patch` 和 `exec`。
- `sys_prompt` 省略时回退到 base；显式 `""` 清除自定义提示词。最终非空自定义提示词会追加在对应代理的内置提示词后。
- `task_analyzer` 和 `observe` 始终没有工具能力，不会暴露或执行工具。base 的工具列表不会授予它们权限；为其显式设置非空 `allow_tools` 会导致启动报错，省略或 `[]` 均可。
- 密钥可用 `api_key` 明文提供，或用 `api_key_env` 指定环境变量；两者互斥。两者都省略时读取 `OPENAI_API_KEY`。最终密钥不能为空，错误信息不会包含密钥值。示例只使用环境变量名，不含真实密钥。
- 所有配置字段拒绝显式 YAML `null`。启动时会校验 base 和所有独立配置，包括本次流程尚未调用的代理；文件不存在、格式错误、未知字段、类型、工具、输出格式或缺少必填项都会报错。

配置文件只允许一个 YAML 文档。旧格式不兼容，也不会自动转换。手动迁移时，将旧 YAML 的顶层 `llm`、`allow_tools` 和 `sys_prompt` 原样移入 `base`，新增 `name: react`；随后可按类型在 `agents.react`、`agents.task_analyzer` 和 `agents.observe` 下填写覆盖项。例如：

```yaml
name: react
base:
  llm:
    api_key_env: OPENAI_API_KEY
    model: your-model
  allow_tools: [read_file, list_dir]
  sys_prompt: "通用约束"
agents:
  task_analyzer:
    allow_tools: []
```

## 启动参数与结果

MCP 的 `exec` 工具调用示例：

```json
{"command":"git","args":["status","--short"]}
```

Agent 使用时需在 `allow_tools` 中加入 `exec`。入口仅允许 `find`、`git`、`grep`、`npm`、`poetry`、`python`、`python3`、`rg`、`sed`、`ts-node`、`tsx`；`args` 可省略，不接受 shell 字符串或工具级 `cwd`。命令固定在 Workspace 根目录执行，可读写整个 Workspace（包括 `.git` 和删除文件），默认禁网；stdin 为 EOF，结束或中断后清理全部子进程。

三个 CLI 入口均可重复传入 `--runtime-dir /path/to/runtime`，由宿主授权额外只读运行时目录。对于 Node prefix 布局，可分别传入 `--runtime-dir /path/to/node/bin --runtime-dir /path/to/node/lib`。运行目录加入受控 PATH；命令调用不能追加挂载或继承宿主环境。执行需要 Linux、bwrap 和 seccomp，缺少隔离能力时仅 `exec` 调用失败，文件工具仍可用。命名 Unix socket、io_uring 和非本机 ABI 不可用；此版本没有 CPU、内存、进程数或磁盘配额。

`exec` 返回 `stdout`、`stderr`、`exit_code`、`truncated`；普通非零退出返回执行结果。参数、启动、超时或取消错误设置 `isError`，并提供 `error_kind`、`error` 及已捕获输出；没有运行命令时退出码为 `-1`。两路捕获合计最多 1 MiB，完整 MCP 响应大小可能使输出进一步截断，不能续读。服务端 `--timeout` 生效，但 `exec` 最长运行 30 秒。

Agent 模式要求非空 `--task` 和 `--workspace`；`--timeout` 必须为正值，默认 30 秒，仅限制单次工具调用。现有 CLI 参数保持不变，最大执行轮数仍固定为 100，不提供 YAML 或 CLI 配置项。成功完成时输出最终答案并以 0 退出；执行失败、配置错误或达到轮数上限时报告原因并以非 0 退出。

`json_llm_output` 要求模型返回一个 JSON 值；`line_protocol_llm_output` 要求返回 Line Protocol。最终结果必须包含 `status`（`completed` 或 `failed`）和非空 `answer`；缺失或未知状态、完成状态下的空答案会报错。
