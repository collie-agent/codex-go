package provider

import (
	"strings"
	"testing"
)

func TestLineProtocolUnterminatedAnswer(t *testing.T) {
	type result struct {
		Answer string `json:"answer"`
	}
	var got result
	// 复现长 Markdown 答案漏掉 JSON 字符串结束引号的输出。
	err := (&LineProtocolOutput[*result]{}).Parse("BEGIN RESULT\nANSWER=\"## 状态机分析\\n```\\nexploring → implement\\n```\\nEND RESULT", &got)
	for _, want := range []string{"invalid JSON literal for ANSWER", "unexpected end of JSON input"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error must contain %q, got %v", want, err)
		}
	}
	if err := (&LineProtocolOutput[*result]{}).Parse("BEGIN RESULT\nANSWER=\"## 状态机分析\\n```\\nexploring → implement\\n```\\n\"\nEND RESULT", &got); err != nil {
		t.Fatal(err)
	}
}
