# goer-agent-sdk

[English](README.md) · **简体中文**

把任意 OpenAI 兼容模型变成一个**只用工具报告完成**的 agent。

你注册工具，模型在循环里调用它们；只有当某个 **end tool** 成功返回时，这一轮才结束。纯文本回答
永远结束不了任务——「做完了没有」由你的代码判定，不由模型的措辞判定。

```go
agent := base.NewBaseAgent(
	"report-agent", "Generates reports", systemPrompt,
	"gpt-4o", os.Getenv("LLM_TOKEN"), os.Getenv("LLM_BASE_URL"),
	finishTool{}, // end tool：这一轮唯一的结束方式
)
agent.AddTool(aggregateTool{}) // 过程中可以调的普通工具

stream, _ := agent.Run(ctx, "统计这张表并给出结论")
for msg := range stream {
	forward(msg) // reasoning = 思考过程，content = 回答正文
}
fmt.Println("answer:", agent.Result().Answer)
```

---

## 设计思路

### 一、一次任务 = 一个有终点的工具循环

```
用户输入
  ↓
构建请求 = system prompt + transcript
  ↓
调模型 ─┬─ 要调工具 ─→ 执行工具 ─→ 结果写回 transcript ─┐
        └─ 只回文本 ─→ 退回并附纠正提示 ───────────────┤
                                                       ↓
                        回到「调模型」（默认最多 66 轮）◀┘
                                                       ↓
                          end tool 成功 ─→ 结束，交付 ModelContent
```

由此推出三条硬规则：

1. 只有成功的 end tool 能结束一轮 run；纯文本回答会被退回，要求调用工具。
2. end tool 失败（`Success=false` 或返回 error）**不**结束，模型看到失败原因后继续。
3. 交付给用户的答案是 end tool 的 `ModelContent`，而不是模型最后那段话。

### 二、两种工具

| | 普通工具 | end tool |
| --- | --- | --- |
| 怎么注册 | `AddTool(t)` | 构造函数、`WithEndTool`、`WithEndTools` |
| 干什么 | 干活：取数、计算、画图、读写记忆 | 交付：提交最终答案 |
| 成功返回 | 循环继续 | **结束本轮**，`ModelContent` 即最终答案 |
| 失败返回 | 循环继续，模型看到错误 | 循环继续，不结束 |

两者实现的是同一个 `Tool` 接口（`Name` / `Description` / `Execute`），差别只在注册时的身份。

**为什么这么设计：**

- **完成必须能被程序断言。** 让模型「说完成了」就算完成，你无法校验——它可能在差一步时宣布结束，
  也可能把「我准备这么做」写成完成。把终点收成一个工具调用后，完成变成一个确定的事件：函数被调用、
  参数能解析、你的校验能通过。
- **两种工具是两种职责。** 普通工具的输出是「给模型看的事实」，end tool 的输出是「给用户看的交付物」。
  如果只有一个工具，模型很容易查了个行数就顺手交付。
- **校验放在 end tool 里最自然。** 它是唯一出口，天然是所有交付约束的检查点（必须是 markdown 表格、
  长度上限、必填字段……）。校验不过就返回 `Success=false`，模型带着原因重来。
- **失败原因也要进上下文。** end tool 的失败原因会写回 transcript，模型才知道为什么被打回、要改什么。

### 三、什么进模型上下文，什么发给产品侧

`ToolResult` 明确分成两个通道：

| 字段 | 去哪 |
| --- | --- |
| `ModelContent` / `ModelData` | 进模型上下文 |
| `Error` | 进模型上下文（失败原因） |
| `Events` | 只发给产品侧，模型永远看不到 |
| `Meta` | 不进上下文，运行时不解释，给你自己记账 |

分开的理由：图表 option、附件 id、进度百分比这类东西进上下文既浪费 token 又干扰推理。工具需要推
产品事件时，用 `ctxkey.ToolEventEmitter` 往 run 的消息流里抛（见「在工具里读 transcript 和发事件」）。

### 四、状态归谁

| 状态 | 谁持有 |
| --- | --- |
| transcript（本轮对话） | 运行时，内存里 |
| 跨轮持久化 | 你：`History()` 取出，`WithHistory()` 灌回 |
| 同时进行的 run | 一个实例一次只跑一个 |
| 上下文压缩 | 运行时，按上下文窗口自己判断 |

一个实例 = 一次 run，所以运行时内部不需要为并发请求做任何协调；跨轮状态由你显式传入，存成什么形态
（DB、Redis、文件）完全由你决定，运行时不碰你的存储。

### 五、上下文压缩

长任务会把上下文窗口撑满。运行时不把这件事推给调用方：窗口的 60% 触发压缩，压缩后逐字保留尾部
预算的 25%，被压段替换成一条 `<compacted-history>` 消息（结构化摘要 + 逐字 user 消息清单）。触发、
边界选择、失败语义见[上下文压缩](#上下文压缩)。

---

## 安装

```
go get github.com/Mrfogg/goer-agent-sdk
```

### 必须加的 `replace` 指令

SDK 需要上游 `go-openai` 还没有的两样东西：请求体的顶层 `ExtraBody`（reasoning 模型用的 `thinking` /
`reasoning` 字段），以及解析回复时的 `reasoning` 字段。两者都在一个 fork 里，而这个 fork 仍然声明
上游模块路径，所以只能用 `replace` 选中它。

在**每个 import 了本 SDK 的模块**的 `go.mod` 里加上：

```
require github.com/sashabaranov/go-openai v1.42.0

replace github.com/sashabaranov/go-openai => github.com/neugls/go-openai v1.42.0-reasoning-extra-body
```

- 漏了不是运行时报错，是**编译**报 unknown field `ExtraBody` / `ReasoningContent`。
- Go 会忽略依赖自己的 `replace`：写在本仓库的 `go.mod` 里对使用方无效，每个使用方都要抄一遍。
- SDK 与你的代码在同一台机器上时，再加一行
  `replace github.com/Mrfogg/goer-agent-sdk => ../goer-agent-sdk`。
- fork 必须对 CI 也可达（公开或已认证），`go mod download` 会在干净机器上拉它。

如果你的上游完全不需要 reasoning 字段，也可以不调用 `WithReasoningEffort`，自己依赖上游
`go-openai`——但仓库里的源码是按 fork 编译的。要求 Go 1.23+。

---

## 快速开始

一个完整可运行的程序：一个普通工具 + 一个 end tool + 跑一轮。

```go
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	base "github.com/Mrfogg/goer-agent-sdk"
)

// 普通工具：模型干活过程中可以调它。
type rowsTool struct{}

func (rowsTool) Name() string        { return "count_rows" }
func (rowsTool) Description() string { return "Count the rows of the current sheet" }

func (rowsTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	// ModelContent 是模型下一轮会看到的内容。
	return base.ToolResult{Success: true, ModelContent: "120 rows"}, nil
}

// end tool：它成功返回，这一轮才结束。
type finishTool struct{}

func (finishTool) Name() string        { return "finish" }
func (finishTool) Description() string { return "Submit the final answer when the task is complete" }

func (finishTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	answer, _ := args["answer"].(string)
	if strings.TrimSpace(answer) == "" {
		// 失败不会结束 run：模型会看到这条错误并重试。
		return base.ToolResult{Success: false, Error: "answer is required"}, nil
	}
	return base.ToolResult{Success: true, ModelContent: answer}, nil
}

func main() {
	agent := base.NewBaseAgent(
		"report-agent",
		"Agent that summarises spreadsheets",
		"You are a data analyst. Use the tools, then call finish with the final answer.",
		"gpt-4o",
		os.Getenv("LLM_TOKEN"),    // 认证 token
		os.Getenv("LLM_BASE_URL"), // base URL，例如 https://api.openai.com/v1
		finishTool{},
	)
	agent.AddTool(rowsTool{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := agent.Run(ctx, "How many rows does this sheet have?")
	if err != nil {
		panic(err)
	}

	for msg := range stream {
		switch msg.Type {
		case base.MsgTypeReasoning:
			fmt.Print(msg.Content) // 模型的思考，流式
		case base.MsgTypeContent:
			fmt.Print(msg.Content) // 回答正文，流式
		case base.MsgTypeUsage:
			fmt.Printf("\n[usage] input=%v output=%v\n", msg.Data["prompt_tokens"], msg.Data["completion_tokens"])
		}
	}

	result := agent.Result()
	switch {
	case result.Err != nil:
		fmt.Println("\nfailed:", result.Err)
	case result.Stopped:
		fmt.Println("\nstopped by the caller")
	default:
		fmt.Println("\nanswer:", result.Answer)
	}
}
```

这一轮里发生了什么：

1. 模型回一个工具调用（`count_rows` 或 `finish`）；
2. 普通工具的结果写回 transcript，循环继续；
3. `finish` 成功后 run 结束，它的内容作为最后一条 `content` 消息发出，channel 关闭，
   `Result().Answer` 是同一份文本；
4. 模型只回文本时，运行时会退回并附上纠正提示。

更多可运行程序在 [`examples/`](examples)：`quickstart`、`tools`、`multiturn`、`streaming`、
`httpapi`、`localmock`——最后一个不需要任何凭据或网络：

```bash
go run ./examples/localmock
```

---

## 使用

### 定义普通工具

```go
type rowsTool struct{}

func (rowsTool) Name() string        { return "count_rows" }
func (rowsTool) Description() string { return "Count the rows of the current sheet" }

func (rowsTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	return base.ToolResult{
		Success:      true,
		ModelContent: "120 rows",                             // 给模型看
		ModelData:    map[string]any{"rows": 120, "cols": 3}, // 结构化，同样进上下文
	}, nil
}

agent.AddTool(rowsTool{})
```

### 定义 end tool

```go
type finishTool struct{}

func (finishTool) Name() string        { return "finish" }
func (finishTool) Description() string { return "Submit the final answer when the task is complete" }

func (finishTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	answer, _ := args["answer"].(string)
	if !strings.Contains(answer, "|") { // 交付约束就写在这里
		return base.ToolResult{Success: false, Error: "answer must contain a markdown table"}, nil
	}
	if len(answer) > 8000 {
		return base.ToolResult{Success: false, Error: "answer is too long, summarise it"}, nil
	}
	return base.ToolResult{Success: true, ModelContent: answer}, nil
}

// 构造函数至少要有一个 end tool，否则 panic。
agent := base.NewBaseAgent("analyst", "Data analyst", prompt, model, token, baseURL, finishTool{})
```

end tool 成功但 `ModelContent` 为空时，运行时会退而使用模型最后那段文本；两者都为空则加一条纠正
消息让模型重来。

### 自定义工具参数 schema

默认参数 schema 是「任意 JSON 对象」。实现 `OpenAIFunctionProvider` 可以给出严格 schema：

```go
func (finishTool) OpenAIFunctionDefinition() *openai.FunctionDefinition {
	return &openai.FunctionDefinition{
		Name:        "finish",
		Description: "Submit the final answer",
		Parameters: base.OpenAIObjectSchema(map[string]jsonschema.Definition{
			"answer": base.OpenAIStringSchema("Final answer in markdown"),
		}, "answer"),
	}
}
```

可用的构造器：`OpenAIObjectSchema`、`OpenAIArraySchema`、`OpenAIStringSchema`、`OpenAIIntegerSchema`、
`OpenAIBooleanSchema`。

### 在工具里读 transcript 和发事件

工具执行时，context 里带着 run 的状态：

```go
// 工具开始执行时的 transcript 副本。
history, _ := ctx.Value(ctxkey.AgentHistory).([]openai.ChatCompletionMessage)

// 往 run 的消息流里推一条产品事件：不进模型上下文。
if emit, ok := ctx.Value(ctxkey.ToolEventEmitter).(base.ToolEventEmitter); ok {
	emit(base.Msg{
		Type: "chart_result", // 类型由你自己定义，运行时只负责转发
		Data: map[string]any{"chart_id": "sales", "chart_option": option},
	})
}
```

`ctxkey` 里共三个键：`ChatID`（会话 id，日志用）、`AgentHistory`（transcript 副本）、
`ToolEventEmitter`（发事件的函数）。

### 发起 run、读结果

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

stream, err := agent.Run(ctx, userInput) // err 表示调用方式有问题，例如实例已在跑
if err != nil {
	return err
}
for msg := range stream {
	forward(msg)
}
result := agent.Result()
```

run 期间只有三种消息：

| `msg.Type` | 含义 |
| --- | --- |
| `reasoning` | 模型的思考文本，流式累积 |
| `content` | 回答正文，最后一条一定是 end tool 的答案 |
| `usage` | 单次 LLM 调用的 token 用量，在 `msg.Data` 里 |

没有心跳、没有开始/结束标记——channel 关闭就是 run 结束。结局只在 `Result()` 里：成功看 `Answer`、
取消看 `Stopped`、失败看 `Err`，token 用量累加在 `Usage` / `LLMCalls`。

### 多轮对话

运行时不替你持久化，跨轮状态显式传递：一轮一个实例，历史自己存。

```go
agent := base.NewBaseAgent("analyst", "Data analyst", prompt, model, token, baseURL, finishTool{}).
	WithHistory(loadHistory(chatID)) // 从你自己的存储里读出来

for msg := range stream {
	forward(msg)
}

saveHistory(chatID, agent.History()) // 返回深拷贝，可以放心留存
```

`History()` 与 `WithHistory()` 都是深拷贝，两边不会共享可变状态；`HasState()` 判断是否已有历史。
落库时 `role`、`content`、`tool_calls`、`tool_call_id` 要原样保存，否则下一轮请求会被上游拒绝（见
「排错」）。可序列化的形态是 `base.AgentContextSnapshot`。

上面两个函数 `loadHistory` / `saveHistory` 代表你自己的存储实现：SDK 没有持久化层，它只负责把
transcript 交给你、再接收回来。

### 记忆模块与计划模块

两个可选模块，注册后自动带上工具和提示词段落：

```go
type myMemory struct{}

func (myMemory) Tools() []base.Tool { return []base.Tool{rememberTool{}, recallTool{}} }
func (myMemory) BuildPromptBlock(ctx context.Context) string {
	return "记忆使用说明…"
}

agent.WithMemory(myMemory{}) // 注册它的工具 + 追加提示词段落
```

计划模块多两个钩子：`Reset()` 每轮 run 开始时调用，`ValidateFinalAnswer(ctx)` 在答案发出前校验
（校验失败会自动完成剩余任务并发出一条事件）。

```go
agent.WithPlanModule(myPlan{}) // Tool() + Prompt() + Reset() + ValidateFinalAnswer()
```

### 上下文压缩

`WithMaxContextTokens` 是唯一的开关——给出模型窗口，其余按比例推导（不设置则默认 1,000,000）。

```go
agent.WithMaxContextTokens(128 * 1024)
```

触发依据是最近一次调用返回的 `prompt_tokens`（事后值，用量缺失时永不触发）超过 `窗口 × 0.6`；
压缩后逐字保留的尾部预算是 `阈值 × 0.25`。

裁剪点的选择顺序：优先切在 `user` 消息（轮次起点）；若最新一轮单独就超预算，就钻进该轮内部的
`assistant` 迭代点；最后才退到 `assistant` 之间。`system` 永不进压缩区，`tool` 消息永远不能做
裁剪点——工具结果不能脱离它应答的那次调用单独发出。

被压段替换成一条 `user` 消息，内容是 `<compacted-history>` + 结构化摘要（8 个固定章节）+ 最多
40 条 user 消息逐字清单 + 续接契约。摘要由模型生成（不带工具、`max_tokens=3000`）；逐字清单由纯
代码抽取、完全不经过模型，所以即使摘要模型跑偏，用户原话也不会丢。摘要调用失败时本次压缩作废，
transcript 保持原样，run 继续。

压缩块就是普通历史：跟着 transcript 一起入库，用 `WithHistory` 恢复即可——恢复后它能被结构特征
认出，下一次压缩只压它之后的增长段，并把上一次的摘要折叠进新摘要。

### 超时、取消与输出模式

```go
ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
defer cancel()

cancel() // 或者 agent.Stop()：都会让 run 以 Result().Stopped 结束
```

长回答不要用 `http.Client.Timeout`（它把整个响应体算进超时），优先用 context deadline。

```go
agent.WithOutputMode(base.OutputModeStreaming)    // 默认：生成过程中持续发消息
agent.WithOutputMode(base.OutputModeNonStreaming) // 每次回复完成后按类型各发一条
```

两种模式的消息类型、end-tool 契约和结果处理完全一致，区别只是消息到达的频率。

### 日志

```go
xlog.SetLevel(xlog.ParseLevel(os.Getenv("GOER_AGENT_LOG_LEVEL"))) // debug / info / warn / error / off
```

默认 `info`。所有日志行都带 `chat_id=`（有时），模型输出、工具参数与结果都会被截断；每轮请求的
消息体积与 token 估算（`logMessageSizes`）是 `debug` 级。

---

## API 参考

### 构造函数

```go
func NewBaseAgent(name, description, systemPrompt, model, authToken, baseURL string, endTools ...Tool) *BaseAgent
```

`baseURL` 传空则用库的默认端点；`nil` 工具与空名字工具会被跳过；至少要有一个可用的 end tool。

### 配置项（都返回 `*BaseAgent`，可链式）

| 方法 | 作用 |
| --- | --- |
| `WithModel(model)` | 覆盖模型 |
| `WithSystemPrompt(prompt)` | 覆盖基础 system prompt |
| `WithLang(lang)` | 强制回答语言 |
| `WithReasoningEffort(effort)` | 开启 reasoning 请求字段 |
| `WithOutputMode(mode)` | `OutputModeStreaming`（默认）或 `OutputModeNonStreaming` |
| `WithStreamUsage(enabled)` | 流式请求是否索要 token 用量（默认开） |
| `WithEndTool(tool)` / `WithEndTools(tools...)` | 追加 end tool |
| `WithMemory(module)` | 注入记忆模块 |
| `WithPlanModule(module)` | 注入计划模块 |
| `WithHTTPClient(client)` | 自定义 HTTP client（代理 / transport） |
| `WithHTTPHeaders(headers)` | 每个请求附加 header |
| `WithToolResultMaxBytes(n)` | 单条工具结果进上下文的体积上限（默认 256 KiB，`0` 关闭） |
| `WithMaxContextTokens(n)` | 模型上下文窗口，压缩触发阈值由它推导（默认 1,000,000） |
| `SetMaxIterations(n)` | 单次 run 的轮次上限（默认 66） |

### 运行、历史、工具

| 方法 | 说明 |
| --- | --- |
| `Run(ctx, input) (chan Msg, error)` | 启动一轮 run；error 表示调用方式有问题（如并发调用） |
| `Result() RunResult` | 已结束 run 的结果 |
| `Stop()` | 取消当前 run |
| `WithHistory(history)` / `History()` | 注入 / 读取 transcript（都是深拷贝） |
| `HasState()` | 是否已有历史 |
| `AddTool(tool)` / `GetTool(name)` / `GetTools()` | 工具注册表（`GetTools` 返回副本） |
| `EndToolNames()` | 排序后的 end tool 名称 |
| `Name()` / `Description()` / `SystemPrompt()` / `Lang()` | 基础信息 |

### 关键类型

```go
type Tool interface {
	Name() string
	Description() string
	Execute(ctx context.Context, args map[string]any) (ToolResult, error)
}

type ToolResult struct {
	Success      bool           // end tool 成功才结束 run
	Error        string         // 失败原因，进模型上下文
	Meta         map[string]any // 你自己的元信息，运行时不做解释
	ModelContent string         // 进模型上下文的内容
	ModelData    map[string]any // 进模型上下文的结构化数据
	Events       []Msg          // 只发给产品侧的事件
}

type Msg struct {
	Type    string // reasoning / content / usage；工具事件可以是任意自定义类型
	Content string
	Name    string
	Data    map[string]any
}

type ToolEventEmitter func(Msg)

type RunResult struct {
	Answer   string     // end tool 返回的最终内容
	Stopped  bool       // 被取消（Stop、context 取消或超时）
	Err      error      // 失败原因
	Usage    TokenUsage // 整轮累加
	LLMCalls int        // 上报过用量的调用次数
}

type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Model            string
	CachedTokens     int // 可选，取决于 provider
	ReasoningTokens  int // 可选，取决于 provider
}

type OutputMode string

const (
	OutputModeStreaming    OutputMode = "streaming"
	OutputModeNonStreaming OutputMode = "non_streaming"
)
```

### 运行时默认值

| 常量 | 值 | 含义 |
| --- | --- | --- |
| `defaultMaxIterations` | 66 | 单次 run 的模型轮次上限 |
| `defaultToolResultMaxBytes` | 256 KiB | 单条工具结果进上下文的体积上限 |
| `defaultMaxContextTokens` | 1,000,000 | 未显式设置时的上下文窗口 |
| `contextCompactionThresholdRatio` | 0.6 | 窗口的该比例即触发上下文压缩 |
| `contextCompactionKeepRatio` | 0.25 | 压缩后逐字保留的尾部占触发阈值的比例 |
| `noToolCallEscalateAfter` | 2 | 连续多少次纯文本后强制 `tool_choice=required` |
| `noToolCallFailAfter` | 5 | 连续多少次纯文本后 run 失败 |
| `llmMaxAttempts` | 3 | 单次 LLM 调用尝试次数 |
| `llmRetryBaseDelay` / `llmRetryMaxDelay` | 500ms / 5s | 重试退避区间 |
| `eventChannelBuffer` | 256 | `Run` 返回 channel 的缓冲 |
| `logContentMaxRunes` | 2000 | 日志内容截断长度 |

---

## 运行规则

1. 构造函数必须传至少一个 end tool，否则 `panic`。
2. 只有 end tool 成功返回才能结束 run；纯文本回答会被退回给模型。
3. end tool 返回 `Success=false` 或 error 都不结束 run。
4. 连续 2 次纯文本回答 → 下一次请求强制 `tool_choice=required`；连续 5 次 → run 失败。
5. 同一轮里排在 end tool 之后的工具调用会写成「已跳过」占位，保证 transcript 合法。
6. LLM 传输失败退避重试 3 次（500ms → 1s），失败原因保留在 `Result().Err` 里。
7. 工具 panic 会被兜进 `Result().Err`，进程不受影响，run 槽位一定释放。
8. 单条工具结果超过 `WithToolResultMaxBytes` 时，负载被替换成带 `"truncated": true` 的裁剪版。
9. run 结束时 channel 关闭，结果只在 `Result()` 里。
10. end tool 的最终答案作为最后一条 `content` 消息出现在流里，同时镜像到 `Result().Answer`。
11. 每次 LLM 调用只要 provider 返回了 token 用量，就发一条 `usage` 消息，并累加到 `Result().Usage`。
12. 上下文压缩在构建请求之前检查，依据是上一次调用返回的 `prompt_tokens`；摘要失败则本次压缩作废，
    transcript 原样保留。

发送前 `sanitizeMessagesForLLM` 会清空每条消息的 `Name`，并把空的 `Content` / `ReasoningContent`
替换成一个空格（不少 provider 会拒绝空字符串），所以内存里的 transcript 与实际报文有这点差异。

---

## 排错

| 现象 | 原因与处理 |
| --- | --- |
| 编译报 unknown field `ExtraBody` / `ReasoningContent` | 漏了 `replace` 指令，见「安装」。 |
| `Result().Err` 为 `llm call failed after 3 attempts (model=…)` | token 错、base URL 错，或上游不可用。原始原因包在错误里。 |
| `Result().Err` 为 `agent replied without calling a tool 5 times in a row` | 模型一直只说话不调工具。在 system prompt 里明确「只有 `finish` 能结束任务」，或把 `finish` 做得更好调。 |
| `Result().Err` 为 `agent execution exceeded max iterations (66)` | 轮次不够或某个工具一直失败。调大 `SetMaxIterations`，或修工具。 |
| 继续对话时被上游拒（HTTP 400） | 存下来的历史里有 `tool_calls` 但没有配对的工具结果。每条 tool 消息的 `tool_call_id` 要原样保存。 |
| run 一直不结束 | 没注册 end tool，或名字与模型调用的不一致。检查 `agent.EndToolNames()`。 |
| 模型自己写的最终文本没进答案 | 答案只取 end tool 的 `ModelContent`；只有它为空时才回退到模型最后的文本。 |
| 工具结果像是被截断了 | 超过 `WithToolResultMaxBytes`。调大上限，或让工具少返回一些。 |
| `Run` 返回 `agent is already running` | 一个实例一次只跑一个 run。每个请求新建实例。 |
| 回答语言不对 | 用 `WithLang("zh-CN")`，语言只由这个选项控制。 |
| 模型思考时前端没有任何输出 | 运行时只发 `reasoning` 和 `content`；模型没有推理内容时中途就没有输出。 |
| 内容一次性出现、没有流式效果 | agent 处于 `OutputModeNonStreaming`，改成 `OutputModeStreaming`（默认）或干脆不调用 `WithOutputMode`。 |
| 没有 `usage` 消息，或 token 数量全是 0 | provider 没有返回用量。流式请求通过 `stream_options.include_usage` 主动索要；不接受它的上游可以用 `WithStreamUsage(false)` 关掉。 |

---

## 开发

```bash
make fmt-check   # gofmt -l
make vet
make test
make test-race
make check       # fmt-check + vet + test
```

测试共 51 个用例，核心是一个本地 `httptest` 假 LLM（SSE 流式，并记录每次请求），因此可以断言
`tool_choice`、消息内容、请求头和调用次数。CI 在 `main` 和 PR 上跑同样的检查。

## 已知限制

1. fork 依赖（见「安装」）需要每个使用方各自声明 `replace`。
2. 压缩状态（上一次摘要、逐字 user 消息清单）是进程内的。用 `WithHistory` 恢复的历史依然能正确压缩
   ——头部的压缩块靠结构特征识别——但计数与摘要折叠会从该块重新起步。

## 与后端实现的关系

本 SDK 与 `golang-backend/agents/orchestratorv2/agent/base` 同源。后端那份保留平台依赖
（`llm.Client()` 读环境变量、`locales`、`utils`），本 SDK 把 token / base URL、记忆块、日志、
HTTP 客户端、摘要模型都参数化。两边的 end-tool 与上下文压缩语义一致——`compaction.go` 与
`compress.go` 要求逐行可比，改动其一记得同步另一边。
