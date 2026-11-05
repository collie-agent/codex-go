# 任务输出接续

运行输出只写入 Redis Stream，不写入 MySQL；恢复会获得新的队列 `task_id`，但沿用原运行和代理参与实例。服务端不提供运行事件历史查询或运行级 SSE 接口。

实时输出使用 `/tasks/{task_id}/events`，其 SSE `id` 为 Redis Stream ID，支持 `last_event_id` 查询参数或 `Last-Event-ID` 请求头。重连时传入最后成功处理的 SSE `id`：

```sh
curl -N -H 'Last-Event-ID: REDIS_EVENT_ID' 'http://localhost:8080/tasks/TASK_ID/events'
```

任务流使用 `type/delta` 协议。事件顺序和断线续传游标使用 Redis Stream entry ID。Redis 流受保留时间限制，过期后无法通过此接口回放。

恢复运行后需要使用新任务的 `task_id` 订阅，原任务事件不会复制到新任务流；新任务的 Redis 游标需独立维护。

代理参与实例按 `agent_instance_id` 建立，父节点由 `parent_agent_instance_id` 指定，`agent_name` 只用于展示。同名独立委派有不同身份，同一委派的多轮执行和恢复共用身份。

客户端只根据生命周期事件更新状态。缺少终态时保持最后已知状态或显示未知，不能因连接断开、运行结束或父代理完成而推断子代理成功。恢复时原实例可从 `failed` 再进入 `pending/running`；已经完成的实例不会重新执行或重发模型输出。运行汇总事件没有代理身份时，不归属给最后输出的子代理。

完整生命周期与终态语义见[代理生命周期协议](agent-lifecycle.md)。正文增量属于各轮临时输出，应保留原始顺序，不能将不同轮次的所有片段拼成一个已确认答案。

会话历史接口 `GET /conversations/{conv_id}/runs` 的 `runs[].agents` 提供持久化的参与实例层级、确认回复、逐轮结果和工具快照，可使用与 SSE 一致的参与实例 ID 合并展示。该快照不是事件回放，无确认终态的参与实例返回 `unknown`；不要用它覆盖客户端已收到的明确生命周期状态。字段及顺序详见 [HTTP 接入文档](codex-server.md#多代理历史视图)。
