# 05: 多代理切换事件

Status: resolved

多代理模式通过 Outputer.Emit 发出 `agent_switch`，消息体包含 `src_agent_name` 和 `dst_agent_name`。

- [x] 委派前发出父到子的切换，子代理交回最终结果时发出子到父的切换。
- [x] 保留发起切换的代理参与实例归属，沿用现有持久化、Redis 和 SSE 链路。
- [x] 输出失败向上传播，停止后续调度。
- [x] 通过已确认的 Run → Outputer.Emit 和运行 → Redis/SSE 边界验证。

## Comments

- 用户确认上述两个测试边界。首个 Run 测试先因缺失事件失败，再实现通过；Redis/SSE 测试先因输出器不支持新类型失败，再接入通过。
- 协议见 [Redis 输出协议](../../../docs/redis-output.md)。
