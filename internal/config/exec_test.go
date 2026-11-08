package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
)

func TestLoadExecSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	data := "name: react\ndiscover_host_commands: true\nexec_commands: [poetry, python3]\nruntime_dirs: [/opt/python]\nbase:\n  llm:\n    model: test\n    api_key: test\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*config.AgentConfig{cfg, cfg.ForAgent("react"), cfg.ForAgent("observe")} {
		if !c.Global.DiscoverHostCommands || !reflect.DeepEqual(c.Global.ExecCommands, []string{"poetry", "python3"}) || !reflect.DeepEqual(c.Global.RuntimeDirs, []string{"/opt/python"}) {
			t.Fatalf("exec settings = %+v", c.Global)
		}
	}
}

func TestOmittedExecSettingsPreserveDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte("name: react\nbase:\n  llm:\n    model: test\n    api_key: test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Global.DiscoverHostCommands || cfg.Global.ExecCommands != nil || cfg.Global.RuntimeDirs != nil {
		t.Fatalf("unexpected default authorization: %+v", cfg.Global)
	}
}

func TestLoadExecNetworkAndEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte("name: react\nexec_network: host\nexec_env: [CODEX_TEST_MYSQL_URL]\nbase:\n  llm:\n    model: test\n    api_key: test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*config.AgentConfig{cfg, cfg.ForAgent("react"), cfg.ForAgent("observe")} {
		if c.Global.ExecNetwork != "host" || !reflect.DeepEqual(c.Global.ExecEnv, []string{"CODEX_TEST_MYSQL_URL"}) {
			t.Fatalf("exec policy=%+v", c.Global)
		}
	}
}

func TestLoadRejectsInvalidExecPolicy(t *testing.T) {
	for _, settings := range []string{
		"exec_network: bridge\n",
		"exec_env: ['BAD=VALUE']\n",
		"exec_env: ['']\n",
		"exec_env: [HOME]\n",
		"exec_env: [PATH]\n",
		"exec_env: [GOPROXY]\n",
	} {
		t.Run(settings, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.yaml")
			if err := os.WriteFile(path, []byte(settings+"name: react\nbase:\n  llm:\n    model: test\n    api_key: test\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := config.LoadAgentConfig(path); err == nil {
				t.Fatal("accepted invalid exec policy")
			}
		})
	}
}
