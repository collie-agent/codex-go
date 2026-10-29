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

`addr` 必填，格式为 `host:port`；`db` 默认为 0。密码可以使用 `password` 或 `password_env`，两者互斥。省略 `redis` 时不建立 Redis 连接。Redis 是全局配置，只能在 YAML 顶层声明，不能放在 `base` 或 `agents.<name>` 中。规划器、观察器和各代理共享同一份全局设置。

每个 task 写入 `codex-go-resp-stream-{task_id}`。普通运行的 task_id 为 RunId；多代理任务使用其实际任务 ID。每条记录包含 Redis 自动生成的 entry ID，以及两个字段：

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
| `text_delta` | `{"text":"hello"}` |
| `tool_call` | `{"name":"read_file","call_id":"call-1","arguments":"{\"path\":\"a.txt\"}"}` |
| `tool_result` | `{"name":"read_file","call_id":"call-1","result":[{"type":"text","text":"content"}]}` |
| `tool_error` | `{"name":"read_file","call_id":"call-1","error":"file missing"}` |
| `text` | `{"text":"完整的模型原始响应文本"}` |
| `usage` | `{"model":"your-model","response_id":"resp-1","status":"completed","input_tokens":"20","output_tokens":"5","cached_input_tokens":"4","uncached_input_tokens":"16"}` |

工具失败时，`tool_error` 也保留能获取到的 `result`。`result` 是 MCP content 的 JSON 数组，`arguments` 是模型生成的 JSON 字符串。模型校验导致整批调用被拒绝时，每个调用收到一个 `tool_error`，说明该批未执行。结果只在执行完成时写入，不随后续模型历史重放重复写入。

Stream 不发送 schema 和请求上下文。流式 delta、工具结果和 usage 不受 `log_level` 过滤；`stream: false` 时没有 delta。usage 按每次 HTTP 尝试记录，缺失计数使用字符串 `"unknown"`，与 `"0"` 区分。

消费者可以用 `XRANGE` 读取历史，或用 `XREAD` 从最后收到的 ID 继续读取。通过 `internal/rdb/redis.go` 的 `WriteTaskStream` 写入，`ReadTaskStream` 可读取相同协议；每次写入将过期时间刷新为 24 小时，不自动裁剪。写入错误会记录诊断，不触发模型或工具重试；Redis 写入也不自动重试，以避免重复事件。

```sh
redis-cli XRANGE codex-go-resp-stream-your-task-id - +
redis-cli XREAD BLOCK 5000 STREAMS codex-go-resp-stream-your-task-id 0-0
```
