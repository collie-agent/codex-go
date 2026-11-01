# Checkpoint / Resume 历史规格与访谈

仅用于追溯；当前契约以 [spec.md](spec.md) 为准。

# Checkpoint / Resume

Status: needs-info

## ReAct resume 修订访谈（2026-10-30）

以下是本轮用户提出的新需求；下文保留旧版规格作为历史，冲突内容待本轮访谈确认后统一修订，不能直接按旧版实现。

- ReAct 持久化原目标、完成标准、run_id、mode、模型工具调用及对应结果（含调用 ID 和顺序）、最终答案和状态。
- 工具记录通过 run_id 关联运行；具体表结构待确认。
- 模型请求期间尚未保存调用时，恢复重新请求模型；最终结果已保存时直接返回。
- 已保存工具结果时，恢复本次运行的持久化上下文，并加载会话内此前运行的结果或错误。
- 用户主动 interrupt 后重新输入，创建新任务；新任务上下文包含此前 completed 运行的 result/error、interrupted 运行的目标、完成标准及工具交互，以及当前输入。
- 待决策：主动中断与失败/崩溃的边界、工具结果未知窗口、工具记录表含义、历史继承范围与顺序。

新需求与 ADR-0003 及旧版“普通 ReAct 从原目标重新开始”的定义冲突；本轮将重新审视该取舍。

### 修订第一轮已确认决策

- 保留 failed，新增 interrupted 表示用户主动停止；崩溃可能留下 running。失败与崩溃允许恢复；interrupted 不允许 resume，下一次输入创建新 run。
- 工具执行前登记调用，执行后保存结果；恢复时缺少结果的调用视为结果未知，停止自动执行，不自动重放。
- 使用共享 tool_execution 表，通过 run_id 关联运行；显式保存调用顺序，不用调用 ID 排序。
- 新任务与 resume 使用同一历史规则：按运行顺序加载此前 completed 的 result/error、interrupted 的目标/完成标准/工具交互、failed 的 error。resume 再加入当前 run 已保存的内容。
- 每条运行记录只保存自身产生的内容，不复制继承的历史。
- 本轮确认尚不代表完整设计收敛；继续确认实现范围、中断入口、未知结果处理和历史消息表达。

### 修订第二轮已确认决策

- Q5 用户指出计划执行 executor、多代理与普通模式使用同一套 ReAct 代码；不采纳仅普通 ReAct 的范围建议，具体执行实例隔离和恢复边界继续核实。
- 支持 CLI Ctrl-C 及服务端按 run_id 显式中断。请求断开、超时、服务退出不自动视为用户主动中断。
- 主动中断停止调度新工具，取消正在执行的工具并尽力保存已有结果；取消不表示副作用已撤销。
- 执行前原子登记整批工具调用，保存批次顺序及批内顺序；各工具完成后分别保存结果，按模型调用顺序恢复上下文。
- 结果未知时返回明确错误及调用 ID；首版不提供标记成功或强制重试接口，该运行在未知结果消除前无法 resume。
- 用户可以开启新任务核实未知结果；failed 的 error 包含未知调用名称、参数及 ID。
- 此前 interrupted 运行的工具交互作为历史参考文本注入，只有当前 resume 的运行恢复原生工具调用/结果消息；历史明确旧目标已中断，当前输入决定新目标。缺失结果标记未知，不伪造工具结果。
- 首版完整加载符合条件的历史，不自动摘要或截断；超过模型上下文限制时报错。

### 修订第三轮已确认决策

- 普通 ReAct、plan-execute executor、多代理均支持恢复；多代理同时持久化委派关系、执行实例、子任务结果和调度位置，避免重新委派已完成任务。
- 每个独立 ReAct 上下文具有稳定 execution_id，工具记录同时关联根 run_id 与 execution_id。计划步骤关联执行实例；多代理同一节点的多次独立调用分别关联执行实例。
- 恢复沿用 execution_id，新上下文创建新 execution_id，不混合不同步骤或 worker 的工具历史。
- 计划恢复跳过 completed 步骤，未完成步骤恢复自身 ReAct 历史继续执行，取代旧版从头执行未完成步骤的规则。
- 普通 ReAct 升级计划执行后，升级前历史作为交接背景，不重复注入各步骤的原生工具消息。
- 下文旧版的表结构、恢复不变量与验收场景尚待设计收敛后统一改写；与修订决策冲突时以修订决策为准。

### 修订第四轮已确认决策

- 原子保存节点输出及子委派安排后再启动子任务。恢复复用已保存的子结果，仅续接未完成实例；子任务齐备后父节点创建新的执行实例处理报告。
- 子节点业务失败交父节点处理；基础设施故障停止整棵运行并保留已确认结果。根节点业务失败保存为 failed，记录失败类型。
- 已有终态业务失败结果时 resume 直接返回保存结果；没有终态结果的执行故障才继续恢复。重新尝试终态业务失败需创建新任务。
- 用户明确排除 Q16、Q17：本次不设计恢复配置变化的兼容性机制，不增加执行预算、轮数和纠错次数的持久化或恢复策略。
- 主动中断作用于整个根 run，包含所有计划步骤或多代理子任务；首版不支持单独中断 worker。已完成实例保留结果，未完成实例保存可确认进度，根运行标记 interrupted，后续输入创建新任务。

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

## 修订第五轮最终回答

- Q19：用户明确不考虑历史数据兼容，不采用旧运行专用拒绝恢复建议。
- Q20：当前执行实例完整保留工具调用及结果，正常运行与恢复采用一致的历史内容。
- Q21：interrupted 历史按步骤或委派组织，包含实例归属、子任务目标、标准、结果及工具交互，仅作为新任务参考。
- 当前规格已统一整理到 spec.md，等待用户确认共同理解后实施。
