package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	fileContext "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/provider"
)

// 用实际结果类型解析示例，确保字段映射正确且两种协议表达相同的业务结果。
func checkContextFewShots[T any](t *testing.T, ctx fileContext.Contexter, count int) {
	t.Helper()
	jsonSections := strings.Split(ctx.JsonFewShot(), " output:\n")[1:]
	lineSections := strings.Split(ctx.LineProtocolFewShot(), " output:\n")[1:]
	// 普通上下文先展示五个通用语法示例；多代理直接展示完整业务文档。
	formatExamples := 5
	if _, ok := ctx.(multiAgentModelContext); ok {
		formatExamples = 0
	}
	if len(jsonSections) != count || len(lineSections) != formatExamples+count {
		t.Fatalf("unexpected example counts: JSON=%d, Line Protocol=%d", len(jsonSections), len(lineSections))
	}
	for i := range jsonSections {
		var want, got T
		decoder := json.NewDecoder(strings.NewReader(strings.SplitN(jsonSections[i], "\n\n", 2)[0]))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&want); err != nil {
			t.Fatalf("JSON example %d: %v", i+1, err)
		}
		document := strings.SplitN(lineSections[formatExamples+i], "\n\n", 2)[0]
		if err := (&provider.LineProtocolOutput[*T]{}).Parse(document, &got); err != nil {
			t.Fatalf("Line Protocol example %d: %v", i+1, err)
		}
		// Line Protocol 通过省略表示空列表，JSON 使用 []；比较前统一零长度列表。
		for _, result := range []any{&want, &got} {
			normalizeFewShotSlices(reflect.ValueOf(result).Elem())
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("example %d differs between protocols: got %+v, want %+v", i+1, got, want)
		}
	}
}

// 规划结果包含嵌套步骤，递归统一其中的空列表。
func normalizeFewShotSlices(value reflect.Value) {
	switch value.Kind() {
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			normalizeFewShotSlices(value.Field(i))
		}
	case reflect.Slice:
		if value.Len() == 0 {
			value.SetZero()
			return
		}
		for i := 0; i < value.Len(); i++ {
			normalizeFewShotSlices(value.Index(i))
		}
	}
}

func TestContextFewShotsMatchResultSchemas(t *testing.T) {
	t.Run("multi-agent", func(t *testing.T) {
		ctx := multiAgentModelContext{ReactContext: fileContext.NewReactContext(&fileContext.DurableState{})}
		checkContextFewShots[MultiAgentResult](t, ctx, 4)
		sections := strings.Split(ctx.LineProtocolFewShot(), " output:\n")[1:]
		for i, section := range sections {
			var got MultiAgentResult
			if err := (&provider.LineProtocolOutput[*MultiAgentResult]{}).Parse(strings.SplitN(section, "\n\n", 2)[0], &got); err != nil {
				t.Fatal(err)
			}
			wantStatus := CompletedReactStatus
			if i == 3 {
				wantStatus = FailedReactStatus
			}
			if got.Result.Status != wantStatus || got.Result.Answer == "" || len(got.Next) != []int{2, 1, 0, 1}[i] {
				t.Fatalf("invalid multi-agent scenario %d: %+v", i+1, got)
			}
			for _, handoff := range got.Next {
				if handoff.Task == "" || len(handoff.CompletionCriteria) == 0 || handoff.Context == "" {
					t.Fatalf("incomplete handoff: %+v", handoff)
				}
				if (i == 1 || i == 3) && handoff.Agent != "supervisor" {
					t.Fatalf("report must return to caller: %+v", handoff)
				}
			}
		}
	})
	t.Run("planner", func(t *testing.T) {
		checkContextFewShots[PlannerResult](t, fileContext.NewPlannerContext(&fileContext.DurableState{}), 1)
	})
	t.Run("standalone", func(t *testing.T) {
		checkContextFewShots[ReactResult](t, fileContext.NewReactContext(&fileContext.DurableState{}), 2)
	})
	t.Run("step", func(t *testing.T) {
		checkContextFewShots[ReactResult](t, fileContext.NewReactContext(&fileContext.DurableState{
			StepExecutions: []fileContext.StepExecution{{Status: fileContext.RunningExecutionStatus}},
		}), 2)
	})
	t.Run("observation", func(t *testing.T) {
		checkContextFewShots[ObserveResult](t, &fileContext.ObservationContext{}, 3)
	})
}
