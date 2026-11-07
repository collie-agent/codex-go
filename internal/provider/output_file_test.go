package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/provider"
)

func TestOutputAppendsLogsInConfiguredDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "logs")
	for _, text := range []string{"first", "second"} {
		output := provider.NewStderrOutputer(nil, dir)
		if err := output.Emit(context.Background(), provider.OutputEvent{TaskID: "task", Kind: provider.OutputText, Data: json.RawMessage(`{"text":"` + text + `"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "codex.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "LLM response text=first") || !strings.Contains(string(data), "LLM response text=second") {
		t.Fatalf("logs=%s", data)
	}
}

func TestOutputReturnsDirectoryError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	err := provider.NewStderrOutputer(nil, path).Emit(context.Background(), provider.OutputEvent{Kind: provider.OutputText, Data: json.RawMessage(`{"text":"message"}`)})
	if err == nil {
		t.Fatal("expected directory error")
	}
}

func TestOutputLogsTaskMetadataWithTimestampAndLevel(t *testing.T) {
	var output bytes.Buffer
	err := provider.NewStderrOutputer(&output).Emit(t.Context(), provider.OutputEvent{Kind: provider.OutputTaskInfo, Data: json.RawMessage(`{"run_id":"run-1","conv_id":"conv-1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), " [INFO] run_id=run-1 conv_id=conv-1\n") {
		t.Fatalf("unformatted metadata: %q", output.String())
	}
	if _, err := time.ParseInLocation("2006/01/02 15:04:05", output.String()[:19], time.Local); err != nil {
		t.Fatal(err)
	}
}
