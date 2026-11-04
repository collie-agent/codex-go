package application

import (
	"context"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	"github.com/collie-agent/codex-go/internal/provider"
)

type deliveryRecorder struct{ events []provider.OutputEvent }

func (r *deliveryRecorder) Emit(_ context.Context, e provider.OutputEvent) error {
	r.events = append(r.events, e)
	return nil
}

func TestOutputDeliveryDoesNotRequireDatabase(t *testing.T) {
	recorder := &deliveryRecorder{}
	// A nil database ensures output delivery is independent of database storage.
	_, ctx, opts, cleanup := prepareOutput(context.Background(), Options{Output: recorder}, &config.AgentConfig{}, nil)
	defer cleanup()
	if err := opts.Output.Emit(ctx, provider.OutputEvent{TaskID: "task", RunID: "run", Kind: provider.OutputTextDelta}); err != nil {
		t.Fatal(err)
	}
	if len(recorder.events) != 1 {
		t.Fatalf("delivered %d events", len(recorder.events))
	}
}
