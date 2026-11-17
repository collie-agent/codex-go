package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	"github.com/collie-agent/codex-go/internal/provider"
)

func TestExplorationFewShotProtocols(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		for _, multiAgent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/multi=%t", schema, multiAgent), func(t *testing.T) {
				prompt := explorationFewShot(schema, multiAgent)
				if schema == config.LineProtocolLLMOutputSchema {
					if strings.Contains(prompt, `"exploration":`) || strings.Contains(prompt, `"target_files":`) {
						t.Fatal("line protocol few-shot contains JSON examples")
					}
					if !strings.Contains(prompt, "TARGET_FILES=\"internal/parser/parser.go\"\nTARGET_FILES=\"internal/parser/parser_test.go\"") {
						t.Fatal("missing repeated target file lines")
					}
				} else if strings.Contains(prompt, "BEGIN RESULT") || strings.Contains(prompt, "TARGET_FILES=") {
					t.Fatal("JSON few-shot contains line protocol examples")
				}
				parts := strings.Split(prompt, "\n响应：\n")
				if len(parts) != 5 {
					t.Fatalf("expected four examples, got %d", len(parts)-1)
				}
				for i, part := range parts[1:] {
					response := strings.SplitN(part, "\n\n### 示例 ", 2)[0]
					response = strings.SplitN(response, "\n## 分类后的回答", 2)[0]
					var result ReactResult
					var err error
					if multiAgent {
						var wrapped MultiAgentResult
						if schema == config.JsonLLMOutputSchema {
							err = (&provider.JsonOutput[*MultiAgentResult]{}).Parse(response, &wrapped)
						} else {
							err = (&provider.LineProtocolOutput[*MultiAgentResult]{}).Parse(response, &wrapped)
						}
						result = wrapped.Result
						if len(wrapped.Next) != 0 {
							t.Fatal("phase decision must not delegate")
						}
					} else if schema == config.JsonLLMOutputSchema {
						err = (&provider.JsonOutput[*ReactResult]{}).Parse(response, &result)
					} else {
						err = (&provider.LineProtocolOutput[*ReactResult]{}).Parse(response, &result)
					}
					if err != nil {
						t.Fatalf("example %d: %v", i+1, err)
					}
					if result.Status != "" || result.Answer != "" || result.Exploration == nil {
						t.Fatalf("example %d is not an independent phase decision: %+v", i+1, result)
					}
					decision := result.Exploration
					if decision.Choice != []string{"implement", "extend", "unable", ""}[i] || decision.Basis == "" {
						t.Fatalf("example %d: unexpected decision %+v", i+1, decision)
					}
					if i == 0 && (len(decision.TargetFiles) != 2 || decision.TargetFiles[0] != "internal/parser/parser.go" || decision.TargetFiles[1] != "internal/parser/parser_test.go") {
						t.Fatalf("implementation target lost: %+v", decision)
					}
				}
			})
		}
	}
}
