# Checkpoint / Resume

Status: ready-for-agent

## 已确认约束

- 持久化包含 `conversation`、`agent_run` 和 `step_execution` 三张表。
- `conversation` 包含 `conv_id`。
- `agent_run` 包含 `run_id`、`conv_id`、`goal`、`status`、`result`、`error`。
- `step_execution` 包含 `step_id`、`run_id`、`status`、`intent`、`completion_criteria`、`completion_evidence`、`observation`。
- 每张表均包含 `create_at`、`update_at`。
- 普通 ReAct 模式不产生 `step_execution` 记录。

## 当前代码事实

- `internal/database/database.go` 提供 GORM / MySQL 连接；尚未定义业务表与迁移。
- `internal/context/context.go` 的 `DurableState` 仅驻留内存，包含运行目标、步骤列表及当前步骤索引。
- Plan 模式执行前设置步骤为 running，成功后设置 completed 并记录观察与完成证据；执行失败设置 failed。
- Planner 保留 completed 步骤并选择 pending 步骤，目前拒绝 running、failed、interrupted 步骤。
- 普通 ReAct 的模型与工具上下文仅驻留内存。

## 设计状态

用户已确认完整规格，包括保留 position 作为步骤的业务顺序。当前进入任务拆分，任务拆分方案确认后发布到本地 issue tracker。

## 第一轮已确认决策

- 首版支持进程崩溃后恢复及执行失败后手动重试；暂不提供主动暂停。
- 恢复通过显式操作触发，服务启动不自动执行遗留任务。
- 一个 conversation 包含多个 run；新任务创建新 run，resume 保留 run_id 和 goal。
- conversation 首版仅表示归属，不默认共享历史消息。
- 计划执行恢复时跳过 completed 步骤，未完成步骤从头执行。
- 普通 ReAct 恢复原 run，从原 goal 重新开始，不要求还原模型每轮消息。
- 外部操作与 checkpoint 之间存在结果不确定窗口；工具自行保证幂等，恢复流程不增加人工确认。

## 第二轮已确认决策

- run 状态采用 pending / running / completed / failed。允许显式恢复崩溃遗留的 running 及 failed；completed 的 resume 直接返回保存的 Result。
- 恢复未完成步骤时重置为 pending；保留 completed 步骤及其证据。
- 执行工具前保存 run 和完整计划，每步开始前保存 running，每步结束后保存结果，运行结束保存 Result 或 error。
- checkpoint 写入失败时停止继续执行；同一 checkpoint 的运行状态与步骤变更通过事务提交。
- CLI 和 agent-server 均提供按 run_id 恢复的入口，新任务返回 run_id 和 conv_id。
- 新任务可指定已有 conv_id；未指定时创建新会话。
- resume 使用原 goal，拒绝通过恢复请求修改目标。
- 同一 run 在多个进程间也只允许一个执行者；通过数据库保证互斥，崩溃后可以重新取得执行权。

## 第三轮已确认决策

- `agent_run` 补充 mode、运行级 completion_criteria、workspace。
- `step_execution` 补充 position、revision；以 (run_id, step_id) 唯一标识步骤，不要求 StepId 全局唯一。
- position 明确保留，不以 create_at 代替计划顺序；批量创建的步骤可能具有相同时间戳。
- 恢复使用调用时提供的当前 YAML、模型和工具配置，不保存凭据或整份配置快照。
- 恢复必须使用原 workspace，不能改变已确定的执行模式；已有计划和验收标准保持不变。
- 两个命令的 agent 运行默认持久化，必须配置 CODEX_MYSQL_URL；数据库不可用则报错。纯 MCP 服务入口保持原行为。
- 通过独立的显式迁移命令建表，不在 agent 运行时自动迁移。
- 数据库列名统一 snake_case，时间字段明确使用 create_at、update_at；不额外引入软删除字段。
- stdout 保留最终答案输出；run 创建成功、执行开始前向 stderr 输出 run_id 和 conv_id。
- 用户明确拒绝数据库租约方案：不采用 lease_owner / lease_expires_at / lease_version 及租期、续租机制。跨进程互斥要求仍保留，由第四轮确认的连接锁实现。

## 第四轮已确认决策

- 使用按 run_id 命名的 MySQL GET_LOCK，由专用连接持有至运行结束；锁已占用时立即拒绝另一执行请求。
- checkpoint 使用持锁连接写入；连接失效时停止执行。正常结束显式释放锁，崩溃后由数据库在连接断开时释放锁。
- 每步完成时，将步骤结果和完整结构化执行结果放入同一事务保存。
- agent_run.result 保存 JSON，保留 Answer、Observation、CompletionEvidence，而非只保存答案文本。
- 计划步骤全部 completed、run 尚未 completed 时，恢复直接使用保存的执行结果完成 run，不再调用模型或工具。

## 最终表结构

以下为数据库列名；JSON 列保存结构化结果或列表。运行级与步骤级 completion_criteria 分别对应现有代码的字符串列表与单个字符串。

| 表 | 字段 | 约束及说明 |
| --- | --- | --- |
| conversation | conv_id, create_at, update_at | conv_id 为主键 |
| agent_run | run_id, conv_id, goal, status, result, error, mode, completion_criteria, workspace, create_at, update_at | run_id 为主键；conv_id 引用 conversation；result 和 completion_criteria 为 JSON |
| step_execution | step_id, run_id, status, intent, completion_criteria, completion_evidence, observation, position, revision, create_at, update_at | (run_id, step_id) 唯一；run_id 引用 agent_run；按 position 恢复顺序；completion_evidence 和 observation 为 JSON |

不额外创建消息历史表、租约表或租约字段；普通 ReAct 不写 step_execution。

## 执行与恢复不变量

- 新任务先创建 conversation（未指定时）及 pending run，输出 ID，再开始执行。
- 运行开始进入 running；成功进入 completed，失败进入 failed。崩溃可能保留 pending 或 running，显式恢复继续这些尚未完成的运行。
- TaskAnalyzer 已产生的验收标准和已确定模式需要持久化；计划生成后完整保存，执行工具不能早于对应 checkpoint 成功。
- 已有计划恢复时不重新规划；根据 position 重建步骤顺序，由第一个 pending 步骤继续，当前步骤索引无需单独保存。
- running / failed / interrupted 步骤恢复为 pending；completed 步骤及其观察、证据保留。
- 普通 ReAct 不恢复逐轮模型消息和工具结果，直接围绕原目标重新执行。
- 完成步骤与对应完整执行结果原子保存；全步骤完成后的最终收尾不再次执行模型或工具。
- run 已 completed 时直接返回持久化结果，不启动模型或工具。
- 数据库连接故障或 checkpoint 失败后不能继续调度新工具；外部副作用重复执行的幂等由工具负责。
- resume 不能修改原 goal、workspace、已确定的 mode、已有计划或验收标准；当前 YAML 和工具设置由恢复调用提供。

## 验收场景

- 新任务关联已有会话或创建新会话；运行前输出可用于恢复的 ID。
- 计划按顺序持久化，恢复跳过 completed 步骤，仅重新执行未完成步骤。
- failed run 可手动重试；崩溃留下的 pending / running run 可显式恢复。
- 普通 ReAct 始终不产生步骤记录，恢复后保留原 run_id 和 goal。
- 最后一步完成后、run 最终完成前中断，恢复不调用模型或工具，返回已保存的完整结果。
- completed run 重复恢复返回原结果。
- 同一 run 跨进程竞争时仅一方获得执行权；持锁连接断开后可重新获得锁；不同 run 可独立执行。
- 模拟 checkpoint 写入失败，确认不再调度后续工具；事务不能部分保存运行和步骤变更。
- 恢复请求修改 goal / workspace / 已确定 mode 被拒绝；允许使用更新后的配置。
- 迁移命令独立运行且可重复执行；缺少数据库配置、数据库不可用或未迁移时 agent 运行明确报错。
- 验证原有 agent 流程及纯 MCP 服务入口未受回归影响。

## 访谈约定

先逐轮确认设计；在设计树收敛并由用户确认共同理解后开始实现。确定的术语及时进入根 GLOSSARY.md，满足条件的架构取舍进入 docs/adr/。
