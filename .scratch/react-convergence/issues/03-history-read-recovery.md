# 03: 历史结果回读纳入停滞恢复

**What to build:** 反复回读同一历史结果不再耗尽整个执行预算，同时保留真实分页和上下文压缩后必要回读的能力。

**Blocked by:** 01：文件重复读取触发有界恢复

**Status:** resolved

- [x] 历史回读工具提供源 call_id、实际返回字符范围及 EOF 信息，按源结果与覆盖范围生成批次证据，不能使用新回读调用的 call_id 伪装新增证据。
- [x] 同页重复回读和 EOF 后重复读取累计停滞，新的有效分页允许继续。
- [x] 结合上下文压缩代次允许有界的必要回读；同代次重复回读不能持续清零停滞，压缩也不能重置总体预算。
- [x] 重复文件读取与历史回读混合成批仍触发恢复；批次判断在下一轮模型决策前同步完成。
- [x] 端到端覆盖同页重复、EOF 重复、真实分页、压缩后的必要回读及混合批次；确认进入恢复并可继续完成或有界失败。


## Comments

2026-11-14：按确认的 ReactProcess.Run 与模型可见工具响应边界实施 TDD。read_tool_result 返回源 call_id、Unicode 字符实际半开区间与 EOF；持久化 HistoryEvidence 不使用新回读调用的 ID 作为证据。进度守卫按源结果、字符覆盖和 ExecutionCompaction.ThroughBatch 代次统计，同页与 EOF 重复累计停滞，混合文件读取批次同步判断。

Red：`go test ./internal/agent -run '^TestReactHistoryReadReportsActualCharacterRange$' -count=1` 先因响应缺少 offset/eof 失败；补充工具元数据后通过。`go test ./internal/agent -run '^TestReactHistoryRepeatedPageEntersRecoveryAndCompletes$' -count=1` 先因同页回读 9 轮仍未收到恢复反馈失败；接入范围进度观察后通过。

Green：`go test ./internal/agent -run 'TestReactHistory|TestReadToolResult|TestReactCanReadOriginalEvidenceAgain|TestReactPreservesEveryBatch|TestReactDirectoryResults' -count=1` 通过（0.417s）。端到端覆盖中文范围、同页重复、EOF 后重复、文件读取与历史回读混合批次、连续六页新证据，以及收到恢复后正常完成。

压缩场景通过模型 usage 触发真实异步压缩及同步门禁：压缩后首次必要同页回读允许继续，同一压缩代次的再次重复触发恢复。压缩仅更换覆盖代次；总体已用轮数、恢复状态和证据索引由 ticket 02 的执行实例进度检查点统一保存，不刷新总体预算。

最终联合验证：`env -u CODEX_TEST_MYSQL_URL -u CODEX_MYSQL_URL go test ./...` 通过（数据库依赖测试按仓库约定跳过）；历史/搜索/收尾/崩溃恢复关键端到端 `go test -race` 通过（7.294s）。保留原数据库环境的全仓测试因 MySQL 1045 认证失败而未通过，真实数据库持久化仍未验证。
