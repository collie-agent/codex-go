package provider

import "context"

type outputerContextKey struct{}

func WithOutputer(ctx context.Context, output Outputer) context.Context {
	return context.WithValue(ctx, outputerContextKey{}, output)
}

func OutputerFromContext(ctx context.Context) Outputer {
	output, _ := ctx.Value(outputerContextKey{}).(Outputer)
	return output
}
