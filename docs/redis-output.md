# Redis Stream 输出

在 agent YAML 顶层配置 `redis` 即启用 Stream 输出，同时保留 stderr 日志：

```yaml
name: react
log_level: info
redis:
  addr: 127.0.0.1:6379
  # username: default
  # password_env: CODEX_REDIS_PASSWORD
  db: 0
base:
  llm:
    api_key_env: OPENAI_API_KEY
    model: your-model
    stream: true
```

`addr` 必填，格式为 `host:port`；`db` 默认为 0。密码可以使用 `password` 或 `password_env`，两者互斥。省略 `redis` 时不建立 Redis 连接。Redis 是全局配置，只能在 YAML 顶层声明，不能放在 `base` 或 `agents.<name>` 中。规划器和各代理共享同一份全局设置；观察器的输出仅打印到 stderr，不写入 Stream。

每个 task 写入 `codex-go-resp-stream-{task_id}`。普通运行的 task_id 为 RunId；多代理任务使用其实际任务 ID。通过 `codex-consumer` 消费时，该消息及其所有代理的输出统一使用消息中的 `TaskId`，数据库中的运行 ID 仍独立生成。每条记录包含 Redis 自动生成的 entry ID，以及两个字段：

| 字段 | 内容 |
| --- | --- |
| `type` | 下表中的事件类型 |
| `delta` | 对应数据结构的 JSON 字符串 |

ID 使用 Redis 原生 Stream ID，不在 fields 中重复存储。例如：

```text
ID: 1793261930925-0
type: text_delta
delta: {"text":"hello"}
```

| type | delta 示例 |
| --- | --- |
| `task_info` | `{"run_id":"8ab8b473adf7df4574a37fa158d4ff59","conv_id":"09db16c71e2c040d0dc32019f2b504b9"}` |
| `task_completed` | `{}`（仅队列任务整体成功完成后） |
| `text_delta` | `{"text":"hello"}` |
| `answer_delta` | `{"text":"解码后的答案片段"}` |
| `tool_call` | `{"name":"read_file","call_id":"call-1","arguments":"{\"path\":\"a.txt\"}"}` |
| `tool_result` | `{"name":"read_file","call_id":"call-1","result":[{"type":"text","text":"content"}]}` |
| `tool_error` | `{"name":"read_file","call_id":"call-1","error":"file missing"}` |
| `text` | `{"text":"提取并解码后的 answer"}` |
| `usage` | `{"model":"your-model","response_id":"resp-1","status":"completed","input_tokens":"20","output_tokens":"5","cached_input_tokens":"4","uncached_input_tokens":"16"}` |

`task_info` 在新运行的 pending 检查点保存后、开始执行前发送；恢复未完成的运行时也会发送原有 ID。直接重放已完成的运行不会发送该事件。队列任务通过 `TaskId` 对应的 Stream 接收事件；其中的 `run_id` 和 `conv_id` 是数据库中的运行和会话 ID。该事件写入失败会使启动或恢复返回错误。

工具失败时，`tool_error` 也保留能获取到的 `result`。`result` 是 MCP content 的 JSON 数组，`arguments` 是模型生成的 JSON 字符串。模型校验导致整批调用被拒绝时，每个调用收到一个 `tool_error`，说明该批未执行。结果只在执行完成时写入，不随后续模型历史重放重复写入。

`task_completed` 由 `codex-consumer` 在整个任务执行和最终检查点保存成功后写入一次；模型轮次、执行步骤或子代理完成不会单独产生此事件，失败任务也不会产生。该事件沿用任务消息中的 `TaskId`，支持与普通输出相同的回放和续传；直接运行代理不会产生此队列任务事件。

Stream 不发送 schema 和请求上下文。流式 delta、工具结果和 usage 不受 `log_level` 过滤；`stream: false` 时只发送提取后的 `text`，不发送 `text_delta/answer_delta`。usage 按每次 HTTP 尝试记录，缺失计数使用字符串 `"unknown"`，与 `"0"` 区分。

消费者可以用 `XRANGE` 读取历史，或用 `XREAD` 从最后收到的 ID 继续读取。通过 `internal/rdb/redis.go` 的 `WriteTaskStream` 写入，`ReadTaskStream` 可读取相同协议；每次写入将过期时间刷新为 24 小时，不自动裁剪。模型和工具输出的写入错误会记录诊断，不触发模型或工具重试；Redis 写入也不自动重试，以避免重复事件。

```sh
redis-cli XRANGE codex-go-resp-stream-your-task-id - +
redis-cli XREAD BLOCK 5000 STREAMS codex-go-resp-stream-your-task-id 0-0
```


当 `stream: true` 时，`answer_delta` 在模型生成过程中提取 JSON 的根级
`answer` / `result.answer`，或 line protocol 根块 / 直接 RESULT 子块的
`ANSWER` 字符串。字段名、转义和 Unicode 代理对跨 chunk 时会等待足够输入，
只发送已解码的内容。stderr 同时打印 `LLM answer delta=...`，不受日志级别过滤。
同时保留原始 `text_delta`，不发送完整 `text` 事件；同一轮响应中两类片段与 `text` 互斥。

这些片段是当前模型轮次的临时内容，不代表任务已成功：完整响应仍需解析和业务校验，
失败或重试不会撤回已发出的片段。不要跨轮次直接拼接成已确认的最终答案；
队列任务的整体完成仍以 `task_completed` 为准。服务端只提供最终文本、未提供
正文 delta 时，不会补发 `answer_delta`。


Line protocol 也支持空格或 Tab 分隔的单行格式，例如
`BEGIN RESULT STATUS="completed" ANSWER="XXX" END RESULT`。
流式提取和最终解析使用相同的分隔规则，字符串内的空格、协议关键字和转义保持原样；
原有逐行格式仍然支持。
