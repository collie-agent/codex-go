package context

// SingleAgentModelContext 将生成运行级标准的协议限定在普通 ReAct，避免影响多代理和计划步骤。
type SingleAgentModelContext struct{ *ReactContext }

// NeedsCompletionCriteria 判断是否可设置运行级标准；标准可选，始终读取当前运行状态。
func (c SingleAgentModelContext) NeedsCompletionCriteria() bool {
	return len(c.DurableState.StepExecutions) == 0 && len(c.DurableState.CompletionCriteria) == 0
}

func (c SingleAgentModelContext) ModelRequest() ModelRequest {
	request := c.ReactContext.ModelRequest()
	if c.NeedsCompletionCriteria() {
		request.SystemPrompt += "\n\n# GenerateCompletionCriteria\n运行级验收标准是可选的，可直接返回最终结果，无需调用 update_completion_criteria。如需设置标准，可以先调用工具收集必要上下文，再结合原始 Goal 生成可验证的运行级验收标准，并单独调用 update_completion_criteria，使用 completion_criteria 非空字符串列表提交。覆盖目标，不扩大范围，不编造已验证事实。成功后使用 context 中的 CompletionCriteria 验收任务。"
	}
	return request
}
