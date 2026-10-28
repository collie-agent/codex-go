package config

type LogLevel string

const (
	DebugLogLevel LogLevel = "debug"
	InfoLogLevel  LogLevel = "info"
	ErrorLogLevel LogLevel = "error"
)

// InfoEnabled 保持直接构造配置时的零值与 YAML 默认值一致。
func (level LogLevel) InfoEnabled() bool {
	return level == "" || level == InfoLogLevel || level == DebugLogLevel
}
