# codex-go

[效果](https://theviper.world/posts/codex-go/)

一个基于 Go 实现的 coding agent，支持代码读写、命令执行、计划执行与多代理协作，可在本地和服务端运行任务。

## Agent runtime 组件

### Agent Loop / Orchestrator（核心调度器）

核心采用“模型决策 → 执行工具 → 回填结果 → 再次决策”的 ReAct 循环。模型只提出动作，执行器负责校验、执行和状态推进；无工具调用时解析结构化结果，以 `completed` 或 `failed` 收尾。每个 ReAct 执行实例最多运行 100 轮。

`ProcessorOrchestrator` 按处理器返回的后继类型调度，支持直接 ReAct、planner 生成有序计划后由 ReAct 逐步执行，以及运行中通过 `switch_to_plan_execute` 升级为计划执行。同批分支可并发执行，结束后按调度顺序合并；计划步骤顺序推进，保存观察和完成证据。

Multi-agent 模式由 `MultiAgentProcessorOrchestrator` 调度，每个代理复用 `MultiAgentReactProcess` 执行自己的 ReAct 循环。YAML 中通过 `name: multi-agent` 启用，以 `multi-agent.entry` 指定入口，在 `agents` 中定义各代理的职责、模型、工具权限和 `next_agents`，组成允许委派的有向图。可配置 supervisor/worker 分工，也支持 worker 继续向其他代理委派；模型只能选择当前代理的 `next_agents` 中的目标。

代理通过结构化结果 `MultiAgentResult.next` 提出委派，每个新子任务携带目标、完成标准和相关上下文。调度器并发执行同批子任务，等待整批结束后按委派顺序收集报告，再交回调用者决定继续委派或汇总答案。子代理以空 `next` 结束时自动回报调用者，入口代理结束时返回最终结果；子任务的业务失败作为报告交回调用者处理，执行错误则中止运行。`max_rounds` 限制整条执行链的代理调用总数，`max_parallel` 限制同时执行的代理调用数，与单个 ReAct 实例的 100 轮上限分别计数。

各次委派拥有独立的执行状态与工具历史，通过显式上下文和带来源的报告协作，同时共享工作目录。调度器将委派关系、每轮状态和子任务结果保存为检查点，恢复时复用已完成结果，继续未完成的委派。

实现入口：[`internal/agent/orchestrator.go`](internal/agent/orchestrator.go)、[`react.go`](internal/agent/react.go)、[`planner.go`](internal/agent/planner.go)、[`multi_agent.go`](internal/agent/multi_agent.go)、[`multi_agent_react.go`](internal/agent/multi_agent_react.go)、[`internal/config/agent.go`](internal/config/agent.go)。

### Model Runtime / LLM Adapter（模型层）

模型层通过 OpenAI Go SDK 对接 Responses API 及兼容接口，将与 provider 无关的 `ModelRequest` 转换为模型请求，并将 MCP 工具定义转换为原生函数工具。工具调用与返回按调用 ID 配对，供后续请求重放。流式与非流式请求共享输出处理逻辑；流式正文可实时输出，工具调用在响应完成后统一处理。

模型输出支持 JSON 和 Line Protocol，分别进行协议解析及业务校验。ReAct 和观察器的格式解析错误可累计纠正 3 次，第 4 次终止；模型请求错误和业务校验失败直接返回。基础代理配置和代理独立配置决定模型、接口地址、提示词及输出格式，目前没有多厂商专用适配层。

实现入口：[`internal/provider/responses.go`](internal/provider/responses.go)、[`parser.go`](internal/provider/parser.go)、[`internal/config/agent.go`](internal/config/agent.go)。

### Context & State（上下文与状态）

`ReactContext` 组装系统提示词、会话历史、目标、完成标准、观察、原生工具消息与持久状态；`ModelRequest` 提供请求快照。`DurableState` 表达运行身份、计划步骤和当前进度；计划步骤使用 `pending → running → completed/failed` 状态流转。不同步骤和独立委派拥有各自的执行实例，恢复时延续原实例。

观察区分已确认事实 `ConfirmedFacts`、已报告错误 `ReportedErrors` 和待验证推断 `ModelInferences`。`apply_patch` 与 `exec` 的结果异步交给观察器分析，不阻塞下一轮；原始工具结果仍直接返回模型。运行级完成标准可通过一次性的 `update_completion_criteria` 设置，计划步骤另有自己的验收标准。

实现入口：[`internal/context/context.go`](internal/context/context.go)、[`observation.go`](internal/context/observation.go)、[`internal/agent/observe.go`](internal/agent/observe.go)。

### Tool Runtime（工具执行层）

ReAct 通过 MCP stdio 连接独立工具进程，从服务获取工具定义，并根据 `allow_tools` 筛选模型可用能力。执行前再次检查权限和输入 schema，同轮独立调用并发执行；参数或工具错误作为工具结果回填，让模型根据实际错误调整操作。

内置 `read_file`、`list_dir`、`glob`、`grep`、`apply_patch` 和 `exec`，分别负责读取、浏览、匹配、搜索、补丁修改及命令执行。另有技能加载工具 `load_skill`，以及由执行流程开放的完成标准和模式切换控制工具。启动时扫描 `~/.agents/skills` 的技能名称与描述，正文按需加载到工具历史中。文件工具的路径边界与命令沙箱分别实施。

实现入口：[`internal/agent/base_react.go`](internal/agent/base_react.go)、[`tool_validation.go`](internal/agent/tool_validation.go)、[`skills.go`](internal/agent/skills.go)、[`internal/mcp/`](internal/mcp/)。

### Memory（记忆系统）

当前记忆由执行实例内的工具交互、结构化观察、计划步骤记录和同一会话的历史运行组成。新建或恢复运行时，应用层按运行顺序加载此前记录：已完成运行提供目标与结果，失败或主动中断的运行还提供执行进度、工具交互及错误。历史仅作为当前任务的参考，不恢复旧任务的调度状态，也不代表当前任务已经完成。

工具历史保留调用参数、结果及错误；普通执行过程中，后续工具结果写入时会清理旧的 `list_dir`、`glob` 发现结果。检查点恢复则重放已确认交互，保留恢复依据。当前没有独立的向量数据库、语义检索或自动提炼跨会话长期记忆；技能文件提供显式知识和流程复用。

实现入口：[`internal/application/history.go`](internal/application/history.go)、[`run.go`](internal/application/run.go)、[`internal/context/history.go`](internal/context/history.go)。

### Execution Environment / Sandbox（执行环境）

文件工具以 Workspace 为路径边界，校验相对路径和符号链接，拒绝越界访问。`exec` 在 Linux 上使用 bwrap 与 seccomp 隔离整个命令进程树：工作目录可读写，必要运行时目录只读挂载，临时目录独立，默认禁网，不继承宿主 HOME 或凭据环境。入口命令受白名单约束，允许的构建工具仍可启动项目脚本及子进程。

seccomp 进一步限制命名 Unix socket、io_uring 和非本机 ABI；隔离不可用时拒绝执行，不回退宿主。命令退出、超时或取消时清理整个沙箱。当前限制执行时间和输出量，`exec` 最长 30 秒、两路输出合计最多 1 MiB；没有 CPU、内存、进程数或磁盘配额，也不会自动撤销工作目录中的文件修改。

实现入口：[`internal/mcp/exec_runtime.go`](internal/mcp/exec_runtime.go)、[`exec_seccomp_linux.go`](internal/mcp/exec_seccomp_linux.go)、[命令隔离 ADR](docs/adr/0002-command-sandbox-boundary.md)。

### Persistence & Recovery（持久化与恢复）

MySQL 保存会话、代理运行、计划步骤、执行实例及工具执行记录，迁移由 `codex-migrate` 显式执行。工具执行前先事务登记整批调用，再逐项保存结果；运行检查点保存目标、模式、完成标准、计划和委派进度。保存失败或运行锁失效会停止后续调度，最终答案在保存成功后才输出。

通过 `--run-id` 显式恢复原运行，重放已确认的工具历史、跳过已完成步骤、恢复委派进度；已完成运行直接返回保存结果。若调用已登记但结果缺失，则标记为工具执行结果未知并停止自动恢复，避免重复副作用。这是检查点与确认记录机制，不保证外部工具副作用恰好执行一次。

同一运行通过 MySQL 专用连接锁保证单进程执行，并检查连接与锁归属。进程崩溃后可显式接管；用户主动中断会保存终态，后续输入创建新运行并参考旧历史。凭据和完整 YAML 配置不写入运行记录，恢复使用当前模型与工具配置。

实现入口：[`internal/application/resume.go`](internal/application/resume.go)、[`internal/database/runs.go`](internal/database/runs.go)、[`executions.go`](internal/database/executions.go)、[恢复边界 ADR](docs/adr/0003-resume-at-execution-boundaries.md)。

## 快速开始

以下以 `codex-cli` 为例，从构建、配置模型和初始化数据库开始，运行一个读取项目并说明模块职责的任务。`codex-cli` 会启动独立的 `codex-mcp` 子进程来执行工具调用。

### 环境准备

- Go 1.26.0 或兼容该 `go.mod` 的工具链。
- 文件工具依赖 `find`、`sed` 和 ripgrep（`rg`）；独立 MCP 服务启动时会检查这三个命令。
- Agent 运行需要已创建的 MySQL 数据库，并通过 `CODEX_MYSQL_URL` 配置连接；纯 MCP 服务不需要数据库。
- 可访问的 OpenAI Responses API 或兼容接口，以及支持所选输出格式和函数工具调用的模型。
- 使用 `exec` 时还需要 Linux、bwrap 和可用的 seccomp 隔离能力。

### 构建并运行 codex-cli

```sh
make build
```

构建产物为 `bin/codex-mcp`、`bin/codex-cli`、`bin/codex-consumer`、`bin/codex-server` 和 `bin/codex-migrate`，使用 `-trimpath` 和 `-s -w` 减小二进制体积。可以用 `GOOS`、`GOARCH`、`BUILD_DIR` 覆盖构建目标和输出目录。

创建 `agent.yaml`，将 `your-model` 替换为实际模型名；使用兼容接口时，将 `base_url` 替换为对应的 Responses API 基础地址：

```yaml
name: react
log_level: info # debug 额外打印流式文本片段；error 减少诊断详情
# 可选：启用 Redis Stream 输出时取消注释，并填写实际连接信息。
# redis:
#   addr: 127.0.0.1:6379
#   password_env: CODEX_REDIS_PASSWORD
#   db: 0
base:
  llm:
    base_url: https://api.openai.com/v1
    api_key: OPENAI_API_KEY
    model: your-model
    output_schema: json_llm_output
    stream: false # true 使用流式 Responses API
  allow_tools:
    - read_file
    - list_dir
    - glob
    - grep
    - apply_patch
  sys_prompt: |
    只处理用户要求的任务，只修改工作目录中的目标文件，完成时简要说明结果和验证情况。
agents:
  observe:
    # 清除继承的任务执行提示词，只保留观察器内置提示词。
    sys_prompt: ""
```

`agents.observe` 继承 `base.llm`，不具备工具调用能力。Redis 输出默认未启用；需要时按 [Redis 输出](docs/redis-output.md) 配置。

```sh
export OPENAI_API_KEY='your-key'
export CODEX_MYSQL_URL='mysql://user:password@127.0.0.1:3306/codex-go'
./bin/codex-migrate
./bin/codex-cli --config agent.yaml \
  --task '阅读项目并说明主要模块的职责' \
  --workspace /path/to/workspace \
  --mcp-command ./bin/codex-mcp
```

## 架构

下面以 `codex-cli` 入口展示运行时架构。实线表示主要调用或数据访问关系，虚线表示异步观察或可选输出。

```mermaid
flowchart TD
    CLI["codex-cli 命令行入口"] --> APP["Application 运行生命周期与恢复"]
    YAML["agent.yaml 模型、工具与代理配置"] --> APP
    APP --> ORCH["ProcessorOrchestrator 核心调度器"]
    ORCH --> PLAN["Planner 生成有序计划"]
    ORCH --> REACT["ReAct 模型与工具循环"]
    APP --> MULTI["Multi-agent 委派与报告汇总"]
    MULTI --> REACT
    REACT --> CTX["Context 上下文、历史与运行状态"]
    PLAN --> MODEL["Model Runtime 请求适配与输出解析"]
    REACT --> MODEL
    MODEL --> API["Responses API 或兼容接口"]
    REACT --> CHECK["工具权限与参数校验"]
    CHECK --> MCP["codex-mcp 独立进程 / MCP stdio"]
    MCP --> FILE["文件工具 / 路径边界校验"]
    MCP --> EXEC["exec / bwrap 与 seccomp 隔离"]
    FILE --> WS["Workspace 工作目录"]
    EXEC --> WS
    REACT -. "apply_patch / exec 结果" .-> OBS["Observer 异步观察器"]
    OBS --> MODEL
    OBS -. "回填观察" .-> CTX
    APP --> STORE["Persistence 运行锁、检查点与执行记录"]
    REACT --> STORE
    ORCH --> STORE
    MULTI --> STORE
    STORE --> DB[("MySQL")]
    APP --> OUTPUT["Outputer 运行事件输出"]
    MODEL --> OUTPUT
    OUTPUT --> STDERR["stderr 诊断信息"]
    OUTPUT -. "可选" .-> REDIS[("Redis Stream")]
    APP --> ANSWER["stdout 已保存的最终答案"]
```

`codex-cli` 在宿主进程中组织模型调用和状态推进，`codex-mcp` 通过 stdio 提供工具服务；命令隔离在 `exec` 内部实施。MySQL 保存恢复所需的运行记录，Redis Stream 用于可选的事件输出。

### 各执行文件的职责与关系

`make build` 生成以下五个执行文件，默认输出到 `bin/`：

| 执行文件 | 作用 | 与其他组件的关系 |
| --- | --- | --- |
| `codex-cli` | 从命令行执行或恢复 Agent 任务，输出最终答案 | 调用共享 Application 运行层，访问模型接口和 MySQL，默认启动 `codex-mcp` 子进程；可选 Redis 输出 |
| `codex-server` | 提供 HTTP 任务提交、恢复、中断及 SSE 事件接口 | 将任务写入 Redis List，由 Consumer 执行；读取 Redis Stream 推送事件，并访问 MySQL 查询运行与工具调用快照 |
| `codex-consumer` | 消费 Redis List 中的任务，执行或恢复 Agent 运行 | 与 CLI 复用 Application 运行层，访问模型接口和 MySQL，向 Redis Stream 写入事件；默认使用进程内工具，可通过 `--mcp-command` 启动独立 MCP 子进程 |
| `codex-mcp` | 提供文件读写、搜索与命令执行等 MCP stdio 工具 | 由 CLI、配置了 `--mcp-command` 的 Consumer 或其他 MCP 客户端连接；工具作用于指定 Workspace，独立运行无需 MySQL |
| `codex-migrate` | 显式创建或升级 MySQL 表结构 | 在首次运行或数据库结构升级时单独执行，为 CLI、Consumer 和 Server 准备持久化表 |

```mermaid
flowchart TD
    USER["终端用户"] --> CLI["codex-cli"]
    CLIENT["HTTP / SSE 客户端"] --> SERVER["codex-server"]
    SERVER -->|任务入队| QUEUE[("Redis List")]
    QUEUE -->|消费任务| CONSUMER["codex-consumer"]
    CLI -->|启动 / MCP stdio| MCP["codex-mcp"]
    CONSUMER -. "指定 --mcp-command" .-> MCP
    CONSUMER -->|默认| LOCAL["进程内工具"]
    MCP --> WS["Workspace / 文件工具与命令沙箱"]
    LOCAL --> WS
    CLI --> API["Responses API 或兼容接口"]
    CONSUMER --> API
    CLI -->|"运行、检查点与运行事件"| DB[("MySQL")]
    CONSUMER -->|"运行、检查点与运行事件"| DB
    SERVER -->|运行与工具调用快照查询| DB
    MIGRATE["codex-migrate"] -->|创建 / 升级表结构| DB
    CONSUMER -->|输出事件| STREAM[("Redis Stream")]
    CLI -. "可选输出" .-> STREAM
    STREAM -->|读取任务事件| SERVER
    SERVER -->|SSE 推送| CLIENT
```

命令行路径由 `codex-cli` 直接执行任务；HTTP 路径由 `codex-server` 接收请求，经 Redis 队列交给 `codex-consumer` 执行。Server 和 Consumer 是分别启动的进程，通过 Redis 协作；HTTP 提交成功表示任务已入队。CLI 与 Consumer 共享上述运行时组件，Consumer 不需要调用 CLI 二进制。

首次启动前先创建数据库并执行 `codex-migrate`。HTTP 部署还需启动 Redis、Consumer 和 Server，并让 Server 与 Consumer 使用相同的 Redis 数据库和 MySQL 数据库。接入方式见 [HTTP / SSE 服务](docs/codex-server.md)和 [Redis 任务消费](docs/redis-consumer.md)。

## 项目功能

### 任务执行与代理协作

- **单代理执行**：通过 ReAct 循环调用工具、处理结果并完成任务。
- **计划执行**：先生成有序计划，再逐步执行；支持运行中切换到计划模式。
- **多代理协作**：按职责和允许的委派关系分工，支持并发子任务、继续委派和报告汇总。
- **代理生命周期**：记录主代理和各次独立委派的父子关系、状态与输出归属。详见 [代理生命周期协议](docs/agent-lifecycle.md)。

### 模型接入与工具能力

- **模型配置**：接入 Responses API 及兼容接口，支持代理独立配置、流式输出、JSON 与 Line Protocol 输出格式。
- **文件操作**：读取文件、浏览目录、匹配路径、搜索内容和应用补丁。
- **命令沙箱**：在 Linux 沙箱中执行命令，限制工作目录访问、网络、运行时间和输出量。
- **技能加载**：发现本地技能，按需加载正文，复用知识和工作流程。
- **工具权限**：按代理配置工具允许列表，并在执行前校验权限和参数。

### 上下文管理与运行恢复

- **任务上下文**：维护目标、完成标准、计划进度、工具交互和结构化观察。
- **会话延续**：新任务可引用同一会话此前运行的结果及已保存历史。详见 [会话历史与恢复](docs/conversation-history.md)。
- **检查点恢复**：恢复已确认的工具历史，跳过已完成步骤，继续未完成委派；工具执行结果未知时停止自动恢复。
- **主动中断**：停止整个运行及其子任务并保存中断状态；后续通过新运行延续会话，中断后的原运行不能恢复。详见 [HTTP 中断接口](docs/codex-server.md#主动中断运行)。

### 服务接入与事件输出

- **命令行入口**：直接执行或恢复任务，输出最终答案。
- **HTTP 与任务队列**：通过 HTTP 提交任务，由 Redis 队列交给 Consumer 执行，支持配置消费并发数。详见 [HTTP / SSE 服务](docs/codex-server.md) 和 [Redis 任务消费](docs/redis-consumer.md)。
- **实时事件**：通过 Redis Stream 和任务级 SSE 订阅正文、工具交互、用量与生命周期事件。详见 [Redis 输出协议](docs/redis-output.md)。
- **持久历史与回放**：保存运行与工具调用快照，任务级 SSE 支持 Redis 保留期内的游标接续。详见 [恢复代理视图与接续输出](docs/output-replay.md)。
- **会话查询**：查询会话内各次运行的状态、结果、错误及工具调用快照。详见 [会话运行查询](docs/codex-server.md#查询会话的全部代理运行)。

## 代码结构

```text
cmd/main.go           源码入口：直接 ReAct 或 MCP stdio 服务，不纳入 make build
cmd/codex-cli/        编排任务的 CLI，连接独立 MCP 子进程
cmd/codex-server/     HTTP 任务接口、历史查询与 SSE 订阅
cmd/codex-consumer/   Redis 队列消费者，执行或恢复任务
cmd/mcp/              独立 MCP stdio 服务
cmd/migrate/          显式创建/升级持久化表
internal/config/      YAML 校验、基础配置与独立配置解析
internal/database/    MySQL 连接、显式迁移与运行存取
internal/application/ CLI 与 Consumer 共享的持久化运行生命周期
internal/server/      HTTP 路由、任务事件流与运行快照查询
internal/consumer/    队列任务消费、并发控制与终态通知
internal/rdb/         Redis 连接、队列与中断信号
internal/agent/       编排器、ReAct、planner、观察器与 worker
internal/context/     运行状态、步骤记录、上下文格式与提示词
internal/provider/    Responses API、输出 schema、协议解析与请求日志
internal/mcp/         文件工具、路径校验、补丁处理与命令隔离
examples/             YAML 配置示例
docs/adr/             架构决策
```

领域术语见 [GLOSSARY.md](GLOSSARY.md)。本地议题以 Markdown 记录在 `.scratch/<feature>/`，规则见 [议题跟踪说明](docs/agents/issue-tracker.md)。
