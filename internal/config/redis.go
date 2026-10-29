package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// GlobalConfig 保存整个运行共用的设置，加载后按只读配置使用。
// agent 的独立配置不能覆盖这里的值。
type GlobalConfig struct {
	Redis *RedisConfig
}

// RedisConfig 返回全局 Redis 设置；未配置时禁用 Stream 输出。
func (c *AgentConfig) RedisConfig() *RedisConfig {
	if c == nil || c.Global == nil {
		return nil
	}
	return c.Global.Redis
}

// RedisConfig 属于整个运行；省略顶层 redis 时不启用 Stream 输出。
type RedisConfig struct {
	Addr     string
	Username string
	Password string
	DB       int
}

type yamlRedisConfig struct {
	Addr        string `yaml:"addr"`
	Username    string `yaml:"username"`
	Password    string `yaml:"password"`
	PasswordEnv string `yaml:"password_env"`
	DB          int    `yaml:"db"`
}

func resolveRedis(raw *yamlRedisConfig) (*RedisConfig, error) {
	if raw == nil {
		return nil, nil
	}
	host, port, err := net.SplitHostPort(raw.Addr)
	number, portErr := strconv.Atoi(port)
	if err != nil || strings.TrimSpace(host) == "" || portErr != nil || number < 1 || number > 65535 {
		return nil, fmt.Errorf("redis.addr must be a host:port with port 1-65535")
	}
	if raw.DB < 0 {
		return nil, fmt.Errorf("redis.db must not be negative")
	}
	if raw.Password != "" && raw.PasswordEnv != "" {
		return nil, fmt.Errorf("redis.password and redis.password_env are mutually exclusive")
	}
	password := raw.Password
	if raw.PasswordEnv != "" {
		password = os.Getenv(raw.PasswordEnv)
	}
	if raw.PasswordEnv != "" && password == "" {
		return nil, fmt.Errorf("redis.password_env is empty or unset")
	}
	return &RedisConfig{Addr: raw.Addr, Username: raw.Username, Password: password, DB: raw.DB}, nil
}
