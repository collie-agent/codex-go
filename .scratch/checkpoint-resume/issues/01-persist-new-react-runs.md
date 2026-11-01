# 01: 新建 ReAct 运行并持久化完整生命周期

> 本任务单属于旧版记录；本轮恢复语义以 [当前规格](../spec.md) 为准，不按本文件中的从头重跑规则实施。

**What to build:** 用户通过 CLI 或 agent-server 启动普通 ReAct 任务时，先获得会话与运行 ID，再执行任务；执行状态和完整结果保存到 MySQL，即使任务失败也能查到原运行。提供显式、可重复执行的迁移命令。本任务建立可运行的持久化路径，显式恢复与运行互斥由任务 02 交付。

**Blocked by:** None (can start immediately).

**Status:** resolved

- [x] 提供独立迁移命令，创建 conversation 和 agent_run；重复执行不破坏已有数据，普通 agent 运行不自动迁移。
- [x] conversation 包含 conv_id、create_at、update_at，conv_id 为主键。
- [x] agent_run 包含 run_id、conv_id、goal、status、result、error、mode、completion_criteria、workspace、create_at、update_at；run_id 为主键，conv_id 引用 conversation。
- [x] 列名使用 snake_case，时间列严格使用 create_at 和 update_at，不额外引入软删除字段。result 和运行级 completion_criteria 使用 JSON 保存完整结构。
- [x] 两个 agent 入口默认使用 CODEX_MYSQL_URL 持久化；配置缺失、连接失败或尚未迁移时明确报错，不回退为仅内存运行。
- [x] 未指定 conv_id 时创建新会话，指定已有 conv_id 时关联该会话；每个新任务创建独立 run_id，不再使用固定运行 ID。
- [x] 在执行开始前保存 pending run，并向 stderr 输出 run_id 和 conv_id；stdout 保留最终答案输出。conversation 只表示归属，不默认共享历史消息。
- [x] 运行开始保存 running，成功保存 completed 及完整结构化结果，失败保存 failed 及 error；result 保留 Answer、Observation、CompletionEvidence。
- [x] checkpoint 写入失败则停止继续执行，不再调度后续工具，也不把未保存的结果报告为已持久化成功。
- [x] 保存恢复需要的 goal、workspace、已确定 mode 和已产生的运行级验收标准；不保存凭据或整份 YAML 配置快照。
- [x] 普通 ReAct 不生成或保存执行步骤。纯 MCP 工具服务入口继续无需数据库即可使用。
- [x] 验证两个入口的新任务成功、执行失败、数据库不可用、会话归属及 ID 输出行为；检查结果和时间字段确实落库，并更新命令帮助及使用说明。


## Answer

已按确认的两个测试边界执行 TDD：数据库公开迁移/运行存取接口，以及两个实际编译的命令入口。交付 conversation / agent_run 显式迁移、唯一运行 ID、已有会话关联、pending → running → completed / failed 生命周期、完整 JSON 结果、运行级验收标准和工作目录保存。模型/工具诊断统一进入 stderr，stdout 仅在最终 checkpoint 成功后输出答案。

`go run ./cmd/migrate` 或 `bin/codex-migrate` 独立执行迁移；`make build` 包含迁移命令。两个 Agent 入口支持 `--conv-id`，纯 MCP 入口不依赖数据库。真实 MySQL trigger 故障注入验证工具执行前 checkpoint 失败会阻止工具、停止后续模型请求；最终保存失败不会输出未保存答案。未加入 ticket 02 的恢复/连接锁或 ticket 03 的步骤持久化。

## Comments

- 2026-10-23：真实 MySQL 8.4 隔离测试库验证重复迁移、精确列名/JSON 类型、会话归属、运行前 ID、完整结果、失败状态、数据库不可用和未迁移错误；原有命令测试改为仅使用显式 `CODEX_TEST_MYSQL_URL`。
- 全量 `go test ./...` 发现既有 `TestObserveExecDecisionRelevance` 四个子场景失败，原因均为提示词缺少“省略对应字段”；已在独立的干净 HEAD 副本复现，本票不修改该无关问题。
- 验证完成：`go test -race ./... -skip '^TestObserveExecDecisionRelevance$'`（配置隔离测试库）、`go vet ./...`、`make build` 均通过；清空两个数据库环境变量后，命令/MCP/数据库相关测试也通过，需要数据库的场景显式跳过。
