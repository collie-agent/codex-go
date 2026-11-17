# 09: 实施阶段允许工作区检查并完成修改

**What to build:** 探索预算耗尽后，代理在实施交接的修改窗口内仍可检查工作区状态，随后执行真实目标修改、验证并完成任务，不因一次必要检查提前终止。

**Blocked by:** None (can start immediately).

**Status:** ready-for-agent

- [x] 复用现有阶段准入、命令用途分类和批次计费机制；实施阶段探索额度为零本身不导致运行结束。
- [x] 对 git status 及明确支持的参数采用有限白名单；工作区检查不消耗探索额度，但消耗一轮修改窗口，同一业务轮不重复扣减。
- [x] 未知参数、复合命令和脚本保守视为调查，不依据模型声明或 exec 工具名称豁免。
- [x] 保留既有 git diff 分类边界：已确认目标变更的精确路径结果核验可沿用验证规则，全仓、历史或无法识别的 diff 不普遍豁免。
- [x] 批次包含调查时沿用调查优先规则；探索额度不足则整批不执行，不能部分执行其中的修改或验证。有额度时沿用单批次一次调查计费。
- [x] 工作区检查、退出码为零、既有未提交改动均不能证明本次目标修改；继续通过目标文件批次前后实际变化确认修改。
- [x] 复用 ReAct 运行入口、模拟模型响应、临时工作目录和真实工具，验证探索耗尽→实施交接→git status→真实补丁→验证→完成；断言实际文件变化和工具执行结果。
- [x] 补充支持与不支持的命令参数、混合批次及只检查不修改的场景；只检查直到窗口耗尽应准确失败，非 Git 工作目录的检查失败不能被误报为修改成功。
- [x] 关键准入与计费处添加说明约束目的的注释；日志区分用途、准入结果、探索额度及修改窗口余量。



## Comments

- 2026-11-17：用户确认测试边界为 ReAct `Run` 入口与 `CommandPurpose` 命令分类接口。模型 API 使用可控响应，工作目录、Git、补丁和 Go 测试均真实执行。
- TDD：先复现探索耗尽后 `git status --short` 被调查门禁拒绝；增加 `workspace_check` 有限白名单及批次用途聚合后，继续复现真实补丁之后验证工具被隐藏；修正已确认修改后的验证准入，保持首次阶段决策和待纠错轮禁用工具。扩大参数白名单前先运行对应失败测试，再添加有限显示选项。
- `workspace_check` 不消耗探索额度，复用既有每业务批次一次修改窗口计费。混合批次保持调查优先、额度不足整批不执行；有额度时多次调查只计一轮。未知选项、脚本、复合命令及无法识别的 diff 保守分类不变。新增准入日志包含用途、允许／拒绝、探索消耗和窗口余量。
- 覆盖探索耗尽→交接→工作区检查→真实补丁→真实 Go 测试→完成；多检查同批次只扣一轮；既有脏文件及非 Git 检查失败不冒充目标修改；仅检查直至窗口耗尽准确失败；混合批次拒绝与单次计费；目标精确 diff 核验与全仓／历史／未知目标 diff 拒绝。
- 相关验证：`go test ./internal/mcp -run '^TestCommandPurpose' -count=1` 通过；新增工作区检查、耗尽后完成、精确 diff 及既有命令／进程会话分类回归通过；`git diff --check` 通过。
- 包级回归：`go test ./internal/agent ./internal/mcp -count=1` 中 MCP 全包通过，agent 的 9 项既有失败已在独立未修改的 HEAD（`8c308eb`）工作树逐项复现：`TestCompactionThresholdsMatchSpec`、`TestReactCorrectionBudgetBlocksOversizedRetry`、`TestReactUsesConfiguredCompactOutput`、`TestReactSmallPatchDoesNotExemptFurtherInvestigation`、`TestPlanExplorationModifiesVerifiesAndIsolatesSteps`、`TestReactResumeLogsOriginalExplorationPolicy`、`TestOrchestratorModeRouting`、`TestReactHistoryCompactionAllowsOneNecessaryReread`、`TestReactRunIncludesRegisteredSkillsInModelContext`。未扩大本工单范围处理这些失败。
