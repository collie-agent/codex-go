# 恢复代理视图与接续输出

运行的输出按 `run_id` 保存；恢复会获得新的队列 `task_id`，但沿用原运行和代理参与实例。实例按 `agent_instance_id` 建立，父节点由 `parent_agent_instance_id` 指定，`agent_name` 只用于展示。同名独立委派有不同身份，同一委派的多轮执行和恢复共用身份。

## 持久历史与运行级 SSE

历史快照：

```sh
curl 'http://localhost:8080/conversations/CONV_ID/runs/RUN_ID/events?after_sequence=0'
```

返回 `{"events":[...]}`，按运行内 `sequence` 升序排列，游标为排他的。取最后一条事件的 `sequence`，即可接续订阅：

```sh
curl -N 'http://localhost:8080/conversations/CONV_ID/runs/RUN_ID/events/stream?after_sequence=42'
```

运行级 SSE 从持久历史补齐序号大于 42 的所有事件，再定期读取新提交的事件。快照读取和订阅建立之间产生的事件也会被补齐。无须另订阅 Redis；可以省略快照步骤，直接从序号 0 订阅，完成全部回放和接续。无人在线、实时推送失败及 Redis 流过期均不影响此接口。

```text
id: 43
data: {"event_id":"stable-event-id","sequence":43,"run_id":"RUN_ID","agent_instance_id":"RUN_ID/delegation-2","parent_agent_instance_id":"RUN_ID/delegation-1","agent_name":"worker","task_id":"new-task-id","kind":"agent_lifecycle","data":{"status":"running"}}

```

- `data` 与历史接口的 `OutputEvent` 相同：`kind` 是事件类型，`data` 为 JSON 对象，包含正文、工具交互、生命周期及其他持久事件。
- SSE `id` 是运行内十进制序号。重连可传 `Last-Event-ID: 43`；若同时提供 `after_sequence`，查询参数优先。游标必须与 URL 中的运行对应。
- `event_id` 跨历史、运行级 SSE 和任务级 SSE 保持一致，可用于重复投递去重。`task_id` 仅用于队列路由，恢复前后可能不同。
- 运行级 SSE 的读取周期使用服务器的 `--flush-interval`；此接口直接读取已提交历史，不再次写入事件。连接保持至客户端断开或读取失败，已完成运行也可以回放。
- 错误在发送 SSE 响应头前返回：游标非法为 `400`，运行不存在或不属于指定会话为 `404`，历史读取失败为 `500`，历史能力未配置为 `503`。已建立连接后的读取失败会断开连接，客户端按已处理游标重连。
- `/tasks/{task_id}/events` 仍使用 Redis ID 和 `type/delta` 协议。两种接口的 SSE `id` 不可混用；任务流过滤了部分历史事件，不能用它的最大序号代替完整历史的消费游标。

## 消费示例

下面展示单个运行的内存视图。刷新后从 0 回放；若要跨刷新保存游标，需将视图与游标一起可靠保存，避免保存游标后丢失对应内容。

```js
const runURL = `/conversations/${convID}/runs/${runID}/events`;
const agents = new Map();
const seen = new Set();
let cursor = 0;

function apply(event) {
  if (seen.has(event.event_id)) return;
  if (event.agent_instance_id) {
    let agent = agents.get(event.agent_instance_id);
    if (!agent) {
      agent = { name: event.agent_name, parent: event.parent_agent_instance_id,
                status: "unknown", events: [] };
      agents.set(event.agent_instance_id, agent);
    }
    agent.events.push(event); // 保留完整正文增量、工具事件及轮次信息。
    if (event.kind === "agent_lifecycle") agent.status = event.data.status;
  }
  // 只有视图更新成功后才提交该事件与消费位置。
  seen.add(event.event_id);
  cursor = event.sequence;
}

const response = await fetch(`${runURL}?after_sequence=${cursor}`);
if (!response.ok) throw new Error(`history: ${response.status}`);
for (const event of (await response.json()).events) apply(event);

function connect() {
  const stream = new EventSource(`${runURL}/stream?after_sequence=${cursor}`);
  stream.onmessage = message => {
    try {
      apply(JSON.parse(message.data));
    } catch (error) {
      stream.close(); // 处理失败时保留游标，由调用方处理错误后重连。
      console.error(error);
    }
  };
  stream.onerror = () => {
    stream.close();
    setTimeout(connect, 1000); // 每次使用最新的已处理游标。
  };
}
connect();
```

客户端只根据生命周期事件更新状态。缺少终态时保持最后已知状态或显示未知，不能因连接断开、运行结束或父代理完成而推断子代理成功。恢复时原实例可从 `failed` 再进入 `pending/running`；已经完成的实例直接使用历史，不会重新执行或重发模型输出。运行汇总事件没有代理身份时，不归属给最后输出的子代理。

完整生命周期与终态语义见[代理生命周期协议](agent-lifecycle.md)。正文增量属于各轮临时输出，应保留原始顺序，不能将不同轮次的所有片段拼成一个已确认答案。
