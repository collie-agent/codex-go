# codex-server

先按 `docs/redis-consumer.md` 配置 Redis、MySQL 和数据库迁移，启动消费者后运行：

```sh
export CODEX_MYSQL_URL='mysql://user:password@127.0.0.1:3306/codex-go'
go run ./cmd/codex-server --redis-url redis://127.0.0.1:6379/0
```

`--addr` 默认为 `:8080`，`--flush-interval` 默认为 `1s`。

```sh
curl -X POST localhost:8080/tasks -H 'Content-Type: application/json' -d '{"input":"说明项目结构"}'
curl -N 'localhost:8080/tasks/TASK_ID/events'
curl -N 'localhost:8080/tasks/TASK_ID/events?last_event_id=EVENT_ID'
```

新建任务返回 `201` 和 `{"task_id":"..."}`。事件以 SSE `id` 和 JSON `data` 返回。省略 `last_event_id` 时从流开始回放，再等待新事件；也支持 `Last-Event-ID` 请求头。服务按间隔将本连接返回的事件批量写入数据库，连接结束时也会尝试写入剩余事件。数据库写入失败会结束连接；客户端可用最后收到的事件 ID 重连。
