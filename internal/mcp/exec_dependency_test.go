package mcp_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	"github.com/collie-agent/codex-go/internal/mcp"
)

// A real local module proxy makes dependency downloads reproducible without
// public network access or relying on packages installed on the host.
func TestGoTidyUpdatesModuleFilesAndReusesPersistentCacheOffline(t *testing.T) {
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for name, body := range map[string]string{"go.mod": "module example.com/cachedep\n\ngo 1.20\n", "dep.go": "package cachedep\nconst Value = 42\n"} {
		f, err := zw.Create("example.com/cachedep@v1.0.0/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/example.com/cachedep/@v/v1.0.0.mod":
			fmt.Fprint(w, "module example.com/cachedep\n\ngo 1.20\n")
		case "/example.com/cachedep/@v/v1.0.0.info":
			fmt.Fprint(w, `{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}`)
		case "/example.com/cachedep/@v/v1.0.0.zip":
			w.Write(archive.Bytes())
		case "/example.com/cachedep/@v/list":
			fmt.Fprintln(w, "v1.0.0")
		default:
			http.NotFound(w, r)
		}
	}))
	defer proxy.Close()
	cache := t.TempDir()
	// Go extracts module directories read-only; restore directory permissions for
	// the temporary fixture cleanup after verifying the real cache behavior.
	t.Cleanup(func() {
		_ = filepath.WalkDir(cache, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(path, 0700)
			}
			return err
		})
	})
	for _, offline := range []bool{false, true} {
		t.Run(fmt.Sprintf("offline=%v", offline), func(t *testing.T) {
			workspace := t.TempDir()
			module := "module example.com/app\n\ngo 1.20\n"
			if offline {
				// Offline tidy can use a cached version, but cannot discover latest.
				module += "\nrequire example.com/cachedep v1.0.0 // indirect\n"
			}
			if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte(module), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := mcp.Config{Workspace: workspace, ExecNetwork: "host", ExecGoProxy: proxy.URL, ExecCaches: []config.ExecCache{{Name: "go-mod", Directory: cache, Env: []string{"GOMODCACHE"}}}}
			if offline {
				proxy.Close()
				cfg.ExecGoProxy = "off"
				cfg.ExecNetwork = "none"
			}
			executor, err := mcp.NewCommandExecutorWithConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			run := func(args ...string) string {
				t.Helper()
				result, err := executor.Execute(t.Context(), workspace, "go", args)
				if err != nil || result.ExitCode != 0 {
					t.Fatalf("go %v: %+v err=%v", args, result, err)
				}
				return result.Stdout
			}
			// Let tidy resolve the import itself: go get would prepopulate go.mod
			// and go.sum and mask failures to write either file during tidy.
			if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\nimport d \"example.com/cachedep\"\nfunc main() { println(d.Value) }\n"), 0600); err != nil {
				t.Fatal(err)
			}
			run("mod", "tidy")
			mod, err := os.ReadFile(filepath.Join(workspace, "go.mod"))
			if err != nil || !strings.Contains(string(mod), "require example.com/cachedep v1.0.0") || strings.Contains(string(mod), "// indirect") {
				t.Fatalf("tidy did not update go.mod: %q err=%v", mod, err)
			}
			if got := run("list", "-m", "example.com/cachedep"); got != "example.com/cachedep v1.0.0\n" {
				t.Fatalf("module = %q", got)
			}
			sum, err := os.ReadFile(filepath.Join(workspace, "go.sum"))
			if err != nil || !strings.Contains(string(sum), "example.com/cachedep v1.0.0 h1:") {
				t.Fatalf("tidy did not update go.sum: %q err=%v", sum, err)
			}
		})
	}
}
