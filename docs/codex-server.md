# codex-server 客户端接入

`codex-server` 提供 HTTP 接口提交新任务、恢复已有运行、请求中断运行，并通过 Server-Sent Events（SSE）读取任务输出。提交成功表示任务已入队，不表示任务已开始或执行成功。

代理状态事件详见[代理生命周期协议](agent-lifecycle.md)；Redis 输出协议详见 [Redis Stream 输出](redis-output.md)。任务输出接续说明见[恢复代理视图与接续输出](output-replay.md)。



## 启动

先按 [Redis List 任务消费](redis-consumer.md) 配置 Redis、MySQL 和数据库迁移，并启动消费者。消费者的 Redis 配置应与服务端连接到同一个 Redis 数据库，且需要配置 Redis 输出，才能收到事件。

```sh
export CODEX_MYSQL_URL='mysql://user:password@127.0.0.1:3306/codex-go'
go run ./cmd/migrate
go run ./cmd/codex-server --redis-url redis://127.0.0.1:6379/0
```

`--addr` 默认为 `:8080`，`--redis-url` 默认为 `redis://localhost:6379/0`。以下示例以 `http://localhost:8080` 为服务地址。

## 创建任务

`POST /tasks`

请求头：`Content-Type: application/json`

```json
{
  "input": "结合代码分析项目是如何实现 multi-agent 的"
}
```

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `input` | string | 二选一 | 新任务内容；与 `run_id` 互斥，不能为空或只有空白字符。原始字符串会传给消费者。 |
| `run_id` | string | 二选一 | 恢复已有运行，详见下文；与 `input` 互斥，不能为空或只有空白字符。 |
| `conv_id` | string | 否 | 新任务所属的已有会话 ID；省略或空字符串时创建新会话。恢复时沿用原运行的会话，此字段不改变其归属。 |

继续已有会话时，请求体示例：`{"input":"继续上一任务","conv_id":"已有会话ID"}`。这会创建新的运行，并加载该会话较早运行的历史；主动中断运行的已保存进度也可作为参考，详见 [会话历史与恢复](conversation-history.md)。服务端负责入队，会话或运行是否存在由消费者执行任务时校验。

`input` 和 `run_id` 必须恰好提供一个有效值；另一个字段应省略或为 `""`，即使只包含空白字符也不能与有效字段同时提交。

```sh
curl -i -X POST 'http://localhost:8080/tasks' \
  -H 'Content-Type: application/json' \
  -d '{"input":"结合代码分析项目是如何实现 multi-agent 的"}'
```

成功返回 HTTP `201`：

```json
{"task_id":"0123456789abcdef0123456789abcdef"}
```

`task_id` 是服务端生成的 32 位小写十六进制字符串，客户端应保存它以订阅对应事件。它与数据库中的运行 ID 和会话 ID 不同。

错误返回 JSON：

| HTTP 状态 | `error` | 说明 |
| --- | --- | --- |
| 400 | `provide either input or run_id` | JSON 无效、字段类型错误、未提供有效的 `input` 或 `run_id`，或两者同时非空。 |
| 500 | `cannot generate task ID` | 无法生成任务 ID。 |
| 500 | `cannot enqueue task` | 任务写入队列失败。 |

例如：`{"error":"provide either input or run_id"}`。运行不存在、运行锁被占用或配置不匹配等执行阶段错误不会作为本次 POST 的 HTTP 错误返回。

## 订阅任务事件

`GET /tasks/{task_id}/events`

| 参数 | 位置 | 必填 | 说明 |
| --- | --- | --- | --- |
| `task_id` | 路径 | 是 | 创建任务时返回的 ID。 |
| `last_event_id` | 查询参数 | 否 | 上次收到并处理的 Redis Stream 事件 ID，从该事件**之后**继续读取。 |
| `Last-Event-ID` | 请求头 | 否 | 与查询参数作用相同；查询参数非空时优先使用查询参数。 |

```sh
curl -N 'http://localhost:8080/tasks/TASK_ID/events'
curl -N 'http://localhost:8080/tasks/TASK_ID/events?last_event_id=EVENT_ID'
# 或使用请求头续传
curl -N -H 'Last-Event-ID: EVENT_ID' 'http://localhost:8080/tasks/TASK_ID/events'
```

成功建立连接后返回 HTTP `200`，`Content-Type: text/event-stream`。每个事件由 `id`、`data` 和一个空行组成，例如：

```text
id: 1793261930925-0
data: {"id":"1793261930925-0","type":"text_delta","delta":"{\"text\":\"hello\"}"}

```

| `data` 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | Redis Stream ID，与 SSE 的 `id` 相同。 |
| `run_id` | string | 所属代理运行 ID，与任务流 ID 分开。 |
| `agent_instance_id`、`parent_agent_instance_id`、`agent_name` | string | 代理参与实例归属；主代理父身份为空，运行汇总可无代理身份。 |
| `type` | string | 输出类型，如 `task_info`、`agent_lifecycle`、`answer_delta`、`text_delta`、`tool_call`、`tool_result`、`tool_error`、`text`、`usage`、`task_completed` 或 `task_interrupted`。 |
| `delta` | string | 对应类型的数据，以 JSON **字符串**编码；需要对该字符串再次解析 JSON。 |

HTTP 接口的 JSON 字段使用小写下划线命名；旧版 `ConvId`、`ID`、`Type` 和 `Delta` 字段需要相应更新。各类型的 `delta` 结构、流式输出配置及保留策略见 [Redis Stream 输出](redis-output.md)。客户端应按 SSE 协议解析事件，再解析 `data` JSON；如果需要读取具体内容，再解析 `delta`。例如浏览器中可使用：

```js
const source = new EventSource(`/tasks/${encodeURIComponent(taskId)}/events`);
source.onmessage = (event) => {
  const payload = JSON.parse(event.data);
  const delta = JSON.parse(payload.delta);
  console.log(event.lastEventId, payload.type, delta);
  if (payload.type === 'task_completed' || payload.type === 'task_interrupted') {
    source.close();
  }
};
```

未提供续传 ID 时，服务从 `0-0` 开始回放 Redis 中仍保留的事件，然后等待新事件。连接断开后可用最后已处理的事件 ID 续传；浏览器 `EventSource` 自动重连时会发送 `Last-Event-ID`。如客户端自行管理重连，应保存 ID 并在处理事件后更新，必要时按 ID 去重。

任务成功或业务失败结束，且最终运行状态保存成功后，消费者发送一条 `task_completed` 事件，`delta` 为 JSON 字符串 `"{}"`。该事件使用与普通输出相同的 Redis Stream ID，支持回放和续传。例如：

```text
id: 1793261930926-0
data: {"id":"1793261930926-0","type":"task_completed","delta":"{}"}

```

主动中断并保存中断状态后，消费者发送 `task_interrupted` 事件，`delta` 同样为 `"{}"`；保存失败不会发送该事件。连接不会因任务结束自动关闭，客户端收到 `task_completed` 或 `task_interrupted` 后应主动关闭连接。`text` 和 `usage` 可能在一次任务中出现多次，不能单独作为任务结束信号。业务失败也发送 `task_completed`，该事件表示任务结束而不表示业务成功；执行故障目前没有专用失败事件；进程异常退出或终态事件写入 Redis 失败时，客户端也可能收不到通知。当前接口不提供任务状态查询；运行开始后可通过下文的中断接口请求停止。未知任务 ID 也会建立连接并等待事件，不能通过 HTTP 200 判断任务是否存在或成功。客户端应根据自身需求关闭连接或设置超时。

SSE 接口只从 Redis Stream 读取并推送事件，不写入 MySQL。Redis 读取失败会结束连接；由于响应头已经发送，客户端通常只能观察到连接断开。续传仍从 Redis 读取，并受 Redis Stream 保留时间限制。

模型配置 `stream: false` 时，`text` 的 `delta` 为 `{"text":"提取并解码后的 answer"}`；`stream: true` 时发送 `text_delta/answer_delta`，不发送 `text`。互斥规则按模型轮次生效。

## 获取运行和会话 ID

新运行的 pending 检查点保存后、执行开始前会发送 `task_info`。解析 `delta` 后可取得运行和会话 ID，以及当前使用的模型名称 `model_name`：

```text
id: 1793261930924-0
data: {"id":"1793261930924-0","type":"task_info","delta":"{\"run_id\":\"8ab8b473adf7df4574a37fa158d4ff59\",\"conv_id\":\"09db16c71e2c040d0dc32019f2b504b9\",\"model_name\":\"your-model\"}"}

```

| ID | 用途 |
| --- | --- |
| `task_id` | 一次队列请求的标识，用于订阅 SSE；恢复请求会生成新的值。 |
| `run_id` | 持久化运行的标识，用于恢复或中断；恢复时保持不变。 |
| `conv_id` | 会话标识，用于在新任务的 `conv_id` 中引用此前历史。 |
| 任务级 SSE `id` / `id` | Redis Stream 事件标识，用于该任务流续传。 |

`model_name` 取自当前加载配置的 `LLMConfig.Model`；未配置运行级模型时为空字符串。

恢复未完成运行时也会发送原有的 `run_id` 和 `conv_id`；直接读取已完成运行的保存结果时不发送 `task_info`。

## 恢复已有运行

通过 `POST /tasks` 提交 `run_id`，省略 `input`：

```sh
curl -i -X POST 'http://localhost:8080/tasks' \
  -H 'Content-Type: application/json' \
  -d '{"run_id":"已有运行 ID"}'
```

成功仍返回 HTTP `201` 和新的 `task_id`。持久化 `run_id` 不变，原任务事件不会复制到新任务流。客户端需使用新的 `task_id` 订阅任务级 SSE；任务级 SSE 只覆盖对应任务，受 Redis 保留时间限制。

支持 `react`、`plan-execute` 和 `multi-agent`。消费者沿用原运行的目标、工作目录、模式和会话，使用当前消费者的模型及工具配置；多代理运行必须使用多代理配置恢复。恢复按执行实例续接已确认工具历史，计划跳过已完成步骤，多代理恢复委派关系与已完成报告。已登记但结果未知的工具调用会阻止自动恢复，以免重复产生副作用。同一 `run_id` 同时只能由一个执行者持有运行锁，重复提交不会绕过此限制。

`pending`、`running` 和可恢复的 `failed` 运行可继续执行；终态业务失败不会重新执行，主动中断的 `interrupted` 运行不能恢复。已完成运行只读取保存结果，消费者不会将该结果重新发布为答案事件，但成功处理后仍会发送 `task_completed`。恢复已保存的业务失败时也发送 `task_completed`；其他恢复失败只记录消费者错误日志，不发送 `task_completed` 或专用失败事件。

部署前先执行 `go run ./cmd/migrate`，同步运行、步骤、执行实例、工具记录及多代理检查点所需的数据库结构。

## 主动中断运行

`POST /runs/{run_id}/interrupt` 请求停止整个根运行，包括计划步骤及多代理子任务。`run_id` 来自 `task_info` 事件；这不是任务队列的 `task_id`。

无需请求体。例如：

```sh
curl -i -X POST 'http://localhost:8080/runs/RUN_ID/interrupt'
```

`task_info` 的 `delta` 解析后包含 `run_id`；事件在运行检查点保存后、执行开始前发送。任务尚在队列中时没有可用于中断的运行 ID，此接口不取消排队中的任务。

- `202`，`{"status":"accepted"}`：持有运行锁的执行者已确认接收请求；此时执行可能尚未停止，中断状态也可能尚未保存，此响应不代表中断已完成。
- `409`，`{"status":"already_requested"}`：执行者已接收过请求；`{"status":"terminal"}` 表示运行已结束。
- `404`，`{"status":"not_found"}`：运行不存在。
- `503`，`{"status":"unavailable"}`：当前无法确认将请求送达执行者；存储或信号通信出错也返回 `503`。

通信或存储出错时，`503` 响应体使用 `{"error":"具体错误"}`；中断服务未配置时为 `{"error":"interrupt service unavailable"}`。`unavailable` 可能表示运行当前没有执行者或执行者未及时确认，不能据此判断运行已结束。

运行持久化为 `interrupted` 后，不能通过原 `run_id` 恢复。后续需通过 `POST /tasks` 提交新 `input` 创建新运行，可同时传入原 `conv_id` 延续会话并参考已保存进度；新运行会获得新的 `run_id`。

客户端收到原任务事件流中的 `task_interrupted` 后，可以确认执行已停止且中断状态已保存。SSE 连接断开、停止输出或未收到 `task_completed` 都不能作为中断完成的依据；`409` 的 `terminal` 也不能单独证明运行已持久化为 `interrupted`。当前没有运行状态查询接口，终态事件写入失败时客户端可能收不到通知。

请求断开或 SSE 连接断开不会主动中断运行。执行中的工具可能已产生副作用，中断不能撤销这些副作用。

## 查询会话的全部代理运行

`GET /conversations/{conv_id}/runs`

返回持久化的会话运行及工具调用快照，不依赖 SSE 订阅或 Redis 事件保留时间。

```json
{
  "conv_id": "conversation-id",
  "runs": [
    {
      "run_id": "run-id",
      "conv_id": "conversation-id",
      "goal": "inspect repository",
      "status": "completed",
      "result": {"answer": "done"},
      "error": "",
      "failure_type": "",
      "mode": "react",
      "create_at": "2026-10-31T12:00:00Z",
      "update_at": "2026-10-31T12:01:00Z",
      "agents": [],
      "tool_calls": [
        {
          "execution_id": "run-id",
          "call_id": "call-1",
          "position": 0,
          "name": "read_file",
          "arguments": "{\"path\":\"README.md\"}",
          "status": "completed",
          "result": "file contents",
          "error": "",
          "batch": 0,
          "batch_position": 0
        }
      ]
    }
  ]
}
```

- 包含该会话所有状态的运行，按 `create_at`、`run_id` 升序返回，不分页。
- `result` 保留数据库中的 JSON 值，无结果时为 `null`；错误文本通过 `error` 返回。
- `tool_calls` 包含该运行所有执行实例（含计划步骤及多代理委派）的已持久化调用。通过 `execution_id`、`position` 区分实例及调用顺序；参数和工具结果保留为字符串。尚无结果的调用仍返回其当前状态，不推断成功或失败。
- 工具调用按 `create_at`、`execution_id`、`position` 升序返回；这不表示并行调用的完成顺序。
- 已有会话无运行时返回 `200` 和 `runs: []`；运行无工具调用时返回 `tool_calls: []`。
- 会话不存在返回 `404`；数据库读取失败返回 `500`；未配置会话查询能力返回 `503`。错误响应格式为 `{"error":"..."}`。
- 查询返回已提交的快照，执行中的最新增量仍以 SSE 为准。

### 多代理历史视图

multi-agent 运行新增 `agents` 数组，包含主代理及全部已保存的委派（包括尚未执行的子代理）。客户端可为每个元素复用相同的代理展示组件，通过 `agent_instance_id` 关联 SSE 中的实时输出，通过 `parent_agent_instance_id` 建立层级。主代理的父身份为 `""`。

| 字段 | 说明 |
| --- | --- |
| `agent_instance_id` | 稳定的代理参与实例 ID；同名多次委派分别返回，同一委派的多轮执行归为一个元素。 |
| `parent_agent_instance_id` | 发起委派的参与实例 ID。 |
| `agent_name` | 展示名称；新检查点保存入口名称，旧检查点缺少入口名称时为 `""`，不猜测名称。 |
| `goal`、`completion_criteria` | 该参与实例的任务和验收标准；未保存标准时为 `null`。 |
| `status` | 检查点确认的 `completed` / `failed`；尚无确认终态时为 `unknown`。它不是实时生命周期状态，不根据父运行终态推断子代理完成、失败或中断。 |
| `result` | 已确认的最终 ReactResult（含 `status`、`answer`、`Observation`、`CompletionEvidence`）；尚未确认最终结果时为 `null`。 |
| `error` | 最后一轮已保存的执行错误；无记录时为 `""`。业务失败原因也可能体现在 `result.answer`。 |
| `tool_calls` | 该参与实例各轮工具记录，字段结构与运行级 `tool_calls` 相同。 |
| `turns` | 按执行轮次排列的历史，每轮包含 `execution_id`、`result`、`error`、`tool_calls`。轮次结果格式与代理 `result` 相同，未确认的轮次结果为 `null`。 |

例如，子代理的视图如下（所属运行的 `agents` 数组中的一个元素）：

```json
{
  "agent_instance_id": "run-id/delegation-2",
  "parent_agent_instance_id": "run-id/delegation-1",
  "agent_name": "worker",
  "goal": "inspect files",
  "completion_criteria": ["files checked"],
  "status": "completed",
  "result": {
    "status": "completed",
    "answer": "files checked",
    "Observation": [],
    "CompletionEvidence": []
  },
  "error": "",
  "tool_calls": [],
  "turns": [
    {
      "execution_id": "run-id/delegation-2/turn-0",
      "result": {
        "status": "completed",
        "answer": "files checked",
        "Observation": [],
        "CompletionEvidence": []
      },
      "error": "",
      "tool_calls": []
    }
  ]
}
```

`agents` 采用深度优先的委派顺序，父代理先于其子树；兄弟委派按创建顺序返回，不表示并行执行的时间顺序。代理级工具按轮次汇总，轮内沿用运行级工具顺序；运行级 `tool_calls` 继续包含全部工具，客户端不要重复渲染两份记录。无工具或轮次时分别返回 `[]`；非 multi-agent 运行或尚无多代理检查点时返回 `agents: []`。

该视图只包含已提交的回复与工具快照，不包含尚未提交的正文增量、完整思考流或生命周期事件历史，也不暴露原始检查点、委派上下文及继承的会话内容。实时状态和增量仍通过任务 SSE 获取，不改变 Redis 事件保留策略。无需新增数据库表或列。
