package config

type McpToolName string

const (
	LoadSkillMCPToolName           McpToolName = "load_skill"
	SwitchToPlanExecuteMCPToolName McpToolName = "switch_to_plan_execute"
	ReadFileMCPToolName            McpToolName = "read_file"
	ListDirMCPToolName             McpToolName = "list_dir"
	GlobMCPToolName                McpToolName = "glob"
	GrepMCPToolName                McpToolName = "grep"
	ApplyPatchMCPToolName          McpToolName = "apply_patch"
	ExecMCPToolName                McpToolName = "exec"
)
