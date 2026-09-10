# goer-agent-sdk

[English](README.md) · **简体中文**

一个不绑定具体模型厂商的 agent 运行时。`BaseAgent` 驱动任意 OpenAI 兼容的对话模型进入
「工具调用循环」，直到某个 **end tool** 成功返回为止。

设计取向很明确：**只有 end tool 能结束一次 run**。纯文本回答永远不会结束 run，模型必须显式地
调用工具把任务「交出去」，因此收尾动作（校验、产出最终内容、失败重试）都集中在工具里，
运行时只负责循环、事件和上下文。

---

## 1. 核心概念

| 概念 | 说明 |
| --- | --- |
| `BaseAgent` | 一个 agent 实例 = 一次对话轮次。持有一份 transcript，跑一轮 `Run`。 |
| `Tool` | `Name` / `Description` / `Execute`，由运行时注册并转成 OpenAI function 定义。 |
| **end tool** | 构造函数必须传入的终止工具。它成功返回 = run 结束；失败 = run 继续。 |
| `Msg` | 运行时向调用方抛出的事件（进度、心跳、终态等）。 |
| `ToolResult` | 工具返回值，区分「进模型上下文的内容」(`ModelContent`/`ModelData`) 与「给前端的事件」(`Events`)。 |
| `MemoryModule` | 可注入的记忆模块：注册工具 + 往 system prompt 追加一段记忆块。 |
| `PlanModule` | 可注入的计划模块：注册工具、追加提示词、每次 run 前 reset、收尾前校验计划。 |

一次 run 的流程：

```
Run(ctx, input)
  └─ 循环（默认最多 66 轮）
       ├─ 组装消息（system prompt + 语言要求 + 记忆块 + end-tool 规则 + transcript）
       ├─ 流式调用 LLM（失败退避重试 3 次）
       ├─ assistant 回复里没有 tool_calls → 纠正提示，继续（连续 2 次后强制 tool_choice=required）
       └─ assistant 回复里有 tool_calls → 顺序执行
            ├─ 命中 end tool 且成功 → 结束，输出最终内容（run_done）
            ├─ 命中 end tool 但失败/报错 → 继续循环
            └─ 普通工具 → 结果写回 transcript，继续循环
```

---

## 2. 目录结构与文件职责

模块名 `github.com/excelmatic/goer-agent-sdk`，根目录包名 `base`（子包 `ctxkey`、`xlog`）。

| 文件 | 职责 |
| --- | --- |
| `agent.go` | `Agent` 接口、`BaseAgent` 结构体、构造函数、所有 `With*` 配置项、工具注册 |
| `run.go` | `Run` / `Stop`、主循环、run 生命周期（并发保护、终态事件）、plan 校验 |
| `toolcall.go` | 单轮工具调用执行、end tool 判定、跳过占位、工具上下文注入 |
| `llm.go` | LLM 客户端构建、流式调用与 delta 合并、重试退避、工具 schema 下发、消息清洗 |
| `prompt.go` | system prompt 拼装与全部提示词模板、停止文案 |
| `history.go` | transcript 读写（深拷贝）、工具结果序列化与体积截断 |
| `events.go` | 事件投递（可取消 / 终态兜底）、心跳 |
| `log.go` | 带 `chat_id` 的日志、日志用 JSON 压缩与文本截断 |
| `constants.go` | 所有运行时默认值（迭代上限、缓冲、重试、阈值等） |
| `tool.go` | `Tool`/`ToolResult`/`Msg`/`ToolEventEmitter` 定义、`MsgType*` 常量、OpenAI schema 辅助函数 |
| `summary.go` | `SummarizeMessages`：把一次 run 的事件汇总成入库用的答案/可见内容/HTML |
| `attachment.go` | 产品侧消息约定（chart 附件、task_completed 记录） |
| `skill.go` | **未接线**：`Skill`/`BaseSkill`，见「已知限制」 |
| `ctxkey/` | 工具侧读取上下文的 key：`ChatID`、`AgentHistory`、`ToolEventEmitter` |
| `xlog/` | 极简分级日志，默认 `info`，可 `SetLevel` |
| `*_test.go` | 单元测试 + 本地 `httptest` 假 LLM 的端到端 run 测试 |

---

## 3. 依赖：必须带上 `replace`

SDK 需要 `ChatCompletionRequest.ExtraBody` 和 OpenAI 风格 `reasoning` 字段，只有
`github.com/neugls/go-openai` 这个 fork 提供；而该 fork 的 `go.mod` 声明的仍是上游模块路径，
所以只能用 `replace` 选中它：

```
replace github.com/sashabaranov/go-openai => github.com/neugls/go-openai v1.42.0-reasoning-extra-body
```

**`replace` 不会被下游继承。** 任何 import 本 SDK 的模块都要在自己的 `go.mod` 里重复这一行，
否则会解析到上游 `sashabaranov/go-openai`，编译时报缺少 `ExtraBody` / `reasoning`。
如果 SDK 目录不在同一仓库内，还需要一行
`replace github.com/excelmatic/goer-agent-sdk => ../goer-agent-sdk`。

长期方案：把这个 fork 发布成自建 module path（如 `github.com/excelmatic/go-openai`），
或把所需能力 upstream 化。

---

## 4. 快速开始

> 下面示例里的 `base` 指向本 SDK，`ctxkey` 是它的子包：
>
> ```go
> import (
> 	base   "github.com/excelmatic/goer-agent-sdk"
> 	"github.com/excelmatic/goer-agent-sdk/ctxkey"
> 	"github.com/sashabaranov/go-openai"
> )
> ```

### 4.1 实现一个 end tool

```go
type finishTool struct{}

func (finishTool) Name() string        { return "finish" }
func (finishTool) Description() string { return "任务完成时调用，提交最终答案" }

// 想自定义 JSON Schema 时实现 OpenAIFunctionProvider：
//   func (finishTool) OpenAIFunctionDefinition() *openai.FunctionDefinition { ... }
func (finishTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	answer, _ := args["answer"].(string)
	if strings.TrimSpace(answer) == "" {
		// 校验不通过 → 返回失败，run 会把这条结果喂回模型继续做
		return base.ToolResult{Success: false, Error: "answer is required"}, nil
	}
	return base.ToolResult{Success: true, ModelContent: answer}, nil
}
```

> 校验就放在这里。end tool 说成功才算成功，说失败 run 就继续——不再有额外的「最终答案校验钩子」。

### 4.2 跑起来

```go
agent := base.NewBaseAgent(
	"report-agent",                 // name
	"生成报表的 agent",              // description
	"You are a data analyst...",    // system prompt
	"gpt-4o",                       // model
	os.Getenv("LLM_TOKEN"),         // auth token
	os.Getenv("LLM_BASE_URL"),      // base URL，留空用库默认端点
	finishTool{},                   // 必填：至少一个 end tool
)

agent.WithEndTools(abortTool{})              // 可选：再追加 end tool
agent.WithModel("gpt-4.1")                   // 可选：覆盖模型
agent.WithLang("zh-CN")                      // 可选：强制回答语言
agent.WithReasoningEffort("medium")          // 可选：开启 reasoning 请求参数
agent.WithToolResultMaxBytes(128 * 1024)     // 可选：单条工具结果进上下文的体积上限
agent.WithHTTPClient(httpClient)             // 可选：自定义 transport / 代理
agent.WithHTTPHeaders(map[string]string{     // 可选：网关需要的额外请求头
	"X-OpenRouter-Title": "excelmatic",
})
agent.WithMemory(memoryModule)               // 可选：记忆模块
agent.WithPlanModule(planModule)             // 可选：计划模块
agent.SetMaxIterations(40)                   // 可选：单次 run 的轮次上限

ctx, cancel := context.WithCancel(context.Background())
defer cancel()

for msg := range agent.Run(ctx, "帮我总结这张表") {
	switch msg.Type {
	case base.MsgTypeProgressUpdate:
		// 流式增量，可转发给前端
	case base.MsgTypeMarkdown:
		// 最终答案
	case base.MsgTypeRunDone:
		fmt.Println("answer:", msg.Content)
	case base.MsgTypeRunError:
		fmt.Println("error:", msg.Content)
	case base.MsgTypeRunStopped:
		fmt.Println("stopped:", msg.Content)
	}
}

history := agent.History() // 交给外部持久化
```

### 4.3 工具里发事件、读上下文

```go
func (t chartTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	// 1) 往产品侧发事件（不自动进模型上下文）
	if emit, ok := ctx.Value(ctxkey.ToolEventEmitter).(base.ToolEventEmitter); ok {
		emit(base.Msg{Type: base.MsgTypeChartResult, Data: map[string]any{
			"chart_id":     "sales",
			"chart_option": option,
		}})
	}

	// 2) 读当前 transcript（副本，改不动运行时状态）
	history, _ := ctx.Value(ctxkey.AgentHistory).([]openai.ChatCompletionMessage)
	_ = history

	// 3) 告诉运行时用户提问的语言，后续轮次会按该语言作答
	return base.ToolResult{
		Success:      true,
		ModelContent: "chart generated",                  // 进模型上下文
		ModelData:    map[string]any{"rows": 120},        // 进模型上下文（结构化）
		Meta:         map[string]any{base.ToolMetaUserQueryLanguageKey: "zh-CN"},
	}, nil
}
```

---

## 5. 运行契约

1. **构造函数必传 end tool**：一个都没有就 `panic`。
2. **只有 end tool 成功返回才能结束 run**：纯文本回答会被写回一条纠正消息并继续迭代。
3. end tool 返回 `Success=false` 或执行返回 error，都不结束 run（结果会喂回模型）。
4. 连续 2 次纯文本回答 → 下一次请求强制 `tool_choice=required`；连续 5 次 → run 以 `run_error` 结束。
5. 同一轮里排在 end tool 之后的工具调用不会执行，但会写入「已跳过」占位结果，保证
   `tool_calls` 与 tool 消息一一对应（否则下次请求会被 OpenAI 兼容接口以 400 拒绝）。
6. LLM 调用失败按 500ms→1s 退避重试 3 次；失败原因用 `%w` 保留在 `run_error` 里。
7. 工具 panic 会被 recover 成 `run_error`，不会掀翻进程，run 槽位一定会释放。
8. `Run` 返回的 channel 在终态事件之后关闭；终态一定是 `run_done` / `run_error` / `run_stopped` 之一。
9. 事件投递随 run 上下文取消；终态事件用 5s 兜底投递——消费方中途不读了也不会泄漏 goroutine。

### 发往模型的消息会被清洗

发送前 `sanitizeMessagesForLLM` 会做三件事（对应部分 provider 的兼容要求）：
`Name` 字段清空、空 `Content` 补一个空格、空 `ReasoningContent` 补一个空格。
所以「内存里的 transcript」与「实际发出的消息」在这一点上不完全一致。

---

## 6. 事件与终态

| 事件 | 触发时机 | 主要字段 |
| --- | --- | --- |
| `start` | run 开始 | — |
| `progress_update` | 流式回答 / 推理内容增量 | `Content` |
| `heartbeat` | 每 1s（消费慢时自动丢弃） | `Data["timestamp"]` |
| `markdown` | 收尾输出最终答案 | `Content` |
| `run_done` | 终态：成功 | `Content` 与 `Data["answer"]` |
| `run_error` | 终态：失败 | `Content` 为错误原因 |
| `run_stopped` | 终态：`Stop()` 或 ctx 取消 | `Content` 为停止文案，`Data["reason"]` 为原因 |

工具还可以自行 emit 产品侧事件：`chart_result`、`task_completed`、`dashboard_html`、
`report_start`、`report_end`。`SummarizeMessages` 会把它们汇总成入库用结构：

```go
summary := base.SummarizeMessages(msgs)
summary.Answer         // 最终答案（有可见内容时以可见内容为准）
summary.VisibleContent // markdown / task_completed / chart 附件 / 停止文案 拼接
summary.HtmlContent    // dashboard_html
```

---

## 7. API 参考

### 构造

```go
func NewBaseAgent(name, description, systemPrompt, model, authToken, baseURL string, endTools ...Tool) *BaseAgent
```

`baseURL` 为空时用库默认端点；`endTools` 至少要有一个可用工具（`nil` 或空名字会被跳过）。

### 配置（都返回 `*BaseAgent`，可链式调用）

| 方法 | 作用 |
| --- | --- |
| `WithModel(model)` | 覆盖模型 |
| `WithSystemPrompt(prompt)` | 覆盖基础 system prompt |
| `WithLang(lang)` | 强制回答语言（会拼进 system prompt） |
| `WithReasoningEffort(effort)` | 开启 reasoning 请求参数；空串关闭 |
| `WithEndTool(tool)` / `WithEndTools(tools...)` | 追加 end tool |
| `WithMemory(module)` | 注入记忆模块（注册工具 + 追加记忆块） |
| `WithPlanModule(module)` | 注入计划模块（注册工具 + 追加提示词 + 每轮 reset + 收尾校验） |
| `WithHTTPClient(client)` | 自定义 HTTP client（代理、transport、连接池） |
| `WithHTTPHeaders(headers)` | 给每个 LLM 请求附加请求头 |
| `WithToolResultMaxBytes(n)` | 单条工具结果进上下文的体积上限，`0` 关闭 |
| `SetMaxIterations(n)` | 单次 run 的轮次上限（默认 66） |

### 运行

| 方法 | 说明 |
| --- | --- |
| `Run(ctx, input) chan Msg` | 启动一次 run，返回事件 channel，需读到关闭 |
| `Stop()` | 取消当前 run，最终会收到 `run_stopped` |

### 历史与内省

| 方法 | 说明 |
| --- | --- |
| `WithHistory(history)` | 注入上一轮 transcript（**深拷贝**入参） |
| `History()` | 读取当前 transcript（**返回副本**） |
| `HasState()` | 是否已有历史 |
| `AddTool(tool)` | 注册普通工具（`nil`/空名字忽略） |
| `GetTool(name)` / `GetTools()` | 查询工具（`GetTools` 返回副本） |
| `EndToolNames()` | 排序后的 end tool 名字 |
| `Name()` / `Description()` / `SystemPrompt()` / `Lang()` | 基础信息 |

### 关键类型

```go
type Tool interface {
	Name() string
	Description() string
	Execute(ctx context.Context, args map[string]any) (ToolResult, error)
}

type ToolResult struct {
	Success      bool            // 是否成功；end tool 成功才结束 run
	Error        string          // 失败原因（会进模型上下文）
	Meta         map[string]any  // 运行时可识别的元信息，如 user_query_language
	ModelContent string          // 进模型上下文的内容
	ModelData    map[string]any  // 进模型上下文的结构化数据
	Events       []Msg           // 给产品侧的事件，不进模型上下文
}

type Msg struct {
	Type    string
	Content string
	Name    string
	Data    map[string]any
}

type ToolEventEmitter func(Msg)
```

### 运行时默认值（`constants.go`）

| 常量 | 值 | 含义 |
| --- | --- | --- |
| `defaultMaxIterations` | 66 | 单次 run 的 assistant 轮次上限 |
| `eventChannelBuffer` | 256 | `Run` 返回 channel 的缓冲 |
| `finalEventDeliveryTimeout` | 5s | 终态事件投递兜底超时 |
| `heartbeatInterval` | 1s | 心跳间隔 |
| `logContentMaxRunes` | 2000 | 日志中内容截断长度 |
| `defaultToolResultMaxBytes` | 256 KiB | 单条工具结果进上下文的体积上限 |
| `noToolCallEscalateAfter` | 2 | 连续多少次纯文本后强制 `tool_choice=required` |
| `noToolCallFailAfter` | 5 | 连续多少次纯文本后 run 失败 |
| `llmMaxAttempts` | 3 | 单次 LLM 调用尝试次数 |
| `llmRetryBaseDelay` / `llmRetryMaxDelay` | 500ms / 5s | 重试退避区间 |

---

## 8. 历史与持久化

SDK **只负责 run 期间的状态**，跨轮次持久化由调用方负责（存库、存 Redis 都行）：

```go
// 上一轮结束后
save(chatID, agent.History())

// 新一轮开始前
agent := base.NewBaseAgent(..., finishTool{}).WithHistory(load(chatID))
```

建议「一次对话轮次 = 一个 agent 实例」。入库时务必完整保留 `role`、`content`、
`tool_calls`、`tool_call_id`：如果历史里只有「助手说要调工具」而没有配对的 tool 结果，
续话时 OpenAI 兼容接口会直接 400（运行时的「跳过占位」只在一次 run 内兜底）。

`AgentContextSnapshot` 提供了可直接序列化的形态：

```go
type AgentContextSnapshot struct {
	History []openai.ChatCompletionMessage `json:"history,omitempty"`
}
```

上下文只增不减，长对话建议自己裁剪，或把裁剪策略放进 `MemoryModule`。

---

## 9. 并发模型

`BaseAgent` 持有 run 级状态（transcript、语言、记忆块、cancelFunc），所以：

- **一次只跑一个 run**：并发调用 `Run` 时，第二个调用立刻返回一条
  `"agent is already running: one BaseAgent instance handles a single run at a time"` 的 `run_error`，不会互相污染；
- `WithHistory` 深拷贝、`History()`/`GetTools()` 返回副本，agent 与调用方不共享可变切片；
- 工具执行发生在 run 的 goroutine 内，工具内部并发请自行保证安全。

---

## 10. 日志与可观测

```go
xlog.SetLevel(xlog.ParseLevel(os.Getenv("GOER_AGENT_LOG_LEVEL"))) // debug/info/warn/error/off
```

- 默认 `info`；`logMessageSizes`（每轮消息体积与 token 估算）是 `debug` 级；
- 日志行统一带 `chat_id=`（从 `ctxkey.ChatID` 取）；
- 模型输出、工具参数与结果都会按 `logContentMaxRunes` 截断，避免日志里出现整段数据。

---

## 11. 测试

```bash
go test ./...
go test -race ./...
```

20 个用例，核心是一个本地 `httptest` 假 LLM（SSE 流式 + 记录请求体），因此可以断言
`tool_choice`、消息内容、请求头、调用次数。覆盖：构造约束、end-tool 契约、纯文本重试与升级、
end tool 失败不结束、跳过占位、取消终态、panic 恢复、LLM 重试、工具结果截断、历史拷贝、
自定义请求头、并发拒绝、工具回传语言。

---

## 12. 已知限制与待办

1. **`skill.go` 未接线**：`Skill`/`BaseSkill` 没有被 `BaseAgent` 使用，`AllowedTools` 不生效，
   `Instruction`/`Model`/`MaxIterations` 也不会影响 run。要么接进 `BaseAgent`（限制工具白名单、
   应用指令与模型设置），要么删除。
2. **fork 依赖**：见第 3 节，`replace` 必须由每个使用方重复声明。
3. **无版本控制**：该目录目前不是 git 仓库，改动无法追溯与回滚。
4. **上下文无限增长**：运行时不自动裁剪 transcript（工具结果有体积上限，历史没有）。
5. **产物约定混在 SDK 内**：`attachment.go` 里的 chart/dashboard 约定属于产品侧，
   通用场景可忽略或自行替换汇总逻辑。

---

## 13. 与后端 `orchestratorv2/agent/base` 的关系

本 SDK 与 `golang-backend/agents/orchestratorv2/agent/base` 同源：后端那份保留了 `llm.Client()`
（读环境变量）、`locales`、`utils` 等平台依赖，本 SDK 则把 token/baseURL、记忆块、日志、
HTTP 客户端都参数化，便于独立复用。两边的 end-tool 语义（构造函数必传、纯文本不结束、
成功才结束、失败继续）保持一致，改动时建议同步。
