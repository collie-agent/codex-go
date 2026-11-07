package application_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/application"
	"github.com/collie-agent/codex-go/internal/testutil"
)

// 使用本地模型服务和真实运行入口，确认新建/恢复均采用同样的目录优先级。
func TestRunAndResumeUseLogDirectoryOverride(t *testing.T) {
	_, store := runStore(t)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		writeResult(w, `{"status":"completed","answer":"done"}`)
	}))
	defer model.Close()
	for _, resume := range []bool{false, true} {
		for _, override := range []bool{false, true} {
			name := "run"
			if resume {
				name = "resume"
			}
			if override {
				name += "/override"
			} else {
				name += "/yaml"
			}
			t.Run(name, func(t *testing.T) {
				configuredDir := filepath.Join(t.TempDir(), "configured")
				path := agentConfig(t, model.URL)
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("log_dir: " + configuredDir + "\n")
				closeErr := f.Close()
				if err != nil {
					t.Fatal(err)
				}
				if closeErr != nil {
					t.Fatal(closeErr)
				}
				opts := application.Options{ConfigPath: path, Task: "report", Workspace: t.TempDir(), Timeout: time.Second}
				expectedDir := configuredDir
				if override {
					opts.LogDir = filepath.Join(t.TempDir(), "override")
					expectedDir = opts.LogDir
				}
				if resume {
					run, err := store.Create(context.Background(), "", opts.Task, opts.Workspace, "react")
					if err != nil {
						t.Fatal(err)
					}
					opts.RunID = run.RunID
					opts.Task = ""
					_, err = application.Resume(t.Context(), opts)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := application.Run(t.Context(), opts); err != nil {
						t.Fatal(err)
					}
				}
				data, err := os.ReadFile(filepath.Join(expectedDir, "codex.log"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(data), "run_id=") || !strings.Contains(string(data), "LLM usage") {
					t.Fatalf("missing run output: %s", data)
				}
				if override {
					if _, err := os.Stat(configuredDir); !os.IsNotExist(err) {
						t.Fatalf("YAML directory used despite override: %v", err)
					}
				}
			})
		}
	}
}
