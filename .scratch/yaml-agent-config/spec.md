# YAML 多代理配置

Status: needs-info

## 目标与术语

一个 YAML 文件提供基础代理配置（base）和按代理类型划分的独立配置。支持 `react`、`task_analyzer`、`observe` 三种配置类型，不引入同类型的多个命名实例。领域术语见根目录 `GLOSSARY.md`。

原单代理版本已由票据 01–03 完成。本扩展不兼容旧 YAML 格式；旧票据保留历史完成记录，新实施工作见票据 04–06。

## YAML 格式

```yaml
name: react
base:
  llm:
    base_url: https://api.openai.com/v1
    api_key_env: OPENAI_API_KEY
    model: base-model
    output_schema: json_llm_output
  allow_tools: [read_file, list_dir]
  sys_prompt: "通用约束"
agents:
  react:
    allow_tools: []
    sys_prompt: "ReAct 专用约束"
  task_analyzer:
    llm:
      api_key_env: ANALYZER_API_KEY
      model: analyzer-model
    sys_prompt: "分析器专用约束"
  observe:
    sys_prompt: ""
```

- 顶层仅支持 `name`、`base`、`agents`。`name` 必填，选择启动的代理类型，本次仍只支持 `react`。
- `base` 必填，且必须包含完整可用的 `llm`；`agents` 可省略，省略或空映射表示全部类型使用 base。
- `agents` 的合法键为 `react`、`task_analyzer`、`observe`；每个值仅支持 `llm`、`allow_tools`、`sys_prompt`。空映射等同于未提供任何独立设置。
- 一个文件只能包含一个 YAML 文档。旧格式中的顶层 `llm`、`allow_tools`、`sys_prompt` 作为未知字段报错，不自动迁移。

## 继承与覆盖

- `llm`、`allow_tools`、`sys_prompt` 分别判断是否提供；省略的设置采用 base 的对应设置。
- 每个代理只读取自身独立配置和 base，不继承其他代理的独立配置。例如观察器不能继承 `agents.react.llm`。
- `llm` 整块替换：独立配置提供 `llm` 时，不继承 base 的任何 LLM 子字段；对该独立块应用 LLM 必填项、默认值和密钥规则。
- `allow_tools` 整个列表替换，不与 base 合并；显式 `[]` 禁用所有工具。
- 独立 `sys_prompt` 替换 base 的自定义提示词，不拼接两份自定义内容；显式 `""` 清除 base 自定义提示词。
- 最终非空自定义提示词追加到对应代理的内置提示词后；空值仍保留对应代理的内置提示词。
- 所有配置字段均拒绝显式 YAML `null`，以免与省略混淆；`llm: {}` 因缺少模型报错。
- base 省略 `allow_tools` 时禁用所有工具，省略 `sys_prompt` 时不追加自定义提示词。

## LLM 规则与启动校验

以下规则对 base 的 LLM 和每个独立 LLM 块分别应用：

- `model` 必填；`base_url` 默认 `https://api.openai.com/v1`。
- `output_schema` 默认 `json_llm_output`，另支持 `line_protocol_llm_output`。
- 支持 `api_key` 明文密钥或 `api_key_env` 环境变量名；两者同时填写时报错。
- 两个密钥字段均未填写时读取 `OPENAI_API_KEY`；最终密钥为空时报错。错误信息不得包含密钥值。
- 启动时校验 base 和所有独立配置，包括未被本次流程调用的代理。base 自身必须有效，不能由各代理独立配置补全无效 base。
- 文件不存在、格式错误、未知字段、未知代理类型、未知工具、未知输出格式、必填项缺失及显式 null 均报告明确错误。
- 已知工具：`read_file`、`list_dir`、`glob`、`grep`、`apply_patch`。

## 各代理的能力边界

- `react` 使用解析后的模型、工具允许列表和自定义提示词；模型端只暴露允许的工具，执行端拒绝未允许的工具调用。
- `task_analyzer`、`observe` 使用各自解析后的模型与提示词，始终不暴露或执行工具。base 中的工具列表不赋予这两类代理工具能力。
- 显式为 `task_analyzer` 或 `observe` 配置非空 `allow_tools` 时启动报错；省略或 `[]` 均允许。这两类代理的最终有效工具集合始终为空。
- ReAct 内部启动观察器时必须使用观察器配置；观察器接通自己的自定义提示词。
- 为现有任务分析器调用接口提供独立配置解析路径，不新增其在 CLI 主流程中的调度。
- 解析和运行配置之间保留清晰边界；解析后的独立配置不应因另一个代理设置提示词或工具而被修改。

## 启动与运行

- `--config <路径>` 显式加载新格式配置；未指定时保留现有 MCP stdio server 模式。
- Agent 模式要求非空 `--task`，不增加交互输入；`--workspace` 指定工作目录，`--timeout` 指定工具超时，默认 30 秒。这些设置不进入 YAML。
- CLI 与 `cmd/agent-server` 均迁移到新配置入口。
- 最大执行轮数仍为代码 const 固定 100，不增加 YAML 或 CLI 配置项。
- 完成后输出最终答案并成功退出；执行失败或达到轮数上限时报告原因并返回非零退出码。沿用内部代理现有触发和运行失败处理方式。
- 示例：`go run ./cmd --config examples/react-agent.yaml --task "任务内容" --workspace /path`。

## 验证与实施约束

- 验证逐项继承、整块 LLM 替换、工具和提示词显式清空、未配置代理回退、null 拒绝及全量校验；包含未使用代理缺少密钥的启动错误。
- 使用可控模型响应验证主代理和观察器实际发送的模型、提示词及工具；直接验证任务分析器独立配置，避免依赖新增调度。
- 更新旧格式测试夹具、示例和 README；保留无配置 MCP 入口、工具执行与拒绝、轮数与退出语义的回归覆盖。
- 关键的继承和工具能力边界加注释。保留既有用户改动，尤其是任务分析器相关未提交文件。
- 本次设计整理不修改运行代码；实施后执行 `go test ./...`。

## 确认记录

- Q1–Q3：`all confirm`，按类型配置、三个设置分别回退、显式空列表与空提示词清空。
- Q4：`confirm`，LLM 整块替换。
- Q5：`不用兼容旧格式`，采用 `name` / `base` / `agents` 单文件结构，不兼容旧格式。
- Q6–Q8：`all confirm`，覆盖三个处理器，base 必填且完整，启动校验全部配置，拒绝 null。
- Q9：`confirm`，两个内部代理保持无工具能力，显式非空工具列表报错。
- 设计问题已收敛，等待用户确认整体理解后进入实现；实施票据暂标记 `needs-info`。
