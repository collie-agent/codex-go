package config

type McpToolName string

const (
	ReadFileMCPToolName   McpToolName = "read_file"
	ListDirMCPToolName    McpToolName = "list_dir"
	GlobMCPToolName       McpToolName = "glob"
	GrepMCPToolName       McpToolName = "grep"
	ApplyPatchMCPToolName McpToolName = "apply_patch"
	ExecMCPToolName       McpToolName = "exec"
)
