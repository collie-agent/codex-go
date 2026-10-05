# 保留 atom-mcp 的补丁匹配语义

`apply_patch` 使用 Go 移植 atom-mcp 的 unified diff 解析和确定性匹配规则。匹配按精确文本、忽略行尾空白、忽略首尾空白、有限标点归一化的顺序尝试，并从前一块之后向前搜索，以兼容参考实现对过时行号和文本差异的处理；这些规则需要显式保留，后续改用系统补丁命令时必须重新评估兼容性。

所有文件的路径和补丁内容在开始写入前完成验证；写入阶段失败不会回滚已完成的修改，错误结果报告已完成和失败的路径。所有工具使用启动时指定的工作目录，补丁接口不提供独立的 `cwd`。

参考：[atom-mcp 补丁匹配决策](/home/xzp/workspace/atom-mcp/docs/adr/0005-deterministic-patch-hunk-matching.md)。
