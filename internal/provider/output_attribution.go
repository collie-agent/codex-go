package provider

import "context"

type participationKey struct{}

// AgentParticipation identifies one independent delegation across turns and resumes.
type AgentParticipation struct {
	ID       string
	ParentID string
	Name     string
}

// WithAgentParticipation 将委派身份与单轮执行身份分离；子委派显式指定具体父实例。
func WithAgentParticipation(ctx context.Context, agent AgentParticipation) context.Context {
	return context.WithValue(ctx, participationKey{}, agent)
}

type attributedOutput struct {
	next  Outputer
	run   string
	agent AgentParticipation
}

// AttributeOutput 装饰输出边界，使模型、工具和错误共用同一归属。
func AttributeOutput(ctx context.Context, next Outputer, run, name string) Outputer {
	if _, ok := next.(*attributedOutput); ok {
		return next
	}
	if next == nil {
		return nil
	}
	a, ok := ctx.Value(participationKey{}).(AgentParticipation)
	if !ok {
		a = AgentParticipation{ID: run + "/main", Name: name}
	}
	return &attributedOutput{next, run, a}
}
func (o *attributedOutput) Emit(ctx context.Context, event OutputEvent) error {
	event.RunID = o.run
	event.AgentInstanceID, event.ParentAgentInstanceID, event.AgentName = o.agent.ID, o.agent.ParentID, o.agent.Name
	return o.next.Emit(ctx, event)
}
