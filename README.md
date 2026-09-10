# goer-agent-sdk

**English** · [简体中文](README.zh-CN.md)

Turn any OpenAI-compatible chat model into a **tool-using agent** that can only report success
through a tool you control.

You register tools, the model calls them in a loop, and the run ends the moment one of your
**end tools** returns success. A text-only reply never ends a run — so "is the task done?" is
answered by your code, not by the model's prose.

```go
agent := base.NewBaseAgent(
	"report-agent", "Generates reports", systemPrompt,
	"gpt-4o", os.Getenv("LLM_TOKEN"), os.Getenv("LLM_BASE_URL"),
	finishTool{}, // end tool: the only way this run can finish
)
agent.AddTool(aggregateTool{}) // regular tools the model may call along the way

stream, err := agent.Run(ctx, "Summarize this sheet")
if err != nil {
	panic(err)
}
for msg := range stream {
	fmt.Print(msg.Content) // "reasoning" = thinking, "content" = answer text
}
fmt.Println("\nanswer:", agent.Result().Answer)
```

---

## Installation

```
go get github.com/Mrfogg/goer-agent-sdk
```

### Required: the `replace` directive

This SDK needs two things that upstream `go-openai` does not have yet: a top-level `ExtraBody`
map on the request (`thinking` / `reasoning` fields for reasoning models), and the
OpenAI-style `reasoning` field when parsing replies. Both live in a fork, and that fork still
declares the upstream module path — so the only way to select it is a `replace` directive.

Add this to the `go.mod` of **every module that imports the SDK**:

```
require github.com/sashabaranov/go-openai v1.42.0

replace github.com/sashabaranov/go-openai => github.com/neugls/go-openai v1.42.0-reasoning-extra-body
```

Why it matters, in practice:

- **Skip it and the build breaks**, and not at runtime: the compiler reports unknown field
  `ExtraBody` / `ReasoningContent` in `llm.go`.
- **Go ignores the `replace` directives of dependencies**: having it in this repository's
  `go.mod` does not help consumers — each consumer copies the two lines above.
- **If the SDK lives next to your code instead of on GitHub**, also add
  `replace github.com/Mrfogg/goer-agent-sdk => ../goer-agent-sdk`.
- **The fork must be reachable** (public, or authenticated) for your CI as well, because
  `go mod download` fetches it on a clean machine.

If your gateway does not need the reasoning fields at all, you can avoid `WithReasoningEffort`
and depend on upstream `go-openai` yourself — but the published SDK source expects the fork.

Requirements: Go 1.23+.

---

## Quick start

A complete program: one regular tool, one end tool, one turn.

```go
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	base "github.com/Mrfogg/goer-agent-sdk"
)

// 1. A regular tool: the model may call it while working.
type rowsTool struct{}

func (rowsTool) Name() string        { return "count_rows" }
func (rowsTool) Description() string { return "Count the rows of the current sheet" }

func (rowsTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	// ModelContent is what the model sees on its next turn.
	return base.ToolResult{Success: true, ModelContent: "120 rows"}, nil
}

// 2. The end tool: success here is what finishes the run.
type finishTool struct{}

func (finishTool) Name() string        { return "finish" }
func (finishTool) Description() string { return "Submit the final answer when the task is complete" }

func (finishTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	answer, _ := args["answer"].(string)
	if strings.TrimSpace(answer) == "" {
		// A failure keeps the run going: the model sees this error and retries.
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
		os.Getenv("LLM_TOKEN"),    // auth token
		os.Getenv("LLM_BASE_URL"), // base URL, e.g. https://api.openai.com/v1
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
			fmt.Print(msg.Content) // the model's thinking, streamed
		case base.MsgTypeContent:
			fmt.Print(msg.Content) // the answer text, streamed
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

What happens at runtime:

1. the model replies with a tool call (`count_rows` or `finish`);
2. regular tool results go back into the transcript and the loop continues;
3. a successful `finish` ends the run: the channel closes and `Result().Answer` holds the content
   your end tool returned;
4. if the model answers with prose only, the runtime sends it back with a corrective message.

---

## Usage recipes

### 1. Several tools, one hand-off

Most agents are "do the work with regular tools, then hand the result to an end tool".

```go
agent := base.NewBaseAgent("analyst", "Data analyst", prompt, model, token, baseURL, finishTool{})
agent.AddTool(queryTool{})     // reads data
agent.AddTool(aggregateTool{}) // computes
agent.AddTool(chartTool{})     // renders
```

Describe the hand-off in your system prompt ("work with the other tools, then call `finish`").
The runtime also appends the rule itself, per request:

> END-TOOL MODE (MANDATORY) — End tools: [finish]. Only a successful call to one of these tools
> can finish this run; a text-only answer never ends the run and will be sent back to you.

### 2. Validate inside the end tool

The end tool is the only gate, so quality checks belong there. Returning a failure is how you
say "not good enough, keep working".

```go
func (finishTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	answer, _ := args["answer"].(string)
	if !strings.Contains(answer, "|") {
		// The model sees this text and fixes the answer on the next turn.
		return base.ToolResult{Success: false, Error: "answer must contain a markdown table"}, nil
	}
	if len(answer) > 8000 {
		return base.ToolResult{Success: false, Error: "answer is too long, summarise it"}, nil
	}
	return base.ToolResult{Success: true, ModelContent: answer}, nil
}
```

### 3. Return structured results to the model

`ModelContent` is free text, `ModelData` is structured; both enter the model context.

```go
return base.ToolResult{
	Success:      true,
	ModelContent: "query finished: 120 rows, 3 columns",
	ModelData: map[string]any{
		"rows":    120,
		"columns": []string{"date", "region", "amount"},
	},
}, nil
```

### 4. Emit product events to your frontend

Tools push events to your frontend without polluting the model context:

```go
import "github.com/Mrfogg/goer-agent-sdk/ctxkey"

func (t chartTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	option := buildChartOption(args) // your code

	if emit, ok := ctx.Value(ctxkey.ToolEventEmitter).(base.ToolEventEmitter); ok {
		emit(base.Msg{
			Type: base.MsgTypeChartResult,
			Data: map[string]any{"chart_id": "sales", "chart_option": option},
		})
	}

	return base.ToolResult{
		Success:      true,
		ModelContent: "chart generated",
	}, nil
}
```

Use `Data` on the emitted `Msg` for anything the frontend needs (chart id, attachment payload,
progress percentage); it never reaches the model.

### 5. Custom JSON schema, and reading the transcript

By default a tool's arguments schema is "any JSON object". Implement `OpenAIFunctionProvider`
for a strict schema:

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

Tools also receive the transcript as it was when the tool started (a copy):

```go
history, _ := ctx.Value(ctxkey.AgentHistory).([]openai.ChatCompletionMessage)
```

### 6. Multi-turn conversation

One agent instance per turn; persistence is yours.

```go
agent := base.NewBaseAgent("analyst", "Data analyst", prompt, model, token, baseURL, finishTool{}).
	WithHistory(loadHistory(chatID)) // []openai.ChatCompletionMessage from your store

for msg := range agent.Run(ctx, userInput) {
	forward(msg)
}

saveHistory(chatID, agent.History()) // returns a copy, safe to keep
```

Store `role`, `content`, `tool_calls` and `tool_call_id` verbatim — see the FAQ about HTTP 400.

### 7. Stream to a frontend with a stop button

```go
ctx, cancel := context.WithCancel(context.Background())
stream, err := agent.Run(ctx, userInput)
if err != nil {
	return err
}

go func() {
	for msg := range stream {
		forward(msg) // "reasoning" or "content", your websocket / SSE writer
	}
	closeClientStream()
}()

// ... user pressed "stop"
cancel() // or: agent.Stop()
```

| Message type | Meaning |
| --- | --- |
| `reasoning` | the model's thinking (reasoning) text, streamed |
| `content` | the answer text, streamed |

Those are the only two types the runtime ever emits. There is no heartbeat, no start marker and
no terminal event: the channel simply closes, and `Result()` then tells you how the run ended.

### 8. Timeouts and cancellation

Give the run a deadline; when it fires the run ends with `Result().Stopped` set (and `Err` is
`context.DeadlineExceeded`).

```go
ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
defer cancel()
```

Avoid `http.Client.Timeout` when you stream long answers — it bounds the whole response body.
Prefer the context deadline, and use `WithHTTPClient` only for transports and proxies.

### 9. Point at another gateway

```go
agent := base.NewBaseAgent("analyst", "Data analyst", prompt, model, apiKey, "https://my-gateway/v1", finishTool{}).
	WithHTTPHeaders(map[string]string{
		"X-OpenRouter-Title": "excelmatic",
		"HTTP-Referer":       "https://excelmatic.com",
	}).
	WithHTTPClient(&http.Client{Transport: myProxyTransport})
```

`baseURL` is passed verbatim to the client; an empty string keeps the library default endpoint.

### 10. Reasoning models

```go
agent.WithReasoningEffort("medium") // sends reasoning_effort + thinking/reasoning body fields
```

Reasoning deltas are streamed as `reasoning` events, kept apart from the answer text that
arrives as `content` events. Pass an empty string to disable the reasoning request fields.

### 11. Memory and plan modules

```go
type myMemory struct{}

func (myMemory) Tools() []base.Tool { return []base.Tool{rememberTool{}, recallTool{}} }

func (myMemory) BuildPromptBlock(ctx context.Context) string {
	return "Known about this user:\n- works in finance" // appended to the system prompt
}

agent.WithMemory(myMemory{})
agent.WithPlanModule(myPlan{}) // Tool() + Prompt() + Reset() + ValidateFinalAnswer()
```

When a plan module is registered: its tool is registered, its prompt section is appended,
`Reset()` runs at the start of every run, and `ValidateFinalAnswer(ctx)` runs just before the
answer is emitted (a failing validation auto-completes the pending tasks and emits an event).

### 12. Keep the context small

```go
agent.WithToolResultMaxBytes(128 * 1024) // oversized tool results become a clipped payload
agent.SetMaxIterations(40)               // cap turns per run (default 66)

// trim history yourself before the next turn:
agent.WithHistory(lastNMessages(loadHistory(chatID), 30))
```

### 13. Persist a run

```go
stream, err := agent.Run(ctx, input)
if err != nil {
	return err
}
for msg := range stream {
	forward(msg) // stream the model output to the user
}

result := agent.Result()
if result.Err != nil {
	// handle the failure (Result().Stopped tells cancellation from error)
}
db.SaveTurn(chatID, result.Answer, agent.History())
```

`result.Answer` is the end tool's final content; `agent.History()` is the full transcript for the
next turn. Product-specific payloads (charts, dashboards) are whatever your tools emit through
`Msg.Data` — the runtime does not aggregate them for you.

### 14. Use it inside an HTTP handler

```go
func handle(w http.ResponseWriter, r *http.Request) {
	agent := base.NewBaseAgent("analyst", "Data analyst", prompt, model, token, baseURL, finishTool{}).
		WithHistory(loadHistory(chatID)) // per-request instance: no shared state

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

One instance handles one run. A concurrent second `Run` returns an error reading
`agent is already running: one BaseAgent instance handles a single run at a time`.

---

## Behavior rules

1. The constructor requires at least one end tool — it panics otherwise.
2. Only a successful end-tool call finishes a run; text-only replies are sent back to the model.
3. A failing end tool (`Success=false`, or a non-nil error) does not finish the run.
4. Two consecutive text-only replies force `tool_choice=required`; five in a row fail the run.
5. Tool calls queued behind an end tool in the same turn are recorded as "skipped" placeholders,
   so the transcript stays valid for the next request.
6. LLM transport failures retry three times (500ms → 1s), then fail with the cause preserved.
7. A panicking tool is recovered into `Result().Err`; the process survives and the run slot is
   released.
8. The channel closes when the run ends. The outcome is only in `Result()`:
   `Answer` on success, `Stopped` for cancellation, `Err` for failure.

### What the model actually receives

`sanitizeMessagesForLLM` clears every message `Name`, and replaces empty `Content` /
`ReasoningContent` with a single space (several providers reject empty strings). The in-memory
transcript and the wire payload therefore differ slightly.

---

## API reference

### Constructor

```go
func NewBaseAgent(name, description, systemPrompt, model, authToken, baseURL string, endTools ...Tool) *BaseAgent
```

An empty `baseURL` keeps the library default endpoint. `nil` tools and tools with an empty name
are skipped; at least one usable end tool is required.

### Options (chainable)

| Method | Effect |
| --- | --- |
| `WithModel(model)` | Override the model |
| `WithSystemPrompt(prompt)` | Replace the base system prompt |
| `WithLang(lang)` | Pin the answer language |
| `WithReasoningEffort(effort)` | Enable reasoning request fields |
| `WithEndTool(tool)` / `WithEndTools(tools...)` | Register more end tools |
| `WithMemory(module)` | Inject a memory module |
| `WithPlanModule(module)` | Inject a plan module |
| `WithHTTPClient(client)` | Custom HTTP client (proxy / transport) |
| `WithHTTPHeaders(headers)` | Extra headers on every request |
| `WithToolResultMaxBytes(n)` | Cap one tool result entering the context (`0` disables) |
| `SetMaxIterations(n)` | Turn cap per run (default 66) |

### Running, history, tools

| Method | Description |
| --- | --- |
| `Run(ctx, input) (chan Msg, error)` | Start a run; the error reports a misuse such as a concurrent run |
| `Result() RunResult` | The outcome of the finished run: `Answer`, `Stopped`, `Err` |
| `Stop()` | Cancel the run (`Result().Stopped` becomes true) |
| `WithHistory(history)` / `History()` | Seed / read the transcript (deep copies) |
| `HasState()` | Whether history exists |
| `AddTool(tool)` / `GetTool(name)` / `GetTools()` | Tool registry (`GetTools` returns a copy) |
| `EndToolNames()` | Sorted end-tool names |
| `Name()` / `Description()` / `SystemPrompt()` / `Lang()` | Metadata |

### Key types

```go
type Tool interface {
	Name() string
	Description() string
	Execute(ctx context.Context, args map[string]any) (ToolResult, error)
}

type ToolResult struct {
	Success      bool            // end tool success finishes the run
	Error        string          // failure reason shown to the model
	Meta         map[string]any  // your own metadata; the runtime does not interpret it
	ModelContent string          // enters the model context
	ModelData    map[string]any  // enters the model context (structured)
	Events       []Msg           // sent to your frontend only
}

type Msg struct {
	Type    string
	Content string
	Name    string
	Data    map[string]any
}

type ToolEventEmitter func(Msg)

// The two messages the runtime emits while a run is streaming.
const (
	MsgTypeReasoning = "reasoning" // the model's thinking text
	MsgTypeContent   = "content"   // the answer text
)

type RunResult struct {
	Answer  string // final content returned by the end tool
	Stopped bool   // the run was cancelled (Stop, ctx cancel or deadline)
	Err     error  // failure reason
}
```

### Runtime defaults

| Constant | Value | Meaning |
| --- | --- | --- |
| `defaultMaxIterations` | 66 | Assistant turns per run |
| `defaultToolResultMaxBytes` | 256 KiB | Cap per tool result |
| `noToolCallEscalateAfter` | 2 | Text-only replies before forcing `tool_choice=required` |
| `noToolCallFailAfter` | 5 | Text-only replies before failing the run |
| `llmMaxAttempts` | 3 | Attempts per LLM call |
| `llmRetryBaseDelay` / `llmRetryMaxDelay` | 500ms / 5s | Backoff bounds |
| `eventChannelBuffer` | 256 | Buffer of the channel returned by `Run` |
| `logContentMaxRunes` | 2000 | Log clipping length |

### Logging

```go
xlog.SetLevel(xlog.ParseLevel(os.Getenv("GOER_AGENT_LOG_LEVEL"))) // debug / info / warn / error / off
```

`info` by default. `logMessageSizes` (per-request size and token estimate) is `debug`. Every line
carries `chat_id=`, and model output, tool arguments and results are clipped.

---

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `unknown field ExtraBody` / `ReasoningContent` at build time | The `replace` directive is missing — add it to your `go.mod` (see Installation). |
| `Result().Err` = `llm call failed after 3 attempts (model=…)` | Wrong token, wrong base URL, or the gateway is down. The wrapped cause is in the message. |
| `Result().Err` = `agent replied without calling a tool 5 times in a row` | The model keeps answering in prose. State explicitly that only `finish` ends the task, or make `finish` easier to call (fewer/simpler arguments). |
| `Result().Err` = `agent execution exceeded max iterations (66)` | The task needs more turns than allowed, or a tool keeps failing. Raise `SetMaxIterations` or fix the tool. |
| HTTP 400 when continuing a conversation | Stored history has an assistant `tool_calls` entry without matching tool results. Persist `tool_call_id` verbatim for every tool message. |
| The run never finishes | No end tool is registered, or its name does not match what the model calls. Check `agent.EndToolNames()`. |
| The model's own final text is missing from the answer | Only the end tool's `ModelContent` becomes the answer. Make `finish` return the text you want to show; the runtime falls back to the last assistant text only when it is empty. |
| A tool result looks truncated | It exceeded `WithToolResultMaxBytes`: the payload is replaced by a clipped one with `"truncated": true`. Raise the cap or shrink the tool output. |
| `Run` returns `agent is already running` | One agent instance = one run. Build a new instance per request (recipe 14). |
| Answers come back in the wrong language | Set `WithLang("zh-CN")` — the language is only controlled by this option. |
| Nothing shows up while the model is thinking | Only `reasoning` and `content` messages are emitted; if the model returns plain text without reasoning there is no early output, and the final answer only appears in `Result().Answer`. |

---

## Development

```bash
make fmt-check   # gofmt -l
make vet
make test
make test-race
make check       # fmt-check + vet + test
```

The suite (20 tests) runs against a local `httptest` fake LLM that streams SSE and records every
request, so it can assert `tool_choice`, message contents, headers and call counts. CI runs the
same checks on `main` and on pull requests.

---

## Known limitations

1. The fork dependency (see Installation) must be repeated by every consumer.
2. The runtime does not trim the transcript — long conversations need your own strategy
   (recipe 12).

## Relationship to the backend copy

This SDK shares its origin with `golang-backend/agents/orchestratorv2/agent/base`. The backend
copy keeps platform dependencies (`llm.Client()` reading environment variables, `locales`,
`utils`); this one parameterizes token / base URL, memory block, logging and HTTP client. Both
follow the same end-tool semantics — keep them in sync when changing either.
