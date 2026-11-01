package consumer

import (
	"testing"

	"github.com/collie-agent/codex-go/internal/agent"
)

func TestHasBusinessFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		results []agent.ReactResult
		want    bool
	}{
		{name: "execution failure has no saved result"},
		{name: "successful result", results: []agent.ReactResult{{Status: agent.CompletedReactStatus}}},
		{name: "saved business failure", results: []agent.ReactResult{{Status: agent.FailedReactStatus}}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasBusinessFailure(tc.results); got != tc.want {
				t.Fatalf("hasBusinessFailure() = %v, want %v", got, tc.want)
			}
		})
	}
}
