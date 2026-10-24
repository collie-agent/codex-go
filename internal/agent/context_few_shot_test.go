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
	// 前五个 Line Protocol 示例仅说明语法，业务示例在其后。
	if len(jsonSections) != count || len(lineSections) != 5+count {
		t.Fatalf("unexpected example counts: JSON=%d, Line Protocol=%d", len(jsonSections), len(lineSections))
	}
	for i := range jsonSections {
		var want, got T
		decoder := json.NewDecoder(strings.NewReader(strings.SplitN(jsonSections[i], "\n\n", 2)[0]))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&want); err != nil {
			t.Fatalf("JSON example %d: %v", i+1, err)
		}
		document := strings.SplitN(lineSections[5+i], "\n\n", 2)[0]
		if err := (&provider.LineProtocolOutput[*T]{}).Parse(document, &got); err != nil {
			t.Fatalf("Line Protocol example %d: %v", i+1, err)
		}
		// Line Protocol 通过省略表示空列表，JSON 使用 []；比较前统一零长度列表。
		for _, result := range []any{&want, &got} {
			value := reflect.ValueOf(result).Elem()
			for j := 0; j < value.NumField(); j++ {
				field := value.Field(j)
				if field.Kind() == reflect.Slice && field.Len() == 0 {
					field.SetZero()
				}
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("example %d differs between protocols: got %+v, want %+v", i+1, got, want)
		}
	}
}

func TestContextFewShotsMatchResultSchemas(t *testing.T) {
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
	t.Run("analyzer", func(t *testing.T) {
		checkContextFewShots[TaskAnalyzerResult](t, fileContext.NewTaskAnalyzerContext(&fileContext.DurableState{}), 2)
	})
}
