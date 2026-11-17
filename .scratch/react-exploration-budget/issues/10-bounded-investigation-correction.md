# 10: 违规调查的有界纠正与延期

**What to build:** 实施阶段误选无额度调查时，代理收到明确拒绝反馈，在剩余修改窗口内改为实施或申请有限探索延期，而不是立即结束整个代理运行；连续违规仍有确定上界。

**Blocked by:** 09：实施阶段允许工作区检查并完成修改。

**Status:** ready-for-human

- [x] 无探索额度的调查或含调查的混合批次整批拒绝，工具不执行，不登记为已执行调用，也不增加探索消耗。
- [x] 每个被拒绝的业务决策轮消耗一轮修改窗口，反馈拒绝原因、可用操作和剩余窗口；有余量时允许下一轮纠正，不增加独立纠正额度。
- [x] 策略拒绝不视为已执行工具明确失败，不触发现有失败退款；明确失败修改操作的既有结算规则保持不变。
- [x] 模型纠正为真实补丁后可继续验证完成；连续违规至窗口耗尽仍无可确认目标修改时失败收尾，保留可信进度。
- [x] 复用现有阶段决策协议申请探索延期，校验具体缺口、必要字段、次数和总预算；未获批不放行调查，获批不刷新同一待实施修改的窗口。
- [x] 重复实施交接、模型用途声明和延期申请均不能绕过窗口或总预算；阶段决策沿用现有计费语义。
- [x] 状态变化及拒绝反馈在继续模型或工具执行前持久化，保存失败不得继续执行；日志明确区分操作拒绝、可继续纠正与最终结束。
- [x] 在现有 ReAct 运行入口验证拒绝调查→补丁→验证→完成、连续拒绝耗尽、混合批次完全不执行、有效延期与次数耗尽拒绝；通过工具实际执行和文件内容断言行为。


## Comments

- 2026-11-17：用户确认测试边界为现有 ReAct `Run` 入口：模拟模型响应，使用临时工作区和真实工具，通过文件内容、执行记录、模型反馈和检查点验证。
- TDD：先复现无额度调查直接终止、延期拒绝后陷入独立阶段纠错、纠正成功后残留过期拒绝，以及混合 `unable` 决策被拒绝却仍结束运行，再逐项实现并验证。
- 调查拒绝不创建执行或计费记录，不扣探索、不进入工具失败退款；每个拒绝轮扣一个修改窗口，持久化拒绝原因、可用操作和余量后才继续模型。保存失败立即停止。
- 实施窗口内的无效延期、重复交接和夹带工具的决策使用同一窗口；有效延期沿用现有字段、次数和总预算检查，不刷新窗口。实施窗口外保持既有阶段纠错语义及混合响应兼容行为。
- 新增 9 个 ReAct 入口测试，覆盖拒绝后补丁→真实 Go 验证→完成、混合批次完全不执行、连续拒绝、有效及耗尽延期、缺失字段、重复交接、保存失败、已执行失败补丁退款、混合阶段决策、总预算和可信既有进度保留。相关旧断言更新为允许纠正后的失败边界。
- 新增测试全部通过；工作区检查、交接及多代理混合决策相关回归通过；`git diff --check` 通过。
- 包级回归：MCP 全包通过；agent 仍有 ticket 09 已记录并在原始 HEAD 复现的同 9 项既有失败，未出现新增失败。失败清单：`TestCompactionThresholdsMatchSpec`、`TestReactCorrectionBudgetBlocksOversizedRetry`、`TestReactUsesConfiguredCompactOutput`、`TestReactSmallPatchDoesNotExemptFurtherInvestigation`、`TestPlanExplorationModifiesVerifiesAndIsolatesSteps`、`TestReactResumeLogsOriginalExplorationPolicy`、`TestOrchestratorModeRouting`、`TestReactHistoryCompactionAllowsOneNecessaryReread`、`TestReactRunIncludesRegisteredSkillsInModelContext`。实现完成，移交人工复核。
