package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
)

func redisConfigFile(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	data := "name: react\nbase:\n  llm:\n    model: test\n    api_key: test\n" + extra
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRedisYAMLValidation(t *testing.T) {
	for _, block := range []string{
		"redis: {}\n",
		"redis:\n  addr: localhost\n",
		"redis:\n  addr: localhost:70000\n",
		"redis:\n  addr: localhost:6379\n  db: -1\n",
		"redis:\n  addr: localhost:6379\n  password: secret\n  password_env: TEST_REDIS_PASSWORD\n",
		"redis:\n  addr: localhost:6379\n  password_env: TEST_MISSING_REDIS_PASSWORD\n",
		"redis:\n  addr: localhost:6379\n  typo: true\n",
	} {
		if _, err := config.LoadAgentConfig(redisConfigFile(t, block)); err == nil {
			t.Fatalf("accepted invalid Redis YAML: %s", block)
		}
	}
	cfg, err := config.LoadAgentConfig(redisConfigFile(t, ""))
	if err != nil || cfg.RedisConfig() != nil {
		t.Fatalf("Redis should be opt-in: cfg=%+v err=%v", cfg, err)
	}
}

func TestRedisYAMLConfigIsGlobal(t *testing.T) {
	t.Setenv("TEST_REDIS_PASSWORD", "secret")
	cfg, err := config.LoadAgentConfig(redisConfigFile(t, "redis:\n  addr: 127.0.0.1:6380\n  username: agent\n  password_env: TEST_REDIS_PASSWORD\n  db: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"react", "observe", "plan"} {
		child := cfg.ForAgent(name)
		if child.RedisConfig() == nil || child.RedisConfig().Addr != "127.0.0.1:6380" || child.RedisConfig().Username != "agent" || child.RedisConfig().Password != "secret" || child.RedisConfig().DB != 2 {
			t.Fatalf("%s Redis config=%+v", name, child.RedisConfig())
		}
		if child.Global != cfg.Global || cfg.Copy().Global != cfg.Global {
			t.Fatal("agent copies must share the global configuration")
		}
	}
}

func TestRedisCannotBeConfiguredPerAgent(t *testing.T) {
	for _, block := range []string{
		"base:\n  llm:\n    model: test\n    api_key: test\n  redis:\n    addr: localhost:6379\n",
		"base:\n  llm:\n    model: test\n    api_key: test\nagents:\n  react:\n    redis:\n      addr: localhost:6379\n",
	} {
		path := filepath.Join(t.TempDir(), "agent.yaml")
		if err := os.WriteFile(path, []byte("name: react\n"+block), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := config.LoadAgentConfig(path); err == nil {
			t.Fatal("accepted agent-specific Redis configuration")
		}
	}
}
