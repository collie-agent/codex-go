package provider

import (
	"reflect"
	"strings"
	"testing"
)

type parserStep struct {
	ID     string `json:"id"`
	Intent string `json:"intent"`
}

type parserPlan struct {
	Revision     int          `json:"revision"`
	Goal         string       `json:"goal"`
	Steps        []parserStep `json:"steps" lineprotocol:"STEP"`
	Observations []string     `json:"observations"`
}

var (
	_ LLMOutputParser[*parserPlan] = (*JsonOutput[*parserPlan])(nil)
	_ LLMOutputParser[*parserPlan] = (*LineProtocolOutput[*parserPlan])(nil)
)

func TestLineProtocolMisplacedFieldReportsPathAndDestination(t *testing.T) {
	type result struct {
		Answer string `json:"answer"`
	}
	type response struct {
		EntryCompletionCriteria []string `json:"entry_completion_criteria"`
		Result                  result   `json:"result"`
	}
	var got response
	err := (&LineProtocolOutput[*response]{}).Parse("BEGIN RESULT\nBEGIN RESULT\nANSWER=\"delegate\"\nENTRY_COMPLETION_CRITERIA=\"cite evidence\"\nEND RESULT\nEND RESULT", &got)
	for _, want := range []string{"result.entry_completion_criteria", "Move ENTRY_COMPLETION_CRITERIA", "outer RESULT", "outside the nested RESULT"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error must contain %q, got %v", want, err)
		}
	}
}

func TestLineProtocolRepeatedItemFieldReportsSeparateBlocks(t *testing.T) {
	type evidence struct {
		Text          string
		SourceCallIDs []string
	}
	type summary struct {
		Facts []evidence `lineprotocol:"FACTS"`
	}
	var got summary
	err := (&LineProtocolOutput[*summary]{}).Parse("BEGIN RESULT\nBEGIN FACTS\nTEXT=\"first\"\nSOURCECALLIDS=\"call_1\"\nTEXT=\"second\"\nSOURCECALLIDS=\"call_2\"\nEND FACTS\nEND RESULT", &got)
	for _, want := range []string{"repeated non-array field TEXT", "Facts[0].Text", "separate BEGIN FACTS / END FACTS blocks", "exactly once"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error must contain %q, got %v", want, err)
		}
	}
}

func TestJsonOutput(t *testing.T) {
	var model parserPlan
	parser := &JsonOutput[*parserPlan]{}
	if err := parser.Parse(`{"revision":2,"goal":"inspect"}`, &model); err != nil {
		t.Fatal(err)
	}
	if model.Revision != 2 || model.Goal != "inspect" {
		t.Fatalf("unexpected model: %+v", model)
	}
	if err := parser.Parse(`{"revision":"two"}`, &model); err == nil {
		t.Fatal("expected type mismatch")
	}
	if err := parser.Parse(`{`, &model); err == nil {
		t.Fatal("expected invalid JSON error")
	}
	if err := parser.Parse(`{}`, nil); err == nil {
		t.Fatal("expected nil destination error")
	}
}

func TestLineProtocolOutput(t *testing.T) {
	document := "begin_plan\nREVISION=2\ngoal=\"inspect\\nsource\"\n" +
		"OBSERVATIONS=\"first\"\nOBSERVATIONS=\"second\"\n" +
		"BEGIN STEP\nID=\"one\"\nINTENT=\"read\"\nEND STEP\n" +
		"BEGIN_STEP\nID=\"two\"\nINTENT=\"write\"\nEND_STEP\nEND_PLAN"
	want := parserPlan{Revision: 2, Goal: "inspect\nsource", Observations: []string{"first", "second"},
		Steps: []parserStep{{ID: "one", Intent: "read"}, {ID: "two", Intent: "write"}}}
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		for _, trailing := range []string{"", newline} {
			var got parserPlan
			if err := (&LineProtocolOutput[*parserPlan]{}).Parse(strings.ReplaceAll(document, "\n", newline)+trailing, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		}
	}
}

func TestLineProtocolRejectsInvalidDocuments(t *testing.T) {
	cases := map[string]string{
		"empty":                       "",
		"blank":                       "BEGIN PLAN\n\nEND PLAN",
		"comment":                     "BEGIN PLAN\n# comment\nEND PLAN",
		"multiple roots":              "BEGIN PLAN\nEND PLAN\nBEGIN PLAN\nEND PLAN",
		"wrong end":                   "BEGIN PLAN\nEND STEP",
		"missing end":                 "BEGIN PLAN",
		"outside field":               "GOAL=\"test\"\nBEGIN PLAN\nEND PLAN",
		"prose":                       "BEGIN PLAN\nEND PLAN\nexplanation",
		"fence":                       "```\nBEGIN PLAN\nEND PLAN\n```",
		"leading whitespace":          " BEGIN PLAN\nEND PLAN",
		"literal whitespace":          "BEGIN PLAN\nREVISION= 1\nEND PLAN",
		"trailing literal whitespace": "BEGIN PLAN\nREVISION=1 \nEND PLAN",
		"invalid json":                "BEGIN PLAN\nGOAL=unquoted\nEND PLAN",
		"nan":                         "BEGIN PLAN\nREVISION=NaN\nEND PLAN",
		"infinity":                    "BEGIN PLAN\nREVISION=Infinity\nEND PLAN",
		"multiple literals":           "BEGIN PLAN\nREVISION=1 2\nEND PLAN",
		"unknown field":               "BEGIN PLAN\nEXTRA=true\nEND PLAN",
		"repeated scalar":             "BEGIN PLAN\nREVISION=1\nrevision=2\nEND PLAN",
		"unknown child":               "BEGIN PLAN\nBEGIN OTHER\nEND OTHER\nEND PLAN",
		"unknown nested field":        "BEGIN PLAN\nBEGIN STEP\nOTHER=1\nEND STEP\nEND PLAN",
		"mixed field and child":       "BEGIN PLAN\nSTEP={}\nBEGIN STEP\nEND STEP\nEND PLAN",
		"type mismatch":               "BEGIN PLAN\nREVISION=\"one\"\nEND PLAN",
		"extra newline":               "BEGIN PLAN\nEND PLAN\n\n",
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			var model parserPlan
			if err := (&LineProtocolOutput[*parserPlan]{}).Parse(document, &model); err == nil {
				t.Fatal("expected parse error")
			}
		})
	}
}

func TestLineProtocolChildTypeName(t *testing.T) {
	type Detail struct {
		Text string `json:"text"`
	}
	type response struct {
		Value *Detail `json:"value"`
	}
	var model response
	parser := &LineProtocolOutput[*response]{}
	if err := parser.Parse("BEGIN RESPONSE\nBEGIN DETAIL\nTEXT=\"ok\"\nEND DETAIL\nEND RESPONSE", &model); err != nil {
		t.Fatal(err)
	}
	if model.Value == nil || model.Value.Text != "ok" {
		t.Fatalf("unexpected model: %+v", model)
	}
	if err := parser.Parse("BEGIN RESPONSE\nBEGIN DETAIL\nEND DETAIL\nBEGIN DETAIL\nEND DETAIL\nEND RESPONSE", &model); err == nil {
		t.Fatal("expected repeated non-array child error")
	}
}

func TestLineProtocolScalarValues(t *testing.T) {
	type response struct {
		Value any `json:"value"`
	}
	parser := &LineProtocolOutput[*response]{}
	for _, literal := range []string{`[1,2]`, `[]`, `{"a":1}`, `{}`, `{'a':1}`} {
		t.Run(literal, func(t *testing.T) {
			var model response
			if err := parser.Parse("BEGIN RESPONSE\nVALUE="+literal+"\nEND RESPONSE", &model); err == nil {
				t.Fatal("expected array or object literal to be rejected")
			}
		})
	}
	for _, tc := range []struct {
		literal string
		want    any
	}{
		{`"[1,2]"`, "[1,2]"},
		{`"{'a':1}"`, "{'a':1}"},
		{`2`, float64(2)},
		{`0.5`, float64(0.5)},
		{`true`, true},
		{`false`, false},
		{`null`, nil},
	} {
		t.Run(tc.literal, func(t *testing.T) {
			var model response
			if err := parser.Parse("BEGIN RESPONSE\nVALUE="+tc.literal+"\nEND RESPONSE", &model); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(model.Value, tc.want) {
				t.Fatalf("got %#v, want %#v", model.Value, tc.want)
			}
		})
	}
}

func TestLineProtocolDestination(t *testing.T) {
	if err := (&LineProtocolOutput[*parserPlan]{}).Parse("BEGIN PLAN\nEND PLAN", nil); err == nil {
		t.Fatal("expected nil destination error")
	}
	if err := (&LineProtocolOutput[parserPlan]{}).Parse("BEGIN PLAN\nEND PLAN", parserPlan{}); err == nil {
		t.Fatal("expected non-pointer destination error")
	}
	var number int
	if err := (&LineProtocolOutput[*int]{}).Parse("BEGIN PLAN\nEND PLAN", &number); err == nil {
		t.Fatal("expected non-struct destination error")
	}
}

func TestLineProtocolResponseDocument(t *testing.T) {
	type result struct {
		Answer string `json:"answer"`
	}
	document := "BEGIN RESULT\nANSWER=\"done\"\nEND RESULT"
	for _, tc := range []struct {
		name  string
		text  string
		valid bool
	}{
		{"bare", document, true},
		{"preamble", "Based on the code examined.\n\n" + document, true},
		{"postscript", document + "\n\nTask completed.", true},
		{"fenced", "```text\n" + document + "\n```", true},
		{"CRLF", strings.ReplaceAll("Explanation\n"+document, "\n", "\r\n"), true},
		{"CR", strings.ReplaceAll("Explanation\n"+document, "\n", "\r"), true},
		{"multiple roots", "Explanation\n" + document + "\n" + document, false},
		{"missing end", "Explanation\nBEGIN RESULT\nANSWER=\"done\"", false},
		{"wrong end", "Explanation\nBEGIN RESULT\nANSWER=\"done\"\nEND OTHER", false},
		{"outside field before", "ANSWER=\"lost\"\n" + document, false},
		{"outside field after", document + "\nANSWER=\"lost\"", false},
		{"invalid inside", "Explanation\nBEGIN RESULT\nANSWER=done\nEND RESULT", false},
		{"prose inside", "BEGIN RESULT\nExplanation\nEND RESULT", false},
		{"no block", "Explanation only", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got result
			err := (&LineProtocolOutput[*result]{}).Parse(lineProtocolResponseDocument(tc.text), &got)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v got result=%+v err=%v", tc.valid, got, err)
			}
			if tc.valid && got.Answer != "done" {
				t.Fatalf("unexpected answer %q", got.Answer)
			}
		})
	}
	// 最小复现：同一个合法块只增加说明前缀就会被严格解析器拒绝。
	var got result
	err := (&LineProtocolOutput[*result]{}).Parse("Explanation\n"+document, &got)
	if err == nil || !strings.Contains(err.Error(), "line 1: invalid line or text outside a block") {
		t.Fatalf("expected original symptom, got %v", err)
	}
}
