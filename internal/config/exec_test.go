package config_test

import (
	"flag"
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

func TestLoadDependencyCacheConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	data := "name: react\nexec_go_proxy: https://proxy.golang.org,direct\nexec_caches:\n  - name: go-mod\n    directory: /tmp/agent-go-mod\n    env: [GOMODCACHE]\nbase:\n  llm:\n    model: test\n    api_key: test\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Global.ExecGoProxy != "https://proxy.golang.org,direct" || len(cfg.Global.ExecCaches) != 1 || cfg.Global.ExecCaches[0].Name != "go-mod" || cfg.ForAgent("react").Global.ExecCaches[0].Env[0] != "GOMODCACHE" {
		t.Fatalf("dependency settings = %+v", cfg.Global)
	}
}

func TestDependencyFlagsOverrideYAMLAndRejectMalformedCaches(t *testing.T) {
	var proxy string
	var caches []config.ExecCache
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	config.RegisterExecDependencyFlags(flags, &proxy, &caches)
	args := config.ExecDependencyArgs("off", []config.ExecCache{})
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	if proxy != "off" || caches == nil || len(caches) != 0 {
		t.Fatalf("proxy=%q caches=%v", proxy, caches)
	}
	if err := flags.Parse([]string{"--exec-cache", `{"name":"pip","directory":"/tmp/pip","env":["PIP_CACHE_DIR"]} trailing`}); err == nil {
		t.Fatal("accepted trailing cache configuration")
	}
}

func TestLoadRejectsInvalidDependencyCaches(t *testing.T) {
	for _, caches := range []string{
		"[{name: '../escape', directory: /tmp/cache}]",
		"[{name: pip, directory: relative}]",
		"[{name: pip, directory: /tmp/cache, env: [HOME]}]",
		"[{name: pip, directory: /tmp/cache, env: [GOPROXY]}]",
		"[{name: pip, directory: /tmp/cache, env: [PIP_CACHE_DIR]}, {name: other, directory: /tmp/other, env: [PIP_CACHE_DIR]}]",
		"[{name: pip, directory: /tmp/cache}, {name: pip, directory: /tmp/other}]",
	} {
		t.Run(caches, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.yaml")
			if err := os.WriteFile(path, []byte("name: react\nexec_caches: "+caches+"\nbase:\n  llm:\n    model: test\n    api_key: test\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := config.LoadAgentConfig(path); err == nil {
				t.Fatal("accepted invalid cache configuration")
			}
		})
	}
}
