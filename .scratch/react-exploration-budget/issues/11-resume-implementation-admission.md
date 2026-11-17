# 11: 恢复后延续实施准入与纠正状态

**What to build:** 代理在工作区检查或调查拒绝后恢复，仍沿用原执行实例的额度、修改窗口及纠正上下文，能够继续完成实施；异常边界准确停止且不重复执行工具。

**Blocked by:** 10：违规调查的有界纠正与延期。

**Status:** ready-for-human

- [x] 在工作区检查后和调查拒绝后分别保存运行检查点并恢复；保留有效策略、阶段、探索消耗、延期次数、修改窗口及拒绝上下文，配置变化不刷新额度。
- [x] 恢复后的模型能够获取拒绝原因和剩余窗口，并基于既有证据实施；已确认执行的检查和工具调用不自动重放，拒绝的调用不伪装为已执行。
- [x] 通过同一执行实例恢复后真实补丁、验证与完成的完整场景验证行为，不仅断言序列化字段。
- [x] 窗口已耗尽时恢复不能取得新实施机会；总预算和停滞机制仍可提前终止，不额外授予业务轮或延期。
- [x] 拒绝反馈或额度变化保存失败时不得继续模型或工具执行；工具执行结果未知时沿用既有停止边界，不猜测失败或自动重跑。
- [x] 复用现有修改窗口恢复、命令计费与失败收尾测试，覆盖恢复后不重复扣减或退款、真实净变更和已有脏文件边界。
- [x] 日志提供执行实例、恢复状态、阶段、操作用途、准入或拒绝原因、额度和窗口变化；最终结果准确区分实际变更、验证结果及未完成项，不以模型调用次数证明完成。
- [x] 完成相关 ReAct 准入、计费、修改证据及恢复回归验证，遵守运行时预算约束和工具结果未知的既有 ADR 决策。


## Comments

- 2026-11-17：用户确认以 ReAct 运行入口和公开检查点作为测试边界，使用真实工作区、真实工具及模拟模型服务；沿用 ticket 8–10 已有未提交实现。
- TDD：先新增检查后恢复、调查拒绝后恢复的完整场景，复现实施阶段模型缺少剩余窗口及交接上下文、恢复日志缺少拒绝原因；再补齐上下文及日志，使测试转绿。
- 两个场景从检查点持久副本恢复原执行实例，模拟存储提交后确认丢失，验证立即停止；恢复时改变配置仍沿用原策略，模型获取剩余窗口、交接证据及拒绝反馈，完成真实补丁、`go test main.go` 和完成响应。检查不重放、拒绝不登记、探索消耗不重复计费，真实目标文件变化得到确认。
- 复用既有恢复窗口耗尽、延期策略恢复、总预算、停滞、保存失败、未知工具结果、修改证据、命令计费及失败退款回归。
- 定向恢复与停止边界测试通过；`git diff --check` 通过。
- 包级回归：`internal/mcp`、`internal/context` 通过；`internal/agent` 的失败清单与 ticket 10 记录的 9 项完全一致，未出现新增失败：`TestCompactionThresholdsMatchSpec`、`TestReactCorrectionBudgetBlocksOversizedRetry`、`TestReactUsesConfiguredCompactOutput`、`TestReactSmallPatchDoesNotExemptFurtherInvestigation`、`TestPlanExplorationModifiesVerifiesAndIsolatesSteps`、`TestReactResumeLogsOriginalExplorationPolicy`、`TestOrchestratorModeRouting`、`TestReactHistoryCompactionAllowsOneNecessaryReread`、`TestReactRunIncludesRegisteredSkillsInModelContext`。本次未重复验证这些失败在原始 HEAD 的表现，沿用 ticket 10 的基线记录。
