package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/application"
	"github.com/collie-agent/codex-go/internal/config"
	"github.com/collie-agent/codex-go/internal/testutil"
)

func TestRunAndResumePassExecConfigurationToExternalMCP(t *testing.T) {
	_, store := runStore(t)
	for _, resume := range []bool{false, true} {
		for _, override := range []bool{false, true} {
			t.Run(testName(resume, override), func(t *testing.T) {
				capture := filepath.Join(t.TempDir(), "args")
				t.Setenv("CODEX_TDD_MCP_ARGS", capture)
				t.Setenv("CODEX_TDD_EXEC_ENV", "configured-value")
				t.Setenv("CODEX_TDD_OVERRIDE_ENV", "override-value")
				command := filepath.Join(t.TempDir(), "mcp")
				if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CODEX_TDD_MCP_ARGS\"\nexit 1\n"), 0700); err != nil {
					t.Fatal(err)
				}
				path := agentConfig(t, "http://127.0.0.1:1")
				cacheDir := appendExecConfig(t, path)
				opts := application.Options{ConfigPath: path, Task: "report", Workspace: t.TempDir(), Timeout: time.Second, MCPCommand: command, Stderr: io.Discard}
				want := "--exec-go-proxy\nhttps://proxy.example.invalid\n--exec-cache\n{\"name\":\"go-mod\",\"directory\":\"/tmp/codex-test-cache\",\"env\":[\"GOMODCACHE\"]}\n--exec-network\nhost\n--exec-env\nCODEX_TDD_EXEC_ENV\n--discover-host-commands\n--exec-command\npoetry\n--runtime-dir\n/opt/configured-runtime\n"
				if override {
					opts.ExecGoProxy = "off"
					opts.ExecCaches = []config.ExecCache{}
					opts.ExecNetwork = "none"
					opts.ExecEnv = []string{"CODEX_TDD_OVERRIDE_ENV"}
					opts.DiscoverHostCommandsSet = true
					opts.ExecCommands = []string{"git"}
					opts.RuntimeDirs = []string{"/opt/override-runtime"}
					want = "--exec-go-proxy\noff\n--exec-cache\n[]\n--exec-network\nnone\n--exec-env\nCODEX_TDD_OVERRIDE_ENV\n--exec-command\ngit\n--runtime-dir\n/opt/override-runtime\n"
				}
				want = strings.ReplaceAll(want, "/tmp/codex-test-cache", cacheDir)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if resume {
					run, err := store.Create(ctx, "", opts.Task, opts.Workspace, "react")
					if err != nil {
						t.Fatal(err)
					}
					opts.RunID, opts.Task = run.RunID, ""
					_, _ = application.Resume(ctx, opts)
				} else {
					_, _ = application.Run(ctx, opts)
				}
				data, err := os.ReadFile(capture)
				if err != nil {
					t.Fatal(err)
				}
				suffix := strings.SplitAfterN(string(data), "1s\n", 2)
				if len(suffix) != 2 || suffix[1] != want {
					t.Fatalf("MCP arguments = %q, want suffix %q", data, want)
				}
			})
		}
	}
}

func testName(resume, override bool) string {
	name := "run"
	if resume {
		name = "resume"
	}
	if override {
		name += "/override"
	} else {
		name += "/yaml"
	}
	return name
}

func appendExecConfig(t *testing.T, path string) string {
	cacheDir := filepath.Join(t.TempDir(), "go-mod")
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(fmt.Sprintf("exec_go_proxy: https://proxy.example.invalid\nexec_caches: [{name: go-mod, directory: %s, env: [GOMODCACHE]}]\nexec_network: host\nexec_env: [CODEX_TDD_EXEC_ENV]\ndiscover_host_commands: true\nexec_commands: [poetry]\nruntime_dirs: [/opt/configured-runtime]\n", cacheDir))
	closeErr := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return cacheDir
}

// The model-visible tool description is the public contract for the in-process tool allowlist.
func TestRunAndResumeUseConfiguredExecAllowlist(t *testing.T) {
	_, store := runStore(t)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		var request struct {
			Tools []struct{ Name, Description string }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		found := false
		for _, tool := range request.Tools {
			if tool.Name == "exec" && strings.Contains(tool.Description, "Allowed entrypoints for this server:") && strings.Contains(tool.Description, "poetry") {
				found = true
			}
		}
		if !found {
			t.Errorf("configured exec allowlist missing from model tools")
		}
		writeResult(w, `{"status":"completed","answer":"done"}`)
	}))
	defer model.Close()
	for _, resume := range []bool{false, true} {
		t.Run(testName(resume, false), func(t *testing.T) {
			path := agentConfig(t, model.URL)
			appendExecConfig(t, path)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.Replace(string(data), "agents:\n  plan:", "agents:\n  react:\n    allow_tools: [exec]\n  plan:", 1))
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			opts := application.Options{ConfigPath: path, Task: "report", Workspace: t.TempDir(), Timeout: time.Second, Stderr: io.Discard}
			if resume {
				run, err := store.Create(context.Background(), "", opts.Task, opts.Workspace, "react")
				if err != nil {
					t.Fatal(err)
				}
				opts.RunID, opts.Task = run.RunID, ""
				if _, err := application.Resume(t.Context(), opts); err != nil {
					t.Fatal(err)
				}
			} else if _, err := application.Run(t.Context(), opts); err != nil {
				t.Fatal(err)
			}
		})
	}
}
