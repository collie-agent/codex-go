# 03: 计划执行全过程保存步骤检查点

> 本任务单属于旧版记录；本轮恢复语义以 [当前规格](../spec.md) 为准，不按本文件中的从头重跑规则实施。

**What to build:** 用户启动计划执行任务后，完整计划及其业务顺序在工具执行前落库；每步执行的开始、结果和完成证据都形成持久化检查点，数据库写入失败时不会继续执行后续工具。本任务交付正常计划执行的持久化路径，恢复由任务 04 交付。

**Blocked by:** 01 — 新建 ReAct 运行并持久化完整生命周期。

**Status:** ready-for-human

- [x] 扩展显式迁移命令以创建 step_execution；重复迁移保持 conversation、agent_run 及其已有数据不变。
- [x] step_execution 包含 step_id、run_id、status、intent、completion_criteria、completion_evidence、observation、position、revision、create_at、update_at，不额外引入软删除字段。
- [x] (run_id, step_id) 唯一，run_id 引用 agent_run；不同 run 可以使用相同的 StepId。
- [x] position 明确表示计划顺序并随计划落库，不能依赖 create_at、插入顺序或 StepId 的文本排序；保留现有 revision 信息。
- [x] 运行级 completion_criteria 是字符串列表，步骤级 completion_criteria 是单个字符串；步骤的 completion_evidence 和 observation 以 JSON 列表保存。
- [x] TaskAnalyzer 产生的验收标准及确定的 mode 在继续执行前落库；已有验收标准和确定的模式不能因重新进入处理器而被无故覆盖。
- [x] Planner 生成计划后一次性保存完整有序步骤，再允许执行工具；计划保存失败时不能执行任何计划步骤。
- [x] 每步执行前保存 running；成功后保存 completed、Observation 和 CompletionEvidence，失败后保存 failed 及运行错误。
- [x] 同一个 checkpoint 中的步骤变更、运行状态及对应完整结构化执行结果使用同一事务提交；每步完成时更新 agent_run.result，不能只在整个运行结束时保存结果。
- [x] 任一 checkpoint 写入失败时立即阻止后续工具调度；不能出现步骤已提交而对应结果未提交的部分保存。
- [x] 两个 agent 入口复用相同持久化运行能力，agent-server 的显式及自动模式选择继续符合现有行为；普通 ReAct 路径仍不产生步骤记录。
- [x] 验证多步执行的开始/成功/失败检查点；在相同 create_at 下仍可按 position 还原原计划；通过写入故障验证事务回滚和后续工具不再执行，并更新计划检查点的使用说明。
