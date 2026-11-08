package mcp_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/collie-agent/codex-go/internal/mcp"
)

func TestCommandExecutorDiscoveryBoundary(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		enabled, inWorkspace bool
		commands             []string
		wantOK               bool
	}{
		{"enabled", true, false, []string{"poetry"}, true},
		{"disabled", false, false, []string{"poetry"}, false},
		{"defaults retained", true, false, []string{"python3"}, true},
		{"workspace executable", true, true, []string{"poetry"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace, runtimeDir := t.TempDir(), t.TempDir()
			if tc.inWorkspace {
				runtimeDir = workspace
			}
			probe := filepath.Join(runtimeDir, "data")
			if err := os.WriteFile(probe, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			launcher := "#!/usr/bin/python3\nfrom pathlib import Path\np=Path(" + "'" + probe + "'" + ")\nassert p.read_text() == 'original'\ntry:\n p.write_text('changed')\nexcept OSError:\n print('readonly')\nelse:\n raise RuntimeError('runtime writable')\n"
			if err := os.WriteFile(filepath.Join(runtimeDir, "poetry"), []byte(launcher), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", runtimeDir)
			executor, err := mcp.NewCommandExecutorWithConfig(mcp.Config{Workspace: workspace, DiscoverHostCommands: tc.enabled, ExecCommands: tc.commands})
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(context.Background(), workspace, "poetry", nil)
			if tc.wantOK {
				if err != nil || result.ExitCode != 0 || result.Stdout != "readonly\n" {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			} else if err == nil {
				t.Fatalf("unexpected execution: %+v", result)
			}
		})
	}
}

func TestCommandExecutorDiscoversPythonVirtualEnvironment(t *testing.T) {
	runtimeDir, workspace := t.TempDir(), t.TempDir()
	bin := filepath.Join(runtimeDir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "pyvenv.cfg"), []byte("home = /usr/bin\ninclude-system-site-packages = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/python3", filepath.Join(bin, "python")); err != nil {
		t.Fatal(err)
	}
	script := "#!" + filepath.Join(bin, "python") + "\nimport sys\nfrom pathlib import Path\np=Path(sys.prefix)/'pyvenv.cfg'\ntry:\n p.write_text('changed')\nexcept OSError:\n print(sys.prefix)\nelse:\n raise RuntimeError('venv writable')\n"
	if err := os.WriteFile(filepath.Join(bin, "poetry"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	executor, err := mcp.NewCommandExecutorWithConfig(mcp.Config{Workspace: workspace, DiscoverHostCommands: true, ExecCommands: []string{"poetry"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), workspace, "poetry", nil)
	if err != nil || result.ExitCode != 0 || result.Stdout != runtimeDir+"\n" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
