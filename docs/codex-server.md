# codex-server 客户端接入

`codex-server` 提供 HTTP 接口创建异步任务，并通过 Server-Sent Events（SSE）读取任务输出。创建成功表示任务已入队，不表示任务已开始或执行成功。

## 启动

先按 [Redis List 任务消费](redis-consumer.md) 配置 Redis、MySQL 和数据库迁移，并启动消费者。消费者的 Redis 配置应与服务端连接到同一个 Redis 数据库，且需要配置 Redis 输出，才能收到事件。

```sh
export CODEX_MYSQL_URL='mysql://user:password@127.0.0.1:3306/codex-go'
go run ./cmd/codex-server --redis-url redis://127.0.0.1:6379/0
```

`--addr` 默认为 `:8080`，`--redis-url` 默认为 `redis://localhost:6379/0`，`--flush-interval`（事件持久化间隔）默认为 `1s`。以下示例以 `http://localhost:8080` 为服务地址。

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
| `input` | string | 是 | 任务内容；不能为空或只有空白字符。原始字符串会传给消费者。 |
| `ConvId` | string | 否 | 已有会话 ID，随任务传给消费者以引用前序运行结果和错误；省略或空字符串时创建新会话。 |

继续已有会话时，请求体示例：`{"input":"继续上一任务","ConvId":"已有会话ID"}`。服务端负责入队，会话是否存在由消费者执行任务时校验。

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
| 400 | `input is required` | JSON 无效、`input` 类型错误或内容为空。 |
| 500 | `cannot generate task ID` | 无法生成任务 ID。 |
| 500 | `cannot enqueue task` | 任务写入队列失败。 |

例如：`{"error":"input is required"}`。

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
data: {"ID":"1793261930925-0","Type":"text_delta","Delta":"{\"text\":\"hello\"}"}

```

| `data` 字段 | 类型 | 说明 |
| --- | --- | --- |
| `ID` | string | Redis Stream ID，与 SSE 的 `id` 相同。 |
| `Type` | string | 输出类型，如 `answer_delta`、`text_delta`、`tool_call`、`tool_result`、`tool_error`、`text`、`usage` 或 `task_completed`。 |
| `Delta` | string | 对应类型的数据，以 JSON **字符串**编码；需要对该字符串再次解析 JSON。 |

字段名大小写如上。各类型的 `Delta` 结构、流式输出配置及保留策略见 [Redis Stream 输出](redis-output.md)。客户端应按 SSE 协议解析事件，再解析 `data` JSON；如果需要读取具体内容，再解析 `Delta`。例如浏览器中可使用：

```js
const source = new EventSource(`/tasks/${encodeURIComponent(taskId)}/events`);
source.onmessage = (event) => {
  const payload = JSON.parse(event.data);
  const delta = JSON.parse(payload.Delta);
  console.log(event.lastEventId, payload.Type, delta);
  if (payload.Type === 'task_completed') {
    source.close();
  }
};
```

未提供续传 ID 时，服务从 `0-0` 开始回放 Redis 中仍保留的事件，然后等待新事件。连接断开后可用最后已处理的事件 ID 续传；浏览器 `EventSource` 自动重连时会发送 `Last-Event-ID`。如客户端自行管理重连，应保存 ID 并在处理事件后更新，必要时按 ID 去重。

整个任务执行成功且最终运行检查点保存成功后，消费者发送一条 `task_completed` 事件，`Delta` 为 JSON 字符串 `"{}"`。该事件使用与普通输出相同的 Redis Stream ID，支持回放和续传。例如：

```text
id: 1793261930926-0
data: {"ID":"1793261930926-0","Type":"task_completed","Delta":"{}"}

```

连接不会因任务结束自动关闭，客户端收到 `task_completed` 后应主动关闭连接。`text` 和 `usage` 可能在一次任务中出现多次，不能单独作为任务结束信号。失败任务不发送 `task_completed`，目前也没有专用失败事件；进程异常退出或完成事件写入 Redis 失败时，客户端也可能收不到完成通知。当前接口不提供任务状态查询或取消接口。未知任务 ID 也会建立连接并等待事件，不能通过 HTTP 200 判断任务是否存在或成功。客户端应根据自身需求关闭连接或设置超时。

服务按 `--flush-interval` 将该连接已发送的事件批量写入 MySQL，断开时也会尝试写入剩余事件。Redis 读取或数据库写入失败会结束连接；由于响应头已经发送，客户端通常只能观察到连接断开。续传仍从 Redis 读取，并受 Redis Stream 保留时间限制；数据库写入不保证客户端已处理事件，也不作为该接口的回放来源。

模型配置 `stream: false` 时，`text` 的 Delta 为 `{"text":"提取并解码后的 answer"}`；`stream: true` 时发送 `text_delta/answer_delta`，不发送 `text`。互斥规则按模型轮次生效。

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
