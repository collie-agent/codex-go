package provider

import (
	"strings"
	"testing"
)

// Validate the actual examples sent to the model against the output parser.
func TestLineProtocolFewShotExamples(t *testing.T) {
	type detail struct {
		Message string `json:"message"`
	}
	type item struct {
		Detail detail   `json:"detail"`
		Empty  struct{} `json:"empty"`
	}
	type example struct {
		Message      string       `json:"message"`
		Count        int          `json:"count"`
		Ratio        float64      `json:"ratio"`
		OK           bool         `json:"ok"`
		Enabled      bool         `json:"enabled"`
		Goal         string       `json:"goal"`
		Tags         []string     `json:"tags"`
		Steps        []parserStep `json:"steps" lineprotocol:"STEP"`
		Note         *string      `json:"note"`
		Text         string       `json:"text"`
		Raw          string       `json:"raw"`
		Completed    bool         `json:"done" lineprotocol:"COMPLETED"`
		MetaNote     string       `json:"meta_note" lineprotocol:"META.NOTE"`
		Items        []item       `json:"items" lineprotocol:"ITEM"`
		Observations []string     `json:"observations"`
		Fallback     *detail      `json:"fallback"`
	}
	sections := strings.Split(lineProtocolFewShotPrompt, " output:\n")[1:]
	if len(sections) != 5 {
		t.Fatalf("expected five examples, got %d", len(sections))
	}
	for i, section := range sections {
		document := strings.SplitN(section, "\n\n", 2)[0]
		var got example
		if err := (&LineProtocolOutput[*example]{}).Parse(document, &got); err != nil {
			t.Fatalf("example %d: %v", i+1, err)
		}
		switch i {
		case 2:
			if got.Text != "quote: \"; slash: \\; newline: \n; tab: \t" || got.Raw != "[1,2]" || !got.Completed || got.MetaNote != "literal dotted field" {
				t.Fatalf("unexpected escaped strings or tagged fields: %+v", got)
			}
		case 3:
			if len(got.Tags) != 1 || got.Tags[0] != "only" || len(got.Items) != 1 || got.Items[0].Detail.Message != "nested" || got.Observations != nil || got.Fallback != nil {
				t.Fatalf("unexpected lists or nested objects: %+v", got)
			}
		}
	}
}
