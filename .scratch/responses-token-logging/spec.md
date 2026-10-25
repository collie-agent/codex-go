# Responses API token 统计日志

Status: needs-info

设计问题已全部确认；等待整体理解确认后进入实现。

## 已确认

- 按每次实际发出的 Responses API 请求分别记录，覆盖内部辅助调用与重试。
- 总输入、总输出指该次请求的 input / output token 数；本次不增加代理运行或会话累计。
- 记录总输入 token、总输出 token、命中缓存的输入 token、未命中缓存的输入 token。
- 所有日志级别均打印 token 统计，包括 `error`；不受 INFO 日志过滤控制。

## 统计口径

- `input_tokens`：服务端报告的本次请求总输入 token。
- `output_tokens`：服务端报告的本次请求总输出 token。
- `cached_input_tokens`：服务端报告的 `usage.input_tokens_details.cached_tokens`。
- `uncached_input_tokens`：`input_tokens - cached_input_tokens`，包括缓存写入所消耗的输入。
- 缺失或 null 的统计不解释为零；对应字段打印 `unknown`。缓存明细缺失时，缓存命中及未命中均为 `unknown`，仍保留已知的输入和输出数值。
- 服务端明确返回的零值按 `0` 打印。

## 请求边界与异常

- 一次 HTTP 尝试对应一条统计日志，包括 SDK 内部自动重试；SDK 调用最终返回时不得再重复记录同一尝试。
- SDK 完整读取的响应有 usage 时记录实际统计，与 HTTP 成败、response 状态、工具调用、输出解析是否成功无关。SDK 在重试前直接关闭且未读取的正文，其统计记录为 `unknown`，不为获取 usage 额外读取或延迟重试。
- 非 completed 响应同样记录；日志中的请求结果保留实际 response 状态。
- 网络错误、超时、HTTP 错误、缺失 usage 或无法解析响应时，缺失的数值打印 `unknown`，不估算消耗。
- 构建请求、生成 schema、校验工具消息等在 HTTP 请求发出前失败的情况不产生 token 日志。
- 每次尝试保留独立日志，不聚合到最终成功的重试，也不增加运行或会话累计。

## 日志格式

- 使用单行日志，每条包含 task ID、现有调用标签（有则打印）、请求模型、response ID（有则打印）、请求结果，以及四项 token 数。
- 所有日志级别均打印，包括 `error`，不新增开关。
- 沿用现有日志输出位置；单条日志一次写入，避免并发请求交错。
- 不为 token 日志添加请求内容、响应正文或凭据；已有上下文和输出日志行为不在本次修改范围内。

示例：

```text
task <id>: [observe] LLM usage model=<model> response_id=<id> status=completed input_tokens=1000 output_tokens=120 cached_input_tokens=600 uncached_input_tokens=400
```

## 实现约束与验收

- 在统一 provider 请求路径记录，覆盖 planner、worker、observe，以及工具调用后再次请求和输出纠错请求。
- 在 HTTP 尝试层接入，以覆盖 SDK 自动重试；读取响应不得影响 SDK 的响应解析、错误处理、重试和连接关闭。
- 统计记录失败不得替换原有请求结果或错误；日志中的未知状态不修改请求语义。
- 在关键的 HTTP 尝试边界、响应正文保留、缺失统计与零值区分处加注释。
- 使用本地 HTTP 测试验证正常 usage、缓存全命中/零命中、显式零、缺失/null usage 和缺失缓存明细。
- 验证非 completed、HTTP 错误、网络错误、SDK 自动重试、工具调用及输出解析失败时的记录行为和每次尝试的日志数量。
- 验证 `info` / `error` 两种日志级别均输出统计，并保留任务、调用标签、模型和可用 response ID。
- 实现后执行 `go test ./...`。

## 代码事实

- 当前所有模型请求集中于 `internal/provider/responses.go` 中的 `Responses.New`，使用非流式请求。
- 响应状态检查、工具调用处理和输出解析发生在请求返回后；输出解析失败也可能已消耗 token。
- 当前日志配置支持 `info` / `error`，默认 `info`。
- 当前 SDK 默认最多自动重试两次；仅在 `Responses.New` 返回后记录无法覆盖每次 HTTP 尝试。

## 确认记录

- Q1：`confirm`，逐请求记录，不增加累计。
- Q2：`所有日志级别都打印`。
- Q3–Q5：`all confirm`，确认缓存计算、未知值、异常与每次 HTTP 尝试记录，以及单行日志字段。
- 本次未引入项目特有的新领域概念，通用 token 统计字段保留在规格中，不扩充 `GLOSSARY.md`。
- 本次选择容易调整，不满足 ADR 的高变更成本条件，不新增 ADR。

## 审查修复（2026-10-25）

- 统计改为被动收集 SDK 读取的响应字节，在关闭时记录，不再在 middleware 中预读取正文。
- SDK 直接丢弃的重试正文不包含可观测的 usage；该次保留 HTTP 状态、四项统计为 `unknown`。最终完整读取的 HTTP 错误正文仍记录实际 usage。此边界保证统计不会阻塞重试。
- SDK 调用结束后兜底关闭所有尝试的响应体；关闭与记录均只执行一次，覆盖响应头返回后取消及读取正文期间超时。
