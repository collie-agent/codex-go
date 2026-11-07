# ReAct 上下文压缩

`glob`、`list_dir` 的原生工具调用与结果只保留在紧接着的一轮决策请求中，同轮格式纠错重试仍可使用；之后从模型视图移除，其他工具记录不受影响。恢复时最新工具批次供首次决策使用。完整执行记录不删除，仍可通过 `read_tool_result` 回读，并作为压缩和恢复的依据。

`internal/agent/compact.go` 在每次 ReAct 模型请求前检查 token 阈值（`exceedTokenThreshold`，24 × 1024）。上一请求报告的输入 token 数或当前模型上下文估算超过阈值时，启动异步压缩。估算将 ASCII 按约四字符一个 token、其他字符按一个 token 计算，不是模型 tokenizer 的精确计数。累计的格式和工具调用纠错消息也计入预算，包括其中引用的非法输出；这些消息不属于可压缩工具记录，摘要生成后仍须纳入预算复检。

压缩任务处理启动时的独立快照，覆盖快照中的全部工具批次。同一执行实例同时最多有一个压缩任务。期间上下文低于同步阈值（`syncTokenThreshold`，32 × 1024）时，ReAct 继续推理，工具调用及结果继续追加到实时上下文和完整执行记录。达到或超过同步阈值时，在下一次模型请求前等待已有压缩任务完成；应用摘要后清除旧 usage 并重新估算。若仍达到同步阈值，则继续压缩并等待，最多应用 3 次摘要，仍超限返回错误，禁止继续推理。每个后台压缩任务限时 2 分钟，等待支持运行上下文取消及截止时间；压缩失败直接返回错误。

压缩结果就绪后，在下一次 ReAct 模型请求前由主循环写入检查点，并切换为摘要 + 所有未被摘要覆盖的完整工具交互。后台压缩期间追加的所有批次都会保留，新增记录在下一次压缩中纳入摘要。

每次生成摘要后复检快照模型视图的预算，最多尝试 3 轮。失败在下一次模型请求前返回错误，保留原检查点。运行结束时取消并回收后台任务，不等待其完成摘要。成功切换后清除旧视图的 usage，避免旧 token 数再次触发压缩。

压缩请求只包含旧摘要、上次覆盖批次之后到本次截止批次的新增原始记录，以及当前目标、验收标准和当前步骤。例如，旧摘要覆盖到第 3 批、本次截止第 6 批，只发送旧摘要和第 4～6 批原文。压缩模型通过 `agents.compact.llm` 独立配置，省略时继承 `base.llm`。`output_schema` 选择严格 JSON 结构化输出或行协议，系统提示词先加入对应 JSON／行协议的公共格式规则及 few-shot，再加入 compact 的字段说明和专用 few-shot，不提供工具。输出沿用 `ExecutionSummary`：Facts、Actions、OpenIssues 中的每项证据必须有 Text 和 SourceCallIDs，且引用已覆盖的真实调用；Pending 仅作为剩余工作的参考。

新摘要和 ThroughBatch 保存成功后，后续推理请求使用新摘要及 ThroughBatch 之后的完整工具记录。压缩或保存失败时停止本次执行，保留已有摘要和完整执行记录。

## 同步等待日志

达到同步阈值且压缩结果尚未应用时，日志通过 `[compact] sync wait` 标识等待过程。每条日志包含 `task`（运行 ID）和 `execution_id`（执行实例 ID）。

| 事件 | 字段及含义 |
| --- | --- |
| `sync wait started` | `tokens` 为当前估算与上一请求报告输入 token 数的较大值；`threshold` 为同步阈值；`applied_attempts` 为本次模型请求前已应用摘要的次数，从 0 开始。 |
| `sync wait completed` | 已收到成功的后台压缩结果；`duration` 为本次等待耗时。 |
| `sync wait failed` | 后台压缩失败，包括压缩任务超时；`duration` 为等待耗时，`error` 为失败原因。 |
| `sync wait cancelled` | 运行上下文被取消或达到截止时间；`duration` 为等待耗时，`error` 为上下文错误。 |

例如，以下日志表示达到 32K 同步阈值后等待压缩结果：

```text
task compact-run: [compact] sync wait started execution_id="compact-instance" tokens=34000 threshold=32768 applied_attempts=0
task compact-run: [compact] sync wait completed execution_id="compact-instance" duration=1.2s
task compact-run: [compact] applied execution_id="compact-instance" through_batch=11
```

`sync wait completed` 只表示结果已返回，之后仍需保存检查点、应用摘要并复检预算；摘要成功应用以 `[compact] applied` 为准。若应用后仍达到同步阈值，可能再次出现等待日志。结果在检查时已经就绪则直接处理，不产生同步等待日志。失败或取消时不会继续发送下一次推理请求。

## 回读原文

每个 ReAct 执行实例提供本地工具 `read_tool_result`，不依赖 MCP 服务端保存执行记录：

```json
{"call_id":"call_18","offset":0,"limit":4000}
```

返回 call_id、tool_name、status、arguments、content、next_offset 和 has_more；原调用有错误时附带 error。content 是执行记录中的原始结果片段，offset 和 limit 按 Unicode 字符计算，避免截断中文。offset 从 0 开始，limit 范围为 1～65536；超过末尾返回空片段。不存在的 call_id 或非法参数作为工具错误反馈。回读只限当前执行实例，可以重复读取，不会重新执行原工具。

## 恢复

摘要是完整执行记录的派生视图，不能替代执行记录。检查点同时保留完整 ToolExecutions 和 ExecutionCompaction；摘要按 ExecutionID 隔离，恢复沿用覆盖批次，新计划步骤清空前一实例摘要。即使未知执行结果已经位于摘要覆盖范围内，也必须停止自动恢复。

数据库新增 `execution_instance.compaction` JSON 列。已有数据库在运行新版程序前执行仓库迁移命令：

```sh
go run ./cmd/migrate
```

## 配置压缩模型

与其他模型调用一样，`llm` 整块替换，独立配置时需要提供模型及凭据来源；`base_url` 和 `stream` 等字段遵循现有 LLM 配置规则。

```yaml
agents:
  compact:
    llm:
      api_key_env: OPENAI_API_KEY
      model: your-compact-model
      output_schema: line_protocol_llm_output # 或 json_llm_output
      stream: false
    sys_prompt: |
      保留与当前目标有关的关键证据。
```

`sys_prompt` 追加到内置压缩指令，省略时继承 base 的提示词。JSON 返回 Facts、Actions、OpenIssues、Pending 数组；行协议用 FACTS、ACTIONS、OPENISSUES 子块表示证据，子块内通过 TEXT 和重复 SOURCECALLIDS 行关联原调用，PENDING 使用重复字符串行。行协议省略空列表。

## 输出格式纠错

压缩模型的 JSON 或行协议输出解析失败时，保留原始上下文，将解析错误和非法输出作为独立的 `role: user` 消息追加到末尾，再次请求模型。后续重试累计保留此前纠错消息，最多允许 3 次纠错；第 4 次格式错误停止执行，不提交新摘要。网络/API 错误、意外工具调用和证据校验失败仍直接返回错误。
