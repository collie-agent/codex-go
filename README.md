# codex-go

Go 实现的coding agent，通过 OpenAI Responses API 进行模型决策，通过 MCP 读取、搜索和修改工作目录中的文件，并执行受隔离的项目命令。默认从 ReAct 开始，支持显式规划执行以及运行中升级为计划执行。

## 项目特性

- **ReAct 循环**：模型在每轮选择调用工具或返回结果，执行器回填工具结果，直到任务完成、失败或达到轮数上限。
- **计划执行**：planner 生成有序步骤，ReAct 逐步执行，并记录步骤状态、观察结果和完成证据。
- **多代理协作**：根据 YAML 配置图调度 supervisor 与 ReAct worker，每次委派独立上下文，业务失败回交 supervisor 决策。
- **异步观察**：对 `apply_patch` 和 `exec` 的结果提取新增事实与错误，供后续决策使用。
- **内置 MCP 工具**：提供文件读取、目录浏览、文件匹配、内容搜索、补丁写入和命令执行，可独立作为 stdio 服务使用。
- **按代理类型配置**：共享基础代理配置，可为 ReAct、观察器和 planner 设置独立模型与提示词；工具权限仅授予 ReAct。
- **结构化输出**：支持 JSON 和 Line Protocol，结合输出解析、业务校验和工具参数 schema 校验控制执行流程。
- **工作目录边界**：文件工具限制路径范围；`exec` 使用 Linux bwrap 与 seccomp 隔离整个命令进程树。

## 快速开始

### 环境准备

- Go 1.26.0 或兼容该 `go.mod` 的工具链。
- 文件工具依赖 `find`、`sed` 和 ripgrep（`rg`）；独立 MCP 服务启动时会检查这三个命令。
- Agent 运行需要已创建的 MySQL 数据库，并通过 `CODEX_MYSQL_URL` 配置连接；纯 MCP 服务不需要数据库。
- 可访问的 OpenAI Responses API 或兼容接口，以及支持所选输出格式和函数工具调用的模型。
- 使用 `exec` 时还需要 Linux、bwrap 和可用的 seccomp 隔离能力。

### 构建与运行

```sh
make build
```

构建产物为 `bin/codex-mcp`、`bin/agent-server` 和 `bin/codex-migrate`，使用 `-trimpath` 和 `-s -w` 减小二进制体积。可以用 `GOOS`、`GOARCH`、`BUILD_DIR` 覆盖构建目标和输出目录。

创建 `agent.yaml`，将 `your-model` 替换为实际模型名：

```yaml
name: react
base:
  llm:
    api_key_env: OPENAI_API_KEY
    model: your-model
    output_schema: json_llm_output
    stream: false # true 使用流式 Responses API；debug 额外打印文本片段
  allow_tools: [read_file, list_dir, glob, grep, apply_patch]
  sys_prompt: "只处理用户要求的任务，完成时说明修改结果。"
```

```sh
export OPENAI_API_KEY='your-key'
export CODEX_MYSQL_URL='mysql://user:password@127.0.0.1:3306/codex-go'
./bin/codex-migrate
./bin/agent-server --config agent.yaml \
  --task '阅读项目并说明主要模块的职责' \
  --workspace /path/to/workspace \
  --mcp-command ./bin/codex-mcp
```

普通配置默认直接进入 ReAct；`name: multi-agent` 自动选择多代理编排器。需要规划后逐步执行时，指定 `--mode plan-execute`：

```sh
./bin/agent-server --config agent.yaml \
  --task '完成目标，并按步骤验证结果' \
  --workspace /path/to/workspace \
  --mcp-command ./bin/codex-mcp \
  --mode plan-execute
```

按保存的 `run_id` 显式恢复 ReAct 或计划执行运行（不需要重新提供任务目标）：

```sh
./bin/agent-server --config agent.yaml --run-id <run_id> \
  --workspace /path/to/workspace --mcp-command ./bin/codex-mcp
```

计划执行恢复示例：首次运行时将 YAML `name` 设为 `plan-execute`，并保存 stderr 输出的 `run_id`；崩溃或失败后，用当前配置和该 ID 恢复：

```sh
./bin/agent-server --config plan-agent.yaml --task '检查项目并修复目标问题' \
  --workspace /path/to/workspace --mcp-command ./bin/codex-mcp
# 使用上次 stderr 中的 run_id；不要再传 --task。
./bin/agent-server --config plan-agent.yaml --run-id <run_id> \
  --workspace /path/to/workspace --mcp-command ./bin/codex-mcp
```

恢复会保留原目标、工作目录、模式、运行级验收标准和已有计划；completed 步骤及其观察和证据会保留，其他步骤从头执行。调用时可使用更新后的模型及工具配置。所有步骤已完成但运行尚未收尾时，直接使用最后一步保存的完整结果结束运行。已完成运行直接重放保存的结果，同一 `run_id` 同时只能由一个进程执行。

执行期间每 100ms 检查持锁连接及锁归属，单次检查超时为 1 秒；检测到失效后取消模型请求、MCP 子进程和正在执行的工具。健康检查与检查点事务串行使用同一连接，不引入租约或续租机制。

完整的配置覆盖示例见 [`examples/react-agent.yaml`](examples/react-agent.yaml)，其中 `your-model` 需要替换，并设置 `OPENAI_API_KEY`。

### Supervisor-worker 多代理

使用 [`examples/supervisor-worker.yaml`](examples/supervisor-worker.yaml)，替换模型名并设置上述数据库及 API key 环境变量后执行：

```sh
./bin/agent-server --config examples/supervisor-worker.yaml \
  --task '检查项目，分别完成实现与复核后汇总结论' \
  --workspace /path/to/workspace --mcp-command ./bin/codex-mcp
```

`multi-agent.entry` 指定入口，`agents.<name>.next_agents` 限定允许的委派目标。
节点继承 `base` 的模型和工具配置，`instruction`（或 `sys_prompt`）覆盖提示词。
每次调用的执行上下文包含 `agents` 列表，按名称排序提供所有显式配置节点的 `name` 和完整生效 `instruction`（未指定时采用继承的提示词），供模型理解角色分工；可委派范围仍由 `next_agents` 限定。
`max_rounds` 限制整个运行的 agent 调用总数，`max_parallel` 限制同时执行的 ReAct 节点数；等待子任务不占并发名额。

所有节点输出 `MultiAgentResult`：`result` 为 ReAct 结果，`next` 为委派列表，每项包含 `agent`、`task`、`completion_criteria` 和 `context`。
每次委派生成独立任务 ID，保存 worker 名称、目标、验收条件、相关信息、结果及失败信息。
worker 完成后以空 `next` 返回，或仅指定调用者作为返回目标；supervisor 收到按委派顺序排列的 `reports` 后再次决策。
worker 的 `failed` 作为结果返回，失败输出中的 `next` 不执行；取消、模型请求错误或检查点错误会终止运行。

各委派隔离模型历史，但仍使用相同工作目录，supervisor 应避免并发修改同一文件。
使用 `--run-id` 恢复时保持原运行身份和目标，从入口重新执行；不恢复中途委派或工具历史。
已完成运行直接返回保存的完整结果。命令行打印最终 `answer`，数据库保留完整 `MultiAgentResult`。

### 入口对照

| 入口 | 用途与执行方式 |
| --- | --- |
| `./bin/agent-server --config agent.yaml --task '目标' --workspace /path/to/workspace --mcp-command ./bin/codex-mcp` | 通过编排器执行，ReAct 通过 MCP stdio 连接独立子进程；支持 ReAct、计划执行和多代理 |
| `./bin/codex-mcp --workspace /path/to/workspace --timeout 30s` | 单独提供 MCP stdio 工具服务，供 MCP 客户端连接 |

`agent-server` 是命令行任务执行入口，通过 stdio 连接 MCP 子进程，当前不提供 HTTP 服务。`--mcp-command` 默认是 `codex-mcp`，需能从 PATH 找到；使用构建目录时建议像示例一样显式传入路径。

## 运行机制与执行流程

### 编排与模式选择

`ProcessorOrchestrator` 从当前节点返回的后继类型调度下一批处理器。同批分支并发执行，配置和状态按分支复制，批次结束后按调度顺序合并结果；任一节点失败会等待本批节点退出后返回错误。当前内置任务流程主要按顺序执行。

| agent-server 配置 | 起点与后续流程 |
| --- | --- |
| `name: react`，不传 `--mode` | ReAct → end |
| `name: plan-execute`，不传 `--mode` | ReAct → end |
| `name: multi-agent`，不传 `--mode` 或指定 `--mode multi-agent` | 按配置图委派，结果返回调用者，入口输出最终结果 |
| `--mode react` | 直接 ReAct → end，由显式模式指定 |
| `--mode plan-execute` | 直接 planner → ReAct 执行各步骤 → end，由显式模式指定 |

未传 `--mode` 时固定以 ReAct 为入口；`name` 不再决定执行模式。恢复运行时沿用已保存的模式。

```mermaid
flowchart TD
    A[agent-server 加载配置与任务] --> B{显式指定 mode?}
    B -->|未传| R[ReAct 执行]
    B -->|react| R[ReAct 执行]
    B -->|plan-execute| P[planner]
    P --> E{存在待执行步骤?}
    E -->|是| S[标记 running，ReAct 执行当前步骤]
    S --> T[记录 completed、Observation、CompletionEvidence]
    T --> P
    E -->|否| F[结束并输出结果]
    R -->|完成| F
    R -->|switch_to_plan_execute| P
    R -->|失败| X[报告错误并退出]
    S -->|失败，标记 failed| X
```

普通 ReAct 由编排器执行时，会自动获得 `switch_to_plan_execute` 控制工具，无需在 `allow_tools` 中配置。模型判断剩余工作需要多个相互依赖、可独立验收的阶段时，可单独调用该工具，提供 `reason` 和 `summary`（已有事实、已完成操作和剩余工作）。编排器保存交接上下文后进入 planner；计划步骤内不开放该工具，不能再次升级。同批调用其他工具会被拒绝。直接运行 `ReactProcess` 的入口不提供此工具。

切换后检查点保存 `plan-execute` 模式及交接上下文，恢复时继续规划或执行剩余步骤。已有数据库需重新执行迁移命令，新增 `agent_run.planning_context` 列。

### ReAct 单轮循环

普通 single-agent ReAct 在缺少运行级标准时，自动获得一次性的 `update_completion_criteria` MCP 工具，无需在 `allow_tools` 中配置。模型可以先调用工具收集必要上下文，再结合原始 `Goal` 生成可验证的标准，单独调用该工具，例如 `{"completion_criteria":["根据实际源码说明入口与调用关系"]}`；工具调用无需附带文本元数据。执行器验证非空列表及非空白条目，将标准写入 context 并保存 checkpoint。参数无效时保留工具及错误结果，供下一轮重试；保存失败则停止运行，不发布新标准。

更新成功后，后续模型请求移除该工具、相关调用历史及生成提示词，仅保留 context 中的 `CompletionCriteria`。已有标准的运行、恢复运行和计划步骤不提供此工具；升级到计划执行沿用运行级标准，步骤标准仍由 planner 生成。运行级标准只能通过该工具生成并更新；最终结果及工具调用附带的响应文本均不能写入标准。执行器允许在提交标准前调用其他工具收集上下文，但未成功提交标准时拒绝返回最终结果。

1. 从目标、完成标准、已确认事实、已报告错误和已有工具结果组装上下文；计划执行时增加整体计划、当前步骤与步骤验收标准。
2. 从 MCP 获取工具定义，仅把 `allow_tools` 允许的工具传给模型，通过 Responses API 请求下一次决策。
3. 模型返回工具调用时，检查允许列表和输入 schema，再并发执行本轮工具。参数校验或工具执行错误会作为工具结果回填，供模型调整下一轮操作。
4. 回填原生函数调用及其结果，保留调用 ID 的对应关系。`apply_patch`、`exec` 还会异步触发观察器，提取新增的 `ConfirmedFacts` 和 `ReportedErrors`。
5. 没有工具调用时，解析并校验结构化结果；`completed` 且答案非空才成功，`failed`、非法状态或轮数耗尽均返回错误。

```mermaid
flowchart LR
    C[组装上下文] --> L[Responses API 模型决策]
    L --> D{返回工具调用?}
    D -->|是| V[校验参数与权限]
    V --> T[并发执行工具并回填结果]
    T --> C
    T -. apply_patch / exec .-> O[异步观察器]
    O -. 新增事实与错误 .-> C
    D -->|否| R[解析并校验结果]
    R -->|完成| F
    R -->|switch_to_plan_execute| P[完成或失败]
```

观察器使用工具结果和上下文的独立快照，不阻塞下一轮；因此观察结论可能在后续轮次才可见，原始工具结果仍直接回传模型。`list_dir`、`glob` 的旧结果会在后续工具结果写入时清除；`read_file`、`grep` 等其他结果会继续保留。

### 计划与状态

planner 首次调用模型生成至少一个有序步骤，每步包含：

| 字段 | 含义 |
| --- | --- |
| `StepId`、`Revision` | 计划内唯一标识、初始版本号（固定为 1） |
| `Intent` | 当前步骤的目标和预期结果 |
| `CompletionCriteria` | 当前步骤可验证的完成标准 |
| `Status` | `pending` → `running` → `completed`，失败时为 `failed` |
| `Observation` | 执行后记录的实际观察结果 |
| `CompletionEvidence` | 执行后记录的验收证据 |

初始步骤必须为 `pending`，观察与证据列表为空。编排器选中步骤后标记 `running`，由 ReAct 执行；成功时必须提供非空的观察与完成证据列表，随后标记 `completed` 并选择下一个待执行步骤。失败会保留失败状态并终止流程。

当前 planner 在已有计划上只选择待执行步骤，不重新调用模型改写计划。后续步骤读取此前步骤的结构化记录，每一步使用新的 ReAct 上下文。全部步骤结束后，编排器返回终止分支上最后一次 ReAct 的结果；当前没有额外的最终汇总模型调用。

`DurableState` 在 MySQL 中以运行和步骤检查点持久化。恢复时按步骤 `position` 重建顺序，跳过已完成步骤；当前步骤索引由有序步骤推导，不单独持久化。

## 配置规则

- 顶层支持 `name`、`log_level`、`base`、`agents` 和 `multi-agent`。`name` 必填，支持 `react`、`plan-execute` 和 `multi-agent`；`base` 必须提供完整有效的 `llm`。多代理模式必须提供 `multi-agent` 执行限制及入口节点，其他模式的 `agents` 可省略或为空。
- `log_level` 可设为 `debug`、`info`（默认）或 `error`，对所有代理生效。`info` 显示 LLM 上下文、JSON schema 和工具调用结果；`error` 隐藏这些详情。所有级别都显示工具调用名称、调用 ID 和完整参数，以及 `LLM response text`，包括工具调用轮次中的非空响应文本。`debug` 包含 `info` 日志，并在流式返回期间实时打印 `LLM response text delta` 片段；完成后仍打印完整文本。编排器不输出分析器模式日志。
- 普通模式的 `agents` 只接受 `react`、`observe`、`plan`；多代理模式接受自定义节点名称（保留 `base`），`next_agents` 必须引用已配置节点。每个节点分别从自身设置和 `base` 解析，不会继承其他节点的独立设置；未设置某项时逐项回退到 base。
- 工具调用失败时，两种日志级别都打印 `[react] tool calling error`，包括执行错误、参数校验错误及调用被拒绝的错误；成功结果详情仍仅在 `info` 级别显示。
- `llm.stream` 默认 `false`；设为 `true` 使用流式返回。流式完成后统一解析结果和工具调用，在 `info/error` 级别仅打印完整响应文本，`debug` 额外打印流式文本片段。
- `llm` 是整块替换。提供独立 `llm` 后，base 中未列出的 LLM 字段不会回退；`model` 必填，`base_url` 默认 `https://api.openai.com/v1`，`output_schema` 默认 `json_llm_output`，也支持 `line_protocol_llm_output`。
- `allow_tools` 列表整体替换，不与 base 合并。省略时回退到 base，显式 `[]` 禁用工具。合法工具为 `read_file`、`list_dir`、`glob`、`grep`、`apply_patch` 和 `exec`。
- `sys_prompt` 省略时回退到 base；显式 `""` 清除自定义提示词。`instruction` 是其别名，二者不可同时提供。最终非空自定义提示词会追加在对应代理的内置提示词后。
- 普通模式的 `observe` 和 `plan` 没有工具能力，base 的工具列表不会授予它们权限；为其显式设置非空 `allow_tools` 会导致启动报错，省略或 `[]` 均可。多代理配置中的显式节点均由 ReAct 执行，按各自的工具列表授权。
- 密钥可用 `api_key` 明文提供，或用 `api_key_env` 指定环境变量；两者互斥。两者都省略时读取 `OPENAI_API_KEY`。最终密钥不能为空，错误信息不会包含密钥值。示例只使用环境变量名，不含真实密钥。
- 所有配置字段拒绝显式 YAML `null`。启动时会校验 base 和所有独立配置，包括本次流程尚未调用的代理；文件不存在、格式错误、未知字段、类型、工具、输出格式或缺少必填项都会报错。

配置文件只允许一个 YAML 文档。旧格式不兼容，也不会自动转换。手动迁移时，将旧 YAML 的顶层 `llm`、`allow_tools` 和 `sys_prompt` 原样移入 `base`，新增 `name: react`；随后可按类型在 `agents.react`、`agents.observe` 和 `agents.plan` 下填写覆盖项。例如：

```yaml
name: react
base:
  llm:
    api_key_env: OPENAI_API_KEY
    model: your-model
  allow_tools: [read_file, list_dir]
  sys_prompt: "通用约束"
agents:
  observe:
    sys_prompt: "观察约束"
```

## MCP 工具与权限

| 工具 | 能力与关键约定 |
| --- | --- |
| `read_file` | 分页读取 UTF-8 文本，返回原始行号；`offset` 从 1 开始，默认每页 200 行 |
| `list_dir` | 按深度浏览目录，返回排序后的工作目录相对路径与类型；默认深度为 1 |
| `glob` | 用 ripgrep glob 匹配文件路径，支持忽略文件规则和分页 |
| `grep` | 按正则或字面文本搜索内容，返回路径、行号和文本，支持大小写与文件 glob 过滤 |
| `apply_patch` | 用 unified diff 新建、修改或删除 UTF-8 文件；支持 `/dev/null`，不支持重命名或二进制文件 |
| `exec` | 在工作目录根目录执行入口白名单中的命令，返回输出和退出状态 |

文件工具要求工作目录相对路径，拒绝绝对路径和越界路径。`read_file`、`list_dir` 允许解析到工作目录内部的符号链接；`glob`、`grep`、`apply_patch` 拒绝路径中的符号链接。目录浏览与搜索排除 `.git`，搜索默认遵守忽略文件规则。

分页结果通过 `truncated` 和 `next_offset` 指示是否需要续读；除 `read_file` 外，浏览与搜索的 `offset` 从 0 开始。工具响应受 1 MiB 大小限制，实际单页可能少于请求条数。

`apply_patch` 接收带 `---`、`+++` 和 `@@` 的 unified diff，不接受 `*** Begin Patch` 格式。补丁按精确文本、忽略行尾空白、忽略首尾空白、有限标点归一化依次尝试匹配，行号不要求精确。所有修改在写入前完成准备与校验，多文件写入不是事务：写入失败时不回滚已完成修改，错误结果通过 `completed`、`failed` 报告范围。设计说明见 [补丁匹配 ADR](docs/adr/0001-preserve-reference-patch-matching.md)。

MCP 服务始终注册全部内置工具，`allow_tools` 由 Agent 调用侧控制，不是独立 MCP 服务的工具访问控制配置。

### exec 命令隔离

Agent 使用时需在 `allow_tools` 中加入 `exec`，示例配置默认未启用。调用示例：

```json
{"command":"git","args":["status","--short"]}
```

入口仅允许 `find`、`git`、`grep`、`npm`、`poetry`、`python`、`python3`、`rg`、`sed`、`ts-node`、`tsx`；`args` 可省略，不接受 shell 字符串或工具级 `cwd`。白名单只限制入口程序，解释器和项目脚本可以启动子进程，整个进程树受同一隔离边界约束。

命令固定在 Workspace 根目录执行，可读写整个 Workspace（包括 `.git` 和删除文件），默认禁网；系统运行文件只读挂载，临时目录独立，不继承宿主凭据环境或 HOME。stdin 为 EOF，结束、超时或取消后清理全部子进程，不保留后台任务。

`agent-server` 和 `codex-mcp` 均可重复传入 `--runtime-dir /path/to/runtime`，由宿主授权额外只读运行时目录。对于 Node prefix 布局，可分别传入 `--runtime-dir /path/to/node/bin --runtime-dir /path/to/node/lib`。运行目录加入受控 PATH；命令调用不能追加挂载或继承宿主环境。

执行需要 Linux、bwrap 和 seccomp，缺少隔离能力时仅 `exec` 调用失败，文件工具仍可用，不回退到宿主执行。命名 Unix socket、io_uring 和非本机 ABI 不可用；此版本没有 CPU、内存、进程数或磁盘配额。设计说明见 [命令隔离 ADR](docs/adr/0002-command-sandbox-boundary.md)。

`exec` 返回 `stdout`、`stderr`、`exit_code`、`truncated`；普通非零退出返回执行结果。参数、启动、超时或取消错误设置 `isError`，并提供 `error_kind`、`error` 及已捕获输出；没有运行命令时退出码为 `-1`。两路捕获合计最多 1 MiB，完整 MCP 响应大小可能使输出进一步截断，不能续读。服务端 `--timeout` 生效，但 `exec` 最长运行 30 秒。

## 启动参数、输出与限制

| 参数 | 说明 |
| --- | --- |
| `--workspace` | 新建任务必填；恢复时可省略并使用原运行保存的工作目录 |
| `--config` | YAML 配置路径；`agent-server` 必填 |
| `--conv-id` | 可选，关联已有会话；省略时创建新会话，每个新任务均创建独立 run_id |
| `--task` | Agent 模式必填，非空任务目标 |
| `--run-id` | 显式恢复已有运行；与 `--task` 互斥，必须同时提供 `--config` |
| `--timeout` | 正数，默认 `30s`，仅限制单次工具调用，不是整个任务或模型请求的超时 |
| `--runtime-dir` | 可重复，额外只读命令运行时目录 |
| `--mcp-command` | 仅 `agent-server`，MCP 可执行文件路径，默认 `codex-mcp` |
| `--mode` | 仅 `agent-server`，可选 `react`、`plan-execute` 或 `multi-agent`；多代理 YAML 默认使用 `multi-agent`，其他配置默认 `react` |

每次 ReAct 运行最多 100 轮，计划执行时每一步各自拥有此上限，当前不提供 YAML 或 CLI 配置项。ReAct 和观察器遇到输出解析错误时，每次运行累计最多纠正 3 次，第 4 次格式错误终止；ReAct 的格式纠错留在当前轮内。模型请求失败和业务校验失败直接返回错误。

`json_llm_output` 要求模型返回一个 JSON 值；`line_protocol_llm_output` 要求返回 Line Protocol。普通 ReAct 的 JSON 结果形如：

```json
{"status":"completed","answer":"任务已完成。","Observation":[],"CompletionEvidence":[]}
```

最终结果必须包含 `status`（`completed` 或 `failed`）和非空的完成答案；计划步骤成功时还必须提供非空的 `Observation` 与 `CompletionEvidence`。CLI 成功完成以 0 退出并打印答案，配置错误、执行失败或达到轮数上限时报告原因并以非 0 退出；`agent-server` 参数缺失或非法 `--mode` 以 2 退出。

stdout 只输出保存成功后的最终答案；stderr 输出运行 ID、错误和模型/工具诊断信息。LLM 请求上下文日志以 YAML 展示并省略工具定义。

## 运行持久化与迁移

`agent-server` 必须配置 `CODEX_MYSQL_URL`，格式为
`mysql://<user>:<password>@127.0.0.1:3306/<database>`。数据库名使用 `codex-go`，需预先创建；用户名、密码中的特殊字符需要 URL 编码。缺少配置、连接失败或未执行迁移时直接报错。

```sh
# 独立迁移，可重复执行并保留已有数据；普通 Agent 运行不会自动迁移。
./bin/codex-migrate
```

迁移创建 `conversation`、`agent_run` 与 `step_execution`。主键分别为 `conv_id`、`run_id` 和 `(run_id, step_id)`；运行及步骤外键分别引用其归属记录。时间列严格使用 `create_at`、`update_at`，不包含软删除字段。

新任务先保存 pending run，再向 stderr 输出 `run_id=<id> conv_id=<id>`，随后进入 running。成功保存 completed 和完整 JSON 结果（包含 Answer、Observation、CompletionEvidence）；失败保存 failed 和错误。运行目标、绝对工作目录、已确定模式和运行级验收标准也会保存。checkpoint 写入失败立即停止后续调度，不输出未保存的最终答案。

`agent-server` 可以用 `--conv-id <已有 conv_id>` 关联会话。会话仅表示归属，不共享消息历史。普通 ReAct 不创建执行步骤记录。使用 `--run-id` 可显式恢复 pending、running 或 failed 运行；进程启动不会自动恢复遗留运行。运行锁由 MySQL 专用连接持有，崩溃后连接断开即可重新接管。

计划模式会在首个执行步骤前保存完整有序计划；步骤按 `position` 恢复，而非按时间戳或 `step_id` 排序。每步进入模型前保存 `running`，完成或失败时将步骤状态、运行检查点和结构化结果在同一事务中提交。检查点写入失败会中止调度；恢复时跳过 completed 步骤，并从头执行其他步骤。

连接池上限为 10、最大空闲连接数为 5、连接最大生命周期为 3 分钟，默认连接超时为 5 秒。默认使用 `utf8mb4` 并启用时间戳解析；URL query 可传入 `timeout`、`tls`、`loc` 等 MySQL 驱动参数。凭据和整份 YAML 配置不会保存到运行记录。

### 持久化集成测试

测试会自动迁移表并写入测试数据，请使用独立测试库。以下以已有账号 `'codex'@'%'` 和测试库 `codex_test` 为例，由 MySQL 管理员执行（按实际账号及 Host 调整）：

```sql
CREATE DATABASE IF NOT EXISTS codex_test CHARACTER SET utf8mb4;
GRANT ALL PRIVILEGES ON `codex\_test`.* TO 'codex'@'%';

-- 测试会创建并删除带动态后缀的临时库；\_ 匹配字面下划线，% 匹配后缀。
GRANT ALL PRIVILEGES ON `codex\_migration\_%`.* TO 'codex'@'%';
GRANT ALL PRIVILEGES ON `checkpoint\_unmigrated\_%`.* TO 'codex'@'%';
SHOW GRANTS FOR 'codex'@'%';
```

上述库名通配符授权要求 `partial_revokes` 未启用。授权立即生效，无需执行 `FLUSH PRIVILEGES`。

故障注入测试需要创建 trigger。若 MySQL 开启 binlog，创建 trigger 可能报 `1419`；由管理员在测试实例上执行：

```sql
-- 记录原值；此配置影响整个实例，放宽创建存储函数和 trigger 时的 binlog 限制。
SHOW GLOBAL VARIABLES LIKE 'log_bin_trust_function_creators';
SET GLOBAL log_bin_trust_function_creators = 1;
```

`SET GLOBAL` 立即生效，重启后失效；账号仍需具备 `TRIGGER` 权限，上面的测试库授权已包含该权限。若仅为本次测试开启，测试后将该变量恢复为原值。

将 `CODEX_TEST_MYSQL_URL` 指向测试库后运行。两个测试包会在同一组表上创建故障注入 trigger，需使用 `-p 1` 串行运行，避免相互干扰；`-count=1` 禁用测试结果缓存：

```sh
export CODEX_TEST_MYSQL_URL='mysql://codex:password@127.0.0.1:3306/codex_test'
go test -p 1 -count=1 -v ./internal/database ./integration

# 或运行全部测试。
go test -p 1 -count=1 ./...
```

如果现有 `CODEX_MYSQL_URL` 已指向可用于测试的数据库，也可以直接复用：

```sh
CODEX_TEST_MYSQL_URL="$CODEX_MYSQL_URL" \
  go test -p 1 -count=1 -v ./internal/database ./integration
```

持久化测试读取 `CODEX_TEST_MYSQL_URL`，命令集成测试会将它作为子进程的 `CODEX_MYSQL_URL`；未配置时这些测试会跳过。`TestMySQLIntegration` 单独读取 `CODEX_MYSQL_URL`，仅执行 `SELECT 1` 连接检查。测试会清理临时库和 trigger，但写入测试库的运行记录会保留。

常见权限错误：`1044` 且库名以 `codex_migration_` 或 `checkpoint_unmigrated_` 开头，检查临时库授权；`1419` 则检查 `log_bin_trust_function_creators`。配置完成后重跑上述命令。

## 代码结构

```text
cmd/main.go           源码入口：直接 ReAct 或 MCP stdio 服务，不纳入 make build
cmd/agent-server/     编排任务的 CLI，连接独立 MCP 子进程
cmd/mcp/              独立 MCP stdio 服务
cmd/migrate/          显式创建/升级持久化表
internal/config/      YAML 校验、基础配置与独立配置解析
internal/database/    MySQL 连接、显式迁移与运行存取
internal/application/ 两个 Agent 入口共享的持久化运行生命周期
internal/agent/       编排器、ReAct、planner、观察器与 worker
internal/context/     运行状态、步骤记录、上下文格式与提示词
internal/provider/    Responses API、输出 schema、协议解析与请求日志
internal/mcp/         文件工具、路径校验、补丁处理与命令隔离
examples/             YAML 配置示例
docs/adr/             架构决策
```

领域术语见 [GLOSSARY.md](GLOSSARY.md)。本地议题以 Markdown 记录在 `.scratch/<feature>/`，规则见 [议题跟踪说明](docs/agents/issue-tracker.md)。
