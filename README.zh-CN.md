# goer-agent-sdk

[English](README.md) · **简体中文**

把任意 OpenAI 兼容的对话模型变成一个**会用工具的 agent**，而「任务完成」这件事只由你写的工具说了算。

你注册工具，模型在循环里调用它们，只要某个 **end tool** 成功返回，这一轮就结束。纯文本回答永远不会结束
run——所以「做完了没有」由你的代码判断，而不是由模型的措辞判断。

```go
agent := base.NewBaseAgent(
	"report-agent", "生成报表", systemPrompt,
	"gpt-4o", os.Getenv("LLM_TOKEN"), os.Getenv("LLM_BASE_URL"),
	finishTool{}, // end tool：这一轮唯一的结束方式
)
agent.AddTool(aggregateTool{}) // 过程中可以调的普通工具

stream, err := agent.Run(ctx, "帮我总结这张表")
if err != nil {
	panic(err)
}
for msg := range stream {
	fmt.Print(msg.Content) // "reasoning" 是思考，"content" 是正文
}
fmt.Println("\nanswer:", agent.Result().Answer)
```

---

## 安装

```bash
go get github.com/Mrfogg/goer-agent-sdk
```

### 必读：必须加 `replace`

SDK 用到两处上游 `go-openai` 还没有的能力：请求体顶层的 `ExtraBody`（reasoning 模型的
`thinking` / `reasoning` 字段），以及解析回复时的 OpenAI 风格 `reasoning` 字段。这两处只在 fork 里，
而那个 fork 的 `go.mod` 声明的仍是上游模块路径，所以只能用 `replace` 选中它。

**每个 import 本 SDK 的模块**都要在自己的 `go.mod` 里加这两行：

```
require github.com/sashabaranov/go-openai v1.42.0

replace github.com/sashabaranov/go-openai => github.com/neugls/go-openai v1.42.0-reasoning-extra-body
```

实际影响：

- **不加就编译不过**（不是运行时问题）：编译器会报 `llm.go` 里 `ExtraBody` / `ReasoningContent`
  字段不存在。
- **Go 不继承依赖的 `replace`**：写在 SDK 仓库自己的 `go.mod` 里对使用方没用，必须各自复制上面两行。
- **SDK 不在同一仓库时**，再加一行
  `replace github.com/Mrfogg/goer-agent-sdk => ../goer-agent-sdk`。
- **fork 必须能拉到**（公开或已鉴权），否则你的 CI 在干净机器上 `go mod download` 会失败。

如果你的网关完全不需要 reasoning 字段，可以不调用 `WithReasoningEffort(...)`，直接依赖上游
`go-openai`；但本仓库发布的源码默认按 fork 构建。

环境要求：Go 1.23+。

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

// 1. 普通工具：模型干活过程中可以调它。
type rowsTool struct{}

func (rowsTool) Name() string        { return "count_rows" }
func (rowsTool) Description() string { return "统计当前表格的行数" }

func (rowsTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	// ModelContent 是下一轮模型能看到的内容。
	return base.ToolResult{Success: true, ModelContent: "共 120 行"}, nil
}

// 2. end tool：它成功返回，这一轮才结束。
type finishTool struct{}

func (finishTool) Name() string        { return "finish" }
func (finishTool) Description() string { return "任务完成时提交最终答案" }

func (finishTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	answer, _ := args["answer"].(string)
	if strings.TrimSpace(answer) == "" {
		// 返回失败 → run 继续：模型看到这个错误后会重做。
		return base.ToolResult{Success: false, Error: "answer is required"}, nil
	}
	return base.ToolResult{Success: true, ModelContent: answer}, nil
}

func main() {
	agent := base.NewBaseAgent(
		"report-agent",
		"总结表格的 agent",
		"You are a data analyst. Use the tools, then call finish with the final answer.",
		"gpt-4o",
		os.Getenv("LLM_TOKEN"),    // auth token
		os.Getenv("LLM_BASE_URL"), // base URL，例如 https://api.openai.com/v1
		finishTool{},
	)
	agent.AddTool(rowsTool{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := agent.Run(ctx, "这张表有多少行？")
	if err != nil {
		panic(err)
	}

	for msg := range stream {
		switch msg.Type {
		case base.MsgTypeReasoning:
			fmt.Print(msg.Content) // 模型的思考内容，流式
		case base.MsgTypeContent:
			fmt.Print(msg.Content) // 回答正文，流式
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

运行时会依次发生：

1. 模型返回工具调用（`count_rows` 或 `finish`）；
2. 普通工具的结果写回 transcript，循环继续；
3. `finish` 成功 → 它的内容作为最后一条 `content` 消息发出，随后 channel 关闭
   （`Result().Answer` 是同一份内容）；
4. 如果模型只回了一段话、没调工具，运行时会把它退回并附上纠正提示。

更多可运行示例放在 [`examples/`](examples)：`quickstart`、`tools`、`multiturn`、
`streaming`、`httpapi`，以及完全不需要密钥和网络的 `localmock`：

```bash
go run ./examples/localmock
```

---

## 使用案例

### 1. 多个工具 + 一次交付

最常见的形态：普通工具干活，end tool 交付。

```go
agent := base.NewBaseAgent("analyst", "数据分析", prompt, model, token, baseURL, finishTool{})
agent.AddTool(queryTool{})     // 取数
agent.AddTool(aggregateTool{}) // 计算
agent.AddTool(chartTool{})     // 画图
```

在 system prompt 里把交付方式说清楚（「先用其他工具干活，最后调用 `finish`」）。运行时每次请求还会
自动追加 end-tool 规则：

> END-TOOL MODE (MANDATORY) — End tools: [finish]. Only a successful call to one of these tools
> can finish this run; a text-only answer never ends the run and will be sent back to you.

### 2. 在 end tool 里做校验

end tool 是唯一的关卡，所以质量校验放在这里。返回失败就是「不合格，继续改」。

```go
func (finishTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	answer, _ := args["answer"].(string)
	if !strings.Contains(answer, "|") {
		// 模型下一轮会看到这条错误，并据此修正。
		return base.ToolResult{Success: false, Error: "answer 必须包含 markdown 表格"}, nil
	}
	if len(answer) > 8000 {
		return base.ToolResult{Success: false, Error: "answer 太长，请精简"}, nil
	}
	return base.ToolResult{Success: true, ModelContent: answer}, nil
}
```

### 3. 给模型返回结构化结果

`ModelContent` 是文本，`ModelData` 是结构化数据，两者都会进模型上下文。

```go
return base.ToolResult{
	Success:      true,
	ModelContent: "查询完成：120 行、3 列",
	ModelData: map[string]any{
		"rows":    120,
		"columns": []string{"date", "region", "amount"},
	},
}, nil
```

### 4. 工具里给前端发事件

工具可以往产品侧推事件，而不会污染模型上下文：

```go
import "github.com/Mrfogg/goer-agent-sdk/ctxkey"

func (t chartTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	option := buildChartOption(args) // 你自己的逻辑

	if emit, ok := ctx.Value(ctxkey.ToolEventEmitter).(base.ToolEventEmitter); ok {
		emit(base.Msg{
			Type: base.MsgTypeChartResult,
			Data: map[string]any{"chart_id": "sales", "chart_option": option},
		})
	}

	return base.ToolResult{
		Success:      true,
		ModelContent: "图表已生成",
	}, nil
}
```

前端需要的额外信息（chart id、附件负载、进度百分比等）放在 `Msg.Data` 里，它不会进模型上下文。

### 5. 自定义 JSON Schema，以及读取 transcript

默认参数 schema 是「任意 JSON 对象」。需要严格 schema 就实现 `OpenAIFunctionProvider`：

```go
func (finishTool) OpenAIFunctionDefinition() *openai.FunctionDefinition {
	return &openai.FunctionDefinition{
		Name:        "finish",
		Description: "提交最终答案",
		Parameters: base.OpenAIObjectSchema(map[string]jsonschema.Definition{
			"answer": base.OpenAIStringSchema("markdown 格式的最终答案"),
		}, "answer"),
	}
}
```

工具还能读到「自己开始执行时」的 transcript 副本：

```go
history, _ := ctx.Value(ctxkey.AgentHistory).([]openai.ChatCompletionMessage)
```

### 6. 多轮对话

一轮一个 agent 实例；持久化由你负责。

```go
agent := base.NewBaseAgent("analyst", "数据分析", prompt, model, token, baseURL, finishTool{}).
	WithHistory(loadHistory(chatID)) // 从你的存储里读 []openai.ChatCompletionMessage

for msg := range agent.Run(ctx, userInput) {
	forward(msg)
}

saveHistory(chatID, agent.History()) // 返回副本，可以放心留存
```

入库时要原样保留 `role`、`content`、`tool_calls`、`tool_call_id`——原因见「排错」里的 HTTP 400。

### 7. 流式转发前端 + 停止按钮

```go
ctx, cancel := context.WithCancel(context.Background())
stream, err := agent.Run(ctx, userInput)
if err != nil {
	return err
}

go func() {
	for msg := range stream {
		forward(msg) // "reasoning" 或 "content"，你的 websocket / SSE 写出
	}
	closeClientStream()
}()

// ... 用户点了「停止」
cancel() // 或者 agent.Stop()
```

| 消息类型 | 含义 |
| --- | --- |
| `reasoning` | 模型的思考（推理）内容，流式吐出 |
| `content` | 回答正文，流式吐出 |

运行时只会发这两种消息：没有心跳、没有开始标记、也没有终态事件——channel 关闭即 run 结束，
之后用 `Result()` 拿结果。
其中最后一条 `content` 一定是 end tool 给出的最终答案。

### 8. 超时与取消

给 run 一个 deadline；超时后 `Result().Stopped` 为 true，`Result().Err` 是
`context.DeadlineExceeded`。

```go
ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
defer cancel()
```

长回答场景**不要**用 `http.Client.Timeout`——它会把整个响应体一起算进超时。优先用 context
deadline，`WithHTTPClient` 只用来配代理/transport。

### 9. 接第三方网关

```go
agent := base.NewBaseAgent("analyst", "数据分析", prompt, model, apiKey, "https://my-gateway/v1", finishTool{}).
	WithHTTPHeaders(map[string]string{
		"X-OpenRouter-Title": "excelmatic",
		"HTTP-Referer":       "https://excelmatic.com",
	}).
	WithHTTPClient(&http.Client{Transport: myProxyTransport})
```

`baseURL` 会原样交给客户端；传空字符串则用库的默认端点。

### 10. reasoning 模型

```go
agent.WithReasoningEffort("medium") // 发送 reasoning_effort 与 thinking/reasoning 请求体字段
```

推理内容会以 `reasoning` 事件单独吐出，与回答正文的 `content` 事件分开。传空串即关闭 reasoning 请求参数。

### 11. 记忆模块与计划模块

```go
type myMemory struct{}

func (myMemory) Tools() []base.Tool { return []base.Tool{rememberTool{}, recallTool{}} }

func (myMemory) BuildPromptBlock(ctx context.Context) string {
	return "已知用户信息：\n- 从事金融行业" // 会被追加到 system prompt
}

agent.WithMemory(myMemory{})
agent.WithPlanModule(myPlan{}) // Tool() + Prompt() + Reset() + ValidateFinalAnswer()
```

注册计划模块后：它的工具会被注册、提示词段落会被追加、每次 run 开始调用 `Reset()`、答案发出前调用
`ValidateFinalAnswer(ctx)`（校验失败会自动完成剩余任务并发出一条事件）。

### 12. 控制上下文体积

```go
agent.WithToolResultMaxBytes(128 * 1024) // 过大的工具结果会被替换成裁剪版
agent.SetMaxIterations(40)               // 单轮 run 的轮次上限（默认 66）

// 下一轮之前自己裁剪历史：
agent.WithHistory(lastNMessages(loadHistory(chatID), 30))
```

### 13. 把一次 run 入库

```go
stream, err := agent.Run(ctx, input)
if err != nil {
	return err
}
for msg := range stream {
	forward(msg) // 把模型输出实时转发给用户
}

result := agent.Result()
if result.Err != nil {
	// 处理失败（Result().Stopped 用来区分「被取消」还是「出错」）
}
db.SaveTurn(chatID, result.Answer, agent.History())
```

`result.Answer` 是 end tool 给出的最终内容；`agent.History()` 是完整 transcript，供下一轮使用。
产品侧的负载（图表、dashboard）由工具自己通过 `Msg.Data` 抛出，运行时不替你聚合。

### 14. 在 HTTP handler 里使用

```go
func handle(w http.ResponseWriter, r *http.Request) {
	agent := base.NewBaseAgent("analyst", "数据分析", prompt, model, token, baseURL, finishTool{}).
		WithHistory(loadHistory(chatID)) // 每请求一个实例：没有共享状态

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	stream, err := agent.Run(ctx, r.FormValue("input"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	for msg := range stream {
		writeEvent(w, msg)
	}
	saveHistory(chatID, agent.Result().Answer, agent.History())
}
```

一个实例一次只跑一个 run。并发调用第二个 `Run` 会拿到一条
`agent is already running: one BaseAgent instance handles a single run at a time` 的 error。

### 15. 流式输出与非流式输出

`OutputModeStreaming`（默认）在模型生成过程中就把消息发出来，前端可以边生成边渲染；
`OutputModeNonStreaming` 等每次回复完成，再按类型各发一条完整消息——适合批量任务、便宜模型，
或者增量对调用方没意义的场景。

```go
agent.WithOutputMode(base.OutputModeStreaming)    // 默认：一次回复会来很多条消息
agent.WithOutputMode(base.OutputModeNonStreaming) // 一次回复只来一条消息
```

其余一切不变：两种消息类型、end-tool 契约、结果处理方式在两种模式下完全一致。离线示例可以
直接对比：

```bash
go run ./examples/localmock                    # 流式
MODE=non_streaming go run ./examples/localmock # 非流式
```

---

## 运行规则

1. 构造函数必须传至少一个 end tool，否则 `panic`。
2. 只有 end tool 成功返回才能结束 run；纯文本回答会被退回给模型。
3. end tool 返回 `Success=false` 或 error 都不结束 run。
4. 连续 2 次纯文本回答 → 下一次请求强制 `tool_choice=required`；连续 5 次 → run 失败。
5. 同一轮里排在 end tool 之后的工具调用会写成「已跳过」占位，保证 transcript 合法。
6. LLM 传输失败退避重试 3 次（500ms → 1s），失败原因保留在 `Result().Err` 里。
7. 工具 panic 会被兜进 `Result().Err`，进程不受影响，run 槽位一定释放。
8. run 结束时 channel 关闭，结果只在 `Result()` 里：成功看 `Answer`，被取消看 `Stopped`，失败看 `Err`。
9. end tool 的最终答案会作为最后一条 `content` 消息出现在流里，同时镜像到 `Result().Answer`。
10. 输出模式：`OutputModeStreaming`（默认）与 `OutputModeNonStreaming` 发出的是同样两种消息，区别只是消息到达的频率。

### 模型实际收到的消息

发送前 `sanitizeMessagesForLLM` 会清空每条消息的 `Name`，并把空的 `Content` / `ReasoningContent`
替换成一个空格（不少 provider 会拒绝空字符串）。所以内存里的 transcript 与实际发出的报文有这点差异。

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
| `WithReasoningEffort(effort)` | 开启 reasoning 请求参数 |
| `WithOutputMode(mode)` | `OutputModeStreaming`（默认）或 `OutputModeNonStreaming` |
| `WithEndTool(tool)` / `WithEndTools(tools...)` | 追加 end tool |
| `WithMemory(module)` | 注入记忆模块 |
| `WithPlanModule(module)` | 注入计划模块 |
| `WithHTTPClient(client)` | 自定义 HTTP client（代理 / transport） |
| `WithHTTPHeaders(headers)` | 每个请求附加 header |
| `WithToolResultMaxBytes(n)` | 单条工具结果进上下文的体积上限（`0` 关闭） |
| `SetMaxIterations(n)` | 单次 run 的轮次上限（默认 66） |

### 运行、历史、工具

| 方法 | 说明 |
| --- | --- |
| `Run(ctx, input) (chan Msg, error)` | 启动一轮 run；error 表示调用方式有问题（如并发调用） |
| `Result() RunResult` | 已结束 run 的结果：`Answer` / `Stopped` / `Err` |
| `OutputMode()` | 当前的输出模式（默认流式） |
| `Stop()` | 取消当前 run（`Result().Stopped` 会变成 true） |
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
	Success      bool            // end tool 成功才结束 run
	Error        string          // 失败原因（会进模型上下文）
	Meta         map[string]any  // 你自己的元信息，运行时不做解释
	ModelContent string          // 进模型上下文的内容
	ModelData    map[string]any  // 进模型上下文的结构化数据
	Events       []Msg           // 只发给前端的事件
}

type Msg struct {
	Type    string
	Content string
	Name    string
	Data    map[string]any
}

type ToolEventEmitter func(Msg)

// 运行时只会发这两种消息
const (
	MsgTypeReasoning = "reasoning" // 模型的思考内容
	MsgTypeContent   = "content"   // 回答正文
)

type RunResult struct {
	Answer  string // end tool 返回的最终内容
	Stopped bool   // 是否被取消（Stop / ctx 取消 / 超时）
	Err     error  // 失败原因
}

type OutputMode string

const (
	OutputModeStreaming    OutputMode = "streaming"     // 生成过程中就发消息
	OutputModeNonStreaming OutputMode = "non_streaming" // 每次回复只发一条
)
```

### 运行时默认值

| 常量 | 值 | 含义 |
| --- | --- | --- |
| `defaultMaxIterations` | 66 | 单次 run 的模型轮次上限 |
| `defaultToolResultMaxBytes` | 256 KiB | 单条工具结果进上下文的体积上限 |
| `noToolCallEscalateAfter` | 2 | 连续多少次纯文本后强制 `tool_choice=required` |
| `noToolCallFailAfter` | 5 | 连续多少次纯文本后 run 失败 |
| `llmMaxAttempts` | 3 | 单次 LLM 调用尝试次数 |
| `llmRetryBaseDelay` / `llmRetryMaxDelay` | 500ms / 5s | 重试退避区间 |
| `eventChannelBuffer` | 256 | `Run` 返回 channel 的缓冲 |
| `logContentMaxRunes` | 2000 | 日志内容截断长度 |

### 日志

```go
xlog.SetLevel(xlog.ParseLevel(os.Getenv("GOER_AGENT_LOG_LEVEL"))) // debug / info / warn / error / off
```

默认 `info`；`logMessageSizes`（每轮消息体积与 token 估算）是 `debug`。所有日志行都带 `chat_id=`，
模型输出、工具参数与结果都会被截断。

---

## 排错

| 现象 | 原因与处理 |
| --- | --- |
| 编译报 `unknown field ExtraBody` / `ReasoningContent` | 漏了 `replace` 指令——按「安装」一节补到自己的 `go.mod`。 |
| `Result().Err` 为 `llm call failed after 3 attempts (model=…)` | token 错、base URL 错，或网关不可用。包装的原始原因就在错误里。 |
| `Result().Err` 为 `agent replied without calling a tool 5 times in a row` | 模型一直只说话不调工具。在 system prompt 里明确「只有 `finish` 能结束任务」，或把 `finish` 做得更好调（参数更少更简单）。 |
| `Result().Err` 为 `agent execution exceeded max iterations (66)` | 轮次不够或某个工具一直失败。调大 `SetMaxIterations`，或修工具。 |
| 续话时 HTTP 400 | 历史里 assistant 的 `tool_calls` 没有配对的 tool 结果。落库时每条 tool 消息的 `tool_call_id` 必须原样保留。 |
| run 一直不结束 | 没注册 end tool，或工具名与模型调用的名字不一致。检查 `agent.EndToolNames()`。 |
| 模型自己写的最终文本没出现在答案里 | 答案只取 end tool 的 `ModelContent`。让 `finish` 返回你想展示的文本；只有它为空时才会回退到模型最后的文本。 |
| 工具结果像是被截断了 | 超过了 `WithToolResultMaxBytes`，负载被替换成带 `"truncated": true` 的裁剪版。调大上限或让工具少返回一些。 |
| `Run` 返回 `agent is already running` | 一个实例一次只跑一个 run。每个请求新建实例（案例 14）。 |
| 回答语言不对 | 用 `WithLang("zh-CN")`，语言只由这个选项控制。 |
| 模型思考时前端没有任何输出 | 运行时只发 `reasoning` 和 `content`；模型没有推理内容时中途就没有输出，最终答案只在 `Result().Answer` 里。 |
| 内容是一次性出现的、没有流式效果 | agent 处于 `OutputModeNonStreaming`。改成 `OutputModeStreaming`（默认）即可，或干脆不调用 `WithOutputMode`。 |

---

## 开发

```bash
make fmt-check   # gofmt -l
make vet
make test
make test-race
make check       # fmt-check + vet + test
```

测试共 20 个用例，核心是一个本地 `httptest` 假 LLM（SSE 流式，并记录每次请求），因此可以断言
`tool_choice`、消息内容、请求头和调用次数。CI 在 `main` 和 PR 上跑同样的检查。

---

## 已知限制

1. fork 依赖（见「安装」）需要每个使用方各自声明 `replace`。
2. 运行时不会自动裁剪 transcript，长对话需要你自己处理（案例 12）。

## 与后端实现的关系

本 SDK 与 `golang-backend/agents/orchestratorv2/agent/base` 同源。后端那份保留平台依赖
（`llm.Client()` 读环境变量、`locales`、`utils`），本 SDK 把 token / base URL、记忆块、日志、
HTTP 客户端都参数化。两边的 end-tool 语义一致——改动其一记得同步另一边。
