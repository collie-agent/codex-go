# Redis List 任务消费

`codex-consumer` 从 `internal/rdb.TASK_QUEUE_NAME`（`codex-go-task-queue`）消费 JSON 编码的 `entity.TaskMessage`，调用 `application.Run()` 执行任务。

配置直接复用 `examples/` 中的 agent YAML，顶层 `redis` 必填，连接参数与 [Redis 输出](redis-output.md) 相同。所有任务使用启动参数指定的工作目录，工具超时默认为 5 分钟，执行模式默认采用 YAML 的 `name`。

```sh
make codex-consumer
export OPENAI_API_KEY='your-key'
export CODEX_MYSQL_URL='mysql://user:password@127.0.0.1:3306/codex-go'
go run ./cmd/migrate
./bin/codex-consumer --config examples/react-agent.yaml --workspace /path/to/workspace
```

先替换示例配置中的模型名；Redis 和 MySQL 需要可连接且数据库已完成迁移。使用 `--concurrency` 设置单实例同时运行的最大任务数（默认为 1，例如 `--concurrency 4`）。并发任务共享工作目录，需确保任务之间不会互相干扰。可用 `--mode` 覆盖执行模式，`--mcp-command` 指定独立 MCP 服务，`--runtime-dir` 可重复指定工具运行时目录。

## 命令执行配置

消费者使用的 Agent YAML 可在顶层配置宿主命令发现、入口白名单和额外运行时目录：

```yaml
# 合入现有 Agent YAML，与 name、base、redis 同级。
discover_host_commands: true
exec_commands: [poetry, python3]
runtime_dirs: [/home/user/go]
```

三项分别对应 `--discover-host-commands`、可重复的 `--exec-command` 和可重复的 `--runtime-dir`。命令行显式传入的值优先，列表按整项替换，不与 YAML 追加合并；`--discover-host-commands=false` 可关闭 YAML 中开启的发现。未配置时不开启发现、使用内置入口白名单，不添加额外运行时目录。

这些设置应用于消费者创建和恢复的代理运行；默认内置工具和 `--mcp-command` 指定的外部 MCP 使用相同规则。它们属于全局配置，不写入 `base` 或 `agents`；代理还需通过 `allow_tools` 启用 `exec` 才能调用命令。完整说明见 [宿主命令与运行时配置](../README.md#宿主命令与运行时配置)。

例如，仅替换 YAML 中的命令白名单，保留其中的发现策略和运行时目录：

```sh
./bin/codex-consumer --config examples/react-agent.yaml \
  --workspace /path/to/workspace --exec-command git
```

## 提交任务

生产者使用 `RPUSH`，消费者使用 `BLPOP`，按 FIFO 消费：

```sh
redis-cli RPUSH codex-go-task-queue '{"Input":"阅读项目并说明主要模块","TaskId":"task-001"}'
redis-cli XREAD BLOCK 5000 STREAMS codex-go-resp-stream-task-001 0-0
```

`Input` 和 `TaskId` 均不能为空。`Input` 作为代理运行目标；`TaskId` 用于结果流关联，所有代理输出统一写入 `codex-go-resp-stream-{TaskId}`，不作为数据库运行 ID 或会话 ID。可选字段 `ConvId` 指定已有会话，新任务可引用该会话此前运行的结果和错误；省略时创建独立会话。例如：`{"Input":"继续上一任务","TaskId":"task-002","ConvId":"已有会话ID"}`。运行开始时，结果流中的 `task_info` 事件提供本次运行的 `run_id`、`conv_id` 和当前模型名称 `model_name`，格式见 [Redis 输出](redis-output.md)。

无效消息和任务执行失败写入 codex.log 后继续消费，成功时也记录完成日志及任务 ID。Redis 读取失败使进程报错退出，由进程管理器负责重启。`SIGINT` / `SIGTERM` 会停止领取新任务，等待已领取的任务运行结束后退出。

`BLPOP` 出队即移除消息，没有确认、自动重入队或去重；进程崩溃或运行取消时消息也不会恢复。需要恢复代理运行时，可根据结果流中 `task_info` 或 codex.log 的 `run_id` 使用 `codex-cli --run-id`，详见 README 的恢复说明。
