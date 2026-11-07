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

每个 task 写入 `codex-go-resp-stream-{task_id}`。通过 `codex-consumer` 消费时，该消息及其所有代理的输出统一使用消息中的 `TaskId`，数据库中的运行 ID 独立生成。输出事件只写入 Redis Stream，不写入 MySQL；Redis 写入失败时返回错误。每条记录包含 Redis 自动生成的 entry ID，以及以下字段：

| 字段 | 内容 |
| --- | --- |
| `type` | 下表中的事件类型 |
| `delta` | 对应数据结构的 JSON 字符串 |
| `run_id` | 所属运行 ID |
| `agent_instance_id`、`parent_agent_instance_id`、`agent_name` | 代理参与实例、父实例及名称；运行汇总可为空 |

ID 使用 Redis 原生 Stream ID，不在 fields 中重复存储。例如：

```text
ID: 1793261930925-0
type: text_delta
delta: {"text":"hello"}
```

| type | delta 示例 |
| --- | --- |
| `task_info` | `{"run_id":"8ab8b473adf7df4574a37fa158d4ff59","conv_id":"09db16c71e2c040d0dc32019f2b504b9","model_name":"your-model"}` |
| `task_completed` | `{}`（队列任务成功或业务失败终止后） |
| `task_interrupted` | `{}`（仅队列任务主动中断且中断状态保存成功后） |
| `agent_lifecycle` | `{"status":"running"}`（状态及转换见[代理生命周期协议](agent-lifecycle.md)） |
| `agent_switch` | `{"src_agent_name":"supervisor","dst_agent_name":"worker"}` |
| `text_delta` | `{"text":"hello"}` |
| `answer_delta` | `{"text":"解码后的答案片段"}` |
| `output_reset` | `{"text_bytes":42,"answer_bytes":6}`（撤回本次失败响应已发送的片段） |
| `tool_call` | `{"name":"read_file","call_id":"call-1","arguments":"{\"path\":\"a.txt\"}"}` |
| `tool_result` | `{"name":"read_file","call_id":"call-1","result":[{"type":"text","text":"content"}]}` |
| `tool_error` | `{"name":"read_file","call_id":"call-1","error":"file missing"}` |
| `text` | `{"text":"提取并解码后的 answer"}` |
| `usage` | `{"model":"your-model","response_id":"resp-1","status":"completed","input_tokens":"20","output_tokens":"5","cached_input_tokens":"4","uncached_input_tokens":"16"}` |

`task_info` 在新运行的 pending 检查点保存后、开始执行前发送；恢复未完成的运行时也会发送原有 ID。直接重放已完成的运行不会发送该事件。队列任务通过 `TaskId` 对应的 Stream 接收事件；其中的 `run_id` 和 `conv_id` 是数据库中的运行和会话 ID。`model_name` 为当前加载配置中的模型名称（`LLMConfig.Model`）；未配置运行级模型时为空字符串。该事件写入失败会使启动或恢复返回错误。

工具失败时，`tool_error` 也保留能获取到的 `result`。`result` 是 MCP content 的 JSON 数组，`arguments` 是模型生成的 JSON 字符串。模型校验导致整批调用被拒绝时，每个调用收到一个 `tool_error`，说明该批未执行。结果只在执行完成时写入，不随后续模型历史重放重复写入。

`task_completed` 由 `codex-consumer` 在任务成功或业务失败结束且最终状态保存成功后写入一次；模型轮次、执行步骤或子代理完成不会单独产生此事件，执行故障不会产生。该事件沿用任务消息中的 `TaskId`，支持与普通输出相同的回放和续传；直接运行代理不会产生此队列任务事件。

主动中断的队列任务在中断状态保存成功后发送 `task_interrupted`，不发送 `task_completed`；中断状态保存失败不会发送 `task_interrupted`。该事件同样使用任务的 Stream，支持回放和续传。

Stream 不发送 schema、请求上下文和原始响应；这些事件仅按日志配置输出。流式 delta、工具结果和 usage 不受 `log_level` 过滤；`stream: false` 时只发送提取后的 `text`，不发送 `text_delta/answer_delta`。usage 按每次 HTTP 尝试记录，缺失计数使用字符串 `"unknown"`，与 `"0"` 区分。

消费者可以用 `XRANGE` 或 `XREAD` 读取临时 Stream；每次写入将过期时间刷新为 24 小时。SSE 的 `id` 是 Redis Stream 事件标识，用于该任务流内的去重和续传。服务端不提供运行事件历史查询接口，Stream 过期后无法通过任务级 SSE 补读。

```sh
redis-cli XRANGE codex-go-resp-stream-your-task-id - +
redis-cli XREAD BLOCK 5000 STREAMS codex-go-resp-stream-your-task-id 0-0
```


当 `stream: true` 时，`answer_delta` 在模型生成过程中提取 JSON 的根级
`answer` / `result.answer`，或 line protocol 根块 / 直接 RESULT 子块的
`ANSWER` 字符串。字段名、转义和 Unicode 代理对跨 chunk 时会等待足够输入，
只发送已解码的内容。stderr 仅在 `log_level: debug` 时打印 `LLM answer delta=...`，
其他级别只写入 Stream，避免流式内容刷屏。
同时保留原始 `text_delta`，不发送完整 `text` 事件；同一轮响应中两类片段与 `text` 互斥。

对于已识别的 `answer` / `result.answer` / `ANSWER` 字符串，若流式提取发现未转义的
控制字符、非法转义（如 `\q`）或 Unicode 转义中出现非法十六进制字符，会立即取消
当前 LLM 请求并关闭流，不等待完整响应；确认非法的当前 chunk 不再发送。
随后撤回此前已发送的片段，并以输出格式错误进入原有纠错重试流程，父运行上下文
保持有效。跨 chunk 尚未收全的 `\`、`\uXXXX` 和 Unicode 代理对会继续等待，
不会提前判错。其他结构及 schema 错误仍由完整响应解析校验。

这些片段是当前模型轮次的临时内容，不代表任务已成功：完整响应仍需解析和业务校验，
响应格式校验失败、混合最终结果与工具调用被拒绝、请求失败或流中断时，若本次请求已发送片段，
会在返回错误及后续纠错重试之前发送 `output_reset`，并在所有日志级别（包括 `error`）
打印撤回的字节数到 stderr。消费端必须处理该事件：按
`run_id + agent_instance_id` 分别维护各代理的正文，对 `text_delta` 缓冲删除末尾
`text_bytes` 个 **UTF-8 字节**，对 `answer_delta` 缓冲删除末尾 `answer_bytes` 个
UTF-8 字节；仅撤回失败请求的内容，保留此前轮次。JavaScript 字符串长度按 UTF-16
计算，不能直接用这两个字节数调用字符串 `slice`；可用 `TextEncoder` / `TextDecoder`
对 UTF-8 字节缓冲截断后解码。按 SSE/Redis entry ID 去重，避免重复执行撤回。
同一响应的 `text_delta` 与 `answer_delta` 是原始正文和解码答案两种视图，选择一种展示，
不要把两个通道拼接到同一个答案中。非流式 `text` 在完整响应通过解析后才发送，
格式失败的响应不发送 `text`，也无需撤回。

撤回事件要求消费端升级；忽略该事件的旧消费端仍会保留失败片段。业务校验在代理层执行，
`output_reset` 只覆盖 provider 请求或格式解析失败，不代表最终业务结果确认。
不要跨轮次直接拼接成已确认的最终答案；
队列任务的整体完成仍以 `task_completed` 为准。服务端只提供最终文本、未提供
正文 delta 时，不会补发 `answer_delta`。


Line protocol 也支持空格或 Tab 分隔的单行格式，例如
`BEGIN RESULT STATUS="completed" ANSWER="XXX" END RESULT`。
流式提取和最终解析使用相同的分隔规则，字符串内的空格、协议关键字和转义保持原样；
原有逐行格式仍然支持。

实时输出使用 `/tasks/{task_id}/events`；接续说明见[任务输出接续](output-replay.md)。

多代理模式在委派前发送 `agent_switch`（父代理 → 子代理），子代理产生最终结果并交回父代理时发送反向事件，包括显式返回、无后续委派和业务失败。主代理最终结束、模型多轮执行及执行错误不产生额外切换事件。事件归属于发起切换的代理参与实例，消息体中的名称只用于展示；并发分支分别发出事件，不表示全局只有一个代理在运行。切换事件沿用统一的 Redis 和 SSE 链路；写入错误向上传播，并停止后续调度。恢复跳过的已完成子代理不重新发送切换事件。
