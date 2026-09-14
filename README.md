# goer-agent-sdk

**English** · [简体中文](README.zh-CN.md)

Turn any OpenAI-compatible chat model into an agent that can only **report completion through a
tool**.

You register tools, the model calls them in a loop, and the turn ends the moment one of your
**end tools** returns success. A text-only reply never ends anything — so "is the task done?" is
answered by your code, not by the model's prose.

```go
agent := base.NewBaseAgent(
	"report-agent", "Generates reports", systemPrompt,
	"gpt-4o", os.Getenv("LLM_TOKEN"), os.Getenv("LLM_BASE_URL"),
	finishTool{}, // end tool: the only way this turn can finish
)
agent.AddTool(aggregateTool{}) // regular tools the model may call along the way

stream, _ := agent.Run(ctx, "Summarize this sheet")
for msg := range stream {
	forward(msg) // "reasoning" = thinking, "content" = answer text
}
fmt.Println("answer:", agent.Result().Answer)
```

---

## Design

### 1. One task = one tool loop with an exit

```
user input
  ↓
build request = system prompt + transcript
  ↓
call the model ─┬─ tool call ─→ run the tool ─→ result into transcript ─┐
                └─ prose only ─→ push back with a correction ───────────┤
                                                                       ↓
                            back to "call the model" (66 turns by default) ◀┘
                                                                       ↓
                        an end tool succeeds ─→ done, deliver ModelContent
```

Three hard rules follow from this:

1. Only a successful end tool finishes a run; a text-only reply is pushed back asking for a tool call.
2. A failing end tool (`Success=false`, or a non-nil error) does **not** finish the run — the model
   sees the reason and keeps working.
3. The answer handed to the user is the end tool's `ModelContent`, not the model's last paragraph.

### 2. Two kinds of tools

| | Regular tool | End tool |
| --- | --- | --- |
| Registered with | `AddTool(t)` | the constructor, `WithEndTool`, `WithEndTools` |
| For | doing the work: queries, computation, charts, memory | delivering: submitting the final answer |
| On success | the loop continues | **the turn ends**, `ModelContent` is the answer |
| On failure | the loop continues, the model sees the error | the loop continues, nothing ends |

Both implement the same `Tool` interface (`Name` / `Description` / `Execute`); only the identity at
registration time differs.

**Why it is designed this way:**

- **Completion has to be assertable by code.** If the model saying "done" were enough, you could not
  verify it: it may declare success one step early, or write "here is how I would do it" as if
  finished. Moving the endpoint into a tool call turns completion into a definite event — a function
  was called, its arguments parsed, your validation passed.
- **Two tools, two responsibilities.** A regular tool's output is a *fact for the model*; an end
  tool's output is a *deliverable for the user*. With a single kind of tool, a model that counted
  some rows tends to hand that straight over.
- **Validation belongs in the end tool.** Being the only exit makes it the natural checkpoint for
  every delivery constraint (markdown table required, length limit, mandatory fields...). A failed
  check returns `Success=false` and the model retries with the reason in hand.
- **Failures belong in the context.** An end tool's failure reason goes into the transcript, which
  is how the model learns why it was rejected and what to change.

### 3. What reaches the model, and what reaches the product

`ToolResult` splits the two channels explicitly:

| Field | Where it goes |
| --- | --- |
| `ModelContent` / `ModelData` | the model context |
| `Error` | the model context (failure reason) |
| `Events` | the product side only; the model never sees them |
| `Meta` | never in the context; the runtime does not interpret it, it is your own bookkeeping |

The reason to keep them apart: chart options, attachment ids and progress percentages in the context
waste tokens and disturb reasoning. When a tool needs to push a product event, it emits through
`ctxkey.ToolEventEmitter` onto the run's message stream (see *Reading the transcript, emitting events*).

### 4. Who owns the state

| State | Owner |
| --- | --- |
| the transcript of one turn | the runtime, in memory |
| persistence across turns | you: read with `History()`, restore with `WithHistory()` |
| concurrent runs | one instance runs one turn at a time |
| context compaction | the runtime, decided from the context window |

One instance = one turn, so the runtime needs no coordination for concurrent requests; cross-turn
state is passed in explicitly, and what it is stored as (DB, Redis, files) is entirely your choice.
The runtime never touches your storage.

### 5. Context compaction

A long task eventually fills the context window, and the runtime does not push that problem onto the
caller: 60% of the window triggers compaction, 25% of the threshold is kept verbatim, and the
compacted span becomes one `<compacted-history>` message (a structured summary plus a verbatim user
message list). Trigger, boundary selection and failure behavior are in
[Context compaction](#context-compaction).

---

## Installation

```
go get github.com/Mrfogg/goer-agent-sdk
```

### Required: the `replace` directive

The SDK needs two things upstream `go-openai` does not have: a top-level `ExtraBody` on the request
(the `thinking` / `reasoning` fields reasoning models use), and the `reasoning` field when parsing
replies. Both live in a fork, and that fork still declares the upstream module path — so a `replace`
directive is the only way to select it.

Add this to the `go.mod` of **every module that imports the SDK**:

```
require github.com/sashabaranov/go-openai v1.42.0

replace github.com/sashabaranov/go-openai => github.com/neugls/go-openai v1.42.0-reasoning-extra-body
```

- Skipping it breaks the **build**, not the run: the compiler reports unknown field `ExtraBody` /
  `ReasoningContent`.
- Go ignores the `replace` directives of dependencies: having them in this repository's `go.mod`
  does not help consumers — each one copies the two lines above.
- If the SDK sits next to your code instead of on GitHub, add
  `replace github.com/Mrfogg/goer-agent-sdk => ../goer-agent-sdk` too.
- The fork must be reachable from your CI as well (public, or authenticated): `go mod download`
  fetches it on a clean machine.

If your upstream does not need the reasoning fields at all, you can skip `WithReasoningEffort` and
depend on upstream `go-openai` yourself — but the published source expects the fork. Go 1.23+.

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

// A regular tool: the model may call it while working.
type rowsTool struct{}

func (rowsTool) Name() string        { return "count_rows" }
func (rowsTool) Description() string { return "Count the rows of the current sheet" }

func (rowsTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	// ModelContent is what the model sees on its next turn.
	return base.ToolResult{Success: true, ModelContent: "120 rows"}, nil
}

// The end tool: success here is what finishes the turn.
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

What happens during that turn:

1. the model replies with a tool call (`count_rows` or `finish`);
2. a regular tool's result goes back into the transcript and the loop continues;
3. a successful `finish` ends the run: its content is emitted as the last `content` message, the
   channel closes, and `Result().Answer` holds the same text;
4. if the model answers with prose only, the runtime sends it back with a corrective message.

More runnable programs live in [`examples/`](examples): `quickstart`, `tools`, `multiturn`,
`streaming`, `httpapi` and `localmock` — the last one needs no credentials and no network:

```bash
go run ./examples/localmock
```

---

## Usage

### Defining a regular tool

```go
type rowsTool struct{}

func (rowsTool) Name() string        { return "count_rows" }
func (rowsTool) Description() string { return "Count the rows of the current sheet" }

func (rowsTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	return base.ToolResult{
		Success:      true,
		ModelContent: "120 rows",                             // for the model
		ModelData:    map[string]any{"rows": 120, "cols": 3}, // structured, also in the context
	}, nil
}

agent.AddTool(rowsTool{})
```

### Defining an end tool

```go
type finishTool struct{}

func (finishTool) Name() string        { return "finish" }
func (finishTool) Description() string { return "Submit the final answer when the task is complete" }

func (finishTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	answer, _ := args["answer"].(string)
	if !strings.Contains(answer, "|") { // delivery constraints belong here
		return base.ToolResult{Success: false, Error: "answer must contain a markdown table"}, nil
	}
	if len(answer) > 8000 {
		return base.ToolResult{Success: false, Error: "answer is too long, summarise it"}, nil
	}
	return base.ToolResult{Success: true, ModelContent: answer}, nil
}

// The constructor needs at least one end tool; it panics otherwise.
agent := base.NewBaseAgent("analyst", "Data analyst", prompt, model, token, baseURL, finishTool{})
```

When an end tool succeeds but returns an empty `ModelContent`, the runtime falls back to the model's
last text; when both are empty it adds a corrective message and lets the model try again.

### Custom argument schema

By default a tool's argument schema is "any JSON object". Implement `OpenAIFunctionProvider` for a
strict schema:

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

Available builders: `OpenAIObjectSchema`, `OpenAIArraySchema`, `OpenAIStringSchema`,
`OpenAIIntegerSchema`, `OpenAIBooleanSchema`.

### Reading the transcript, emitting events

While a tool executes, the context carries the run's state:

```go
// A copy of the transcript as it was when the tool started.
history, _ := ctx.Value(ctxkey.AgentHistory).([]openai.ChatCompletionMessage)

// Push a product event onto the run's stream: it never reaches the model context.
if emit, ok := ctx.Value(ctxkey.ToolEventEmitter).(base.ToolEventEmitter); ok {
	emit(base.Msg{
		Type: "chart_result", // your own type; the runtime only forwards it
		Data: map[string]any{"chart_id": "sales", "chart_option": option},
	})
}
```

`ctxkey` holds three keys: `ChatID` (session id, used in logs), `AgentHistory` (the transcript copy)
and `ToolEventEmitter` (the emit function).

### Starting a run, reading the result

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

stream, err := agent.Run(ctx, userInput) // error means the call is wrong, e.g. a run is active
if err != nil {
	return err
}
for msg := range stream {
	forward(msg)
}
result := agent.Result()
```

A run emits exactly three message types:

| `msg.Type` | Meaning |
| --- | --- |
| `reasoning` | the model's thinking text, streamed |
| `content` | the answer text; the last one is always the end tool's answer |
| `usage` | token usage of one LLM call, in `msg.Data` |

There is no heartbeat, no start marker and no terminal event — the channel closing is the end of the
run. The outcome lives only in `Result()`: `Answer` on success, `Stopped` on cancellation, `Err` on
failure, with token usage summed into `Usage` / `LLMCalls`.

### Multi-turn conversation

The runtime does not persist anything for you; cross-turn state is passed explicitly: one instance
per turn, and you keep the history.

```go
agent := base.NewBaseAgent("analyst", "Data analyst", prompt, model, token, baseURL, finishTool{}).
	WithHistory(loadHistory(chatID)) // from your own store

for msg := range stream {
	forward(msg)
}

saveHistory(chatID, agent.History()) // returns a deep copy, safe to keep
```

`History()` and `WithHistory()` both copy deeply, so neither side shares mutable state; `HasState()`
tells you whether history is already loaded. Persist `role`, `content`, `tool_calls` and
`tool_call_id` verbatim, otherwise the next request is rejected upstream (see *Troubleshooting*). The
serializable shape is `base.AgentContextSnapshot`.

`loadHistory` / `saveHistory` above stand for your own storage functions: the SDK has no persistence
layer, it only hands you the transcript and takes it back.

### Memory and plan modules

Two optional modules; registering one brings in its tools and its prompt section:

```go
type myMemory struct{}

func (myMemory) Tools() []base.Tool { return []base.Tool{rememberTool{}, recallTool{}} }
func (myMemory) BuildPromptBlock(ctx context.Context) string {
	return "how to use this memory..."
}

agent.WithMemory(myMemory{}) // registers its tools + appends its prompt block
```

A plan module adds two hooks: `Reset()` runs at the start of every run, and `ValidateFinalAnswer(ctx)`
runs just before the answer is emitted (a failing validation auto-completes the pending tasks and
emits an event).

```go
agent.WithPlanModule(myPlan{}) // Tool() + Prompt() + Reset() + ValidateFinalAnswer()
```

### Context compaction

`WithMaxContextTokens` is the only switch — give it the model's window and everything else is derived
by ratio (unset means the default 1,000,000).

```go
agent.WithMaxContextTokens(128 * 1024)
```

The trigger is the `prompt_tokens` the most recent call reported (a post-hoc value; a missing usage
never triggers) above `window × 0.6`; the verbatim tail kept after a compaction is `threshold × 0.25`.

The cut point is chosen in this order: on a `user` message (a turn boundary) whenever one fits the
budget; inside the newest turn when that turn alone blows it; between `assistant` steps as a last
resort. `system` never enters the compacted span, and a `tool` message can never be the cut point — a
tool result cannot be sent without the call it answers.

The compacted span becomes one `user` message: `<compacted-history>`, a structured summary (eight
fixed sections), up to 40 user messages verbatim, and the continuation contract. The summary comes
from the model (no tools, `max_tokens=3000`); the verbatim list is extracted by plain code and never
passes through the model, so the user's own words survive a bad summarizer. When the summarizer call
fails, the compaction is aborted, the transcript stays as it was, and the run continues.

The block is ordinary history: persist it with the transcript and restore it with `WithHistory`. A
restored block is recognized structurally, so the next compaction only compacts the growth after it
and folds the previous summary into the new one.

### Timeouts, cancellation, output modes

```go
ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
defer cancel()

cancel() // or agent.Stop(): either ends the run with Result().Stopped set
```

Avoid `http.Client.Timeout` for long answers — it bounds the whole response body. Prefer the context
deadline.

```go
agent.WithOutputMode(base.OutputModeStreaming)    // default: messages while generating
agent.WithOutputMode(base.OutputModeNonStreaming) // one message per reply
```

Both modes share the message types, the end-tool contract and result handling; they differ only in
how often messages arrive.

### Logging

```go
xlog.SetLevel(xlog.ParseLevel(os.Getenv("GOER_AGENT_LOG_LEVEL"))) // debug / info / warn / error / off
```

`info` by default. Every line carries `chat_id=` (when available), and model output, tool arguments
and results are clipped. The per-request size and token estimate (`logMessageSizes`) is `debug`.

---

## API reference

### Constructor

```go
func NewBaseAgent(name, description, systemPrompt, model, authToken, baseURL string, endTools ...Tool) *BaseAgent
```

An empty `baseURL` keeps the library default endpoint. `nil` tools and tools with an empty name are
skipped; at least one usable end tool is required.

### Options (chainable)

| Method | Effect |
| --- | --- |
| `WithModel(model)` | Override the model |
| `WithSystemPrompt(prompt)` | Replace the base system prompt |
| `WithLang(lang)` | Pin the answer language |
| `WithReasoningEffort(effort)` | Enable reasoning request fields |
| `WithOutputMode(mode)` | `OutputModeStreaming` (default) or `OutputModeNonStreaming` |
| `WithStreamUsage(enabled)` | Ask streaming providers for token usage (default on) |
| `WithEndTool(tool)` / `WithEndTools(tools...)` | Register more end tools |
| `WithMemory(module)` | Inject a memory module |
| `WithPlanModule(module)` | Inject a plan module |
| `WithHTTPClient(client)` | Custom HTTP client (proxy / transport) |
| `WithHTTPHeaders(headers)` | Extra headers on every request |
| `WithToolResultMaxBytes(n)` | Cap one tool result entering the context (256 KiB default, `0` disables) |
| `WithMaxContextTokens(n)` | Model context window, which the compaction trigger derives from (1,000,000 default) |
| `SetMaxIterations(n)` | Turn cap per run (66 default) |

### Running, history, tools

| Method | Description |
| --- | --- |
| `Run(ctx, input) (chan Msg, error)` | Start a turn; the error is about the call (e.g. a run is active) |
| `Result() RunResult` | Outcome of the finished run |
| `Stop()` | Cancel the current run |
| `WithHistory(history)` / `History()` | Inject / read the transcript (both deep-copied) |
| `HasState()` | Whether history is already loaded |
| `AddTool(tool)` / `GetTool(name)` / `GetTools()` | Tool registry (`GetTools` returns a copy) |
| `EndToolNames()` | Sorted names of the end tools |
| `Name()` / `Description()` / `SystemPrompt()` / `Lang()` | Identity and prompt accessors |

### Key types

```go
type Tool interface {
	Name() string
	Description() string
	Execute(ctx context.Context, args map[string]any) (ToolResult, error)
}

type ToolResult struct {
	Success      bool           // an end tool's success finishes the run
	Error        string         // failure reason, enters the model context
	Meta         map[string]any // your own metadata; the runtime does not interpret it
	ModelContent string         // enters the model context
	ModelData    map[string]any // structured, enters the model context
	Events       []Msg          // product-side events only
}

type Msg struct {
	Type    string // reasoning / content / usage; tool events may use any custom type
	Content string
	Name    string
	Data    map[string]any
}

type ToolEventEmitter func(Msg)

type RunResult struct {
	Answer   string     // final content returned by the end tool
	Stopped  bool       // cancelled (Stop, context cancel or deadline)
	Err      error      // failure reason
	Usage    TokenUsage // summed over the run
	LLMCalls int        // calls that reported usage
}

type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Model            string
	CachedTokens     int // optional, provider dependent
	ReasoningTokens  int // optional, provider dependent
}

type OutputMode string

const (
	OutputModeStreaming    OutputMode = "streaming"
	OutputModeNonStreaming OutputMode = "non_streaming"
)
```

### Runtime defaults

| Constant | Value | Meaning |
| --- | --- | --- |
| `defaultMaxIterations` | 66 | Assistant turns per run |
| `defaultToolResultMaxBytes` | 256 KiB | Cap per tool result |
| `defaultMaxContextTokens` | 1,000,000 | Context window when none is set |
| `contextCompactionThresholdRatio` | 0.6 | Share of the window that triggers compaction |
| `contextCompactionKeepRatio` | 0.25 | Share of the threshold kept verbatim |
| `noToolCallEscalateAfter` | 2 | Text-only replies before forcing `tool_choice=required` |
| `noToolCallFailAfter` | 5 | Text-only replies before failing the run |
| `llmMaxAttempts` | 3 | Attempts per LLM call |
| `llmRetryBaseDelay` / `llmRetryMaxDelay` | 500ms / 5s | Backoff bounds |
| `eventChannelBuffer` | 256 | Buffer of the channel returned by `Run` |
| `logContentMaxRunes` | 2000 | Log clipping length |

---

## Behavior rules

1. The constructor requires at least one end tool — it panics otherwise.
2. Only a successful end-tool call finishes a run; text-only replies are sent back to the model.
3. A failing end tool (`Success=false`, or a non-nil error) does not finish the run.
4. Two consecutive text-only replies force `tool_choice=required`; five in a row fail the run.
5. Tool calls queued behind an end tool in the same turn are recorded as "skipped" placeholders, so
   the transcript stays valid for the next request.
6. LLM transport failures retry three times (500ms → 1s), then fail with the cause preserved.
7. A panicking tool is recovered into `Result().Err`; the process survives and the run slot is
   released.
8. A single tool result larger than `WithToolResultMaxBytes` is replaced by a clipped payload marked
   `"truncated": true`.
9. The channel closes when the run ends. The outcome is only in `Result()`.
10. The end tool's answer is delivered on the stream as the last `content` message, and mirrored in
    `Result().Answer`.
11. Every LLM call that reports token usage emits one `usage` message and is summed into
    `Result().Usage`.
12. Context compaction is checked before a request is built, from the previous call's
    `prompt_tokens`; when the summarizer call fails the compaction is aborted and the transcript is
    left as it was.

Before sending, `sanitizeMessagesForLLM` clears every message `Name` and replaces empty `Content` /
`ReasoningContent` with a single space (several providers reject empty strings), so the in-memory
transcript and the wire payload differ slightly.

---

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `unknown field ExtraBody` / `ReasoningContent` at build time | The `replace` directive is missing — see Installation. |
| `Result().Err` = `llm call failed after 3 attempts (model=…)` | Wrong token, wrong base URL, or the upstream is down. The wrapped cause is in the message. |
| `Result().Err` = `agent replied without calling a tool 5 times in a row` | The model keeps answering in prose. State explicitly that only `finish` ends the task, or make `finish` easier to call. |
| `Result().Err` = `agent execution exceeded max iterations (66)` | The task needs more turns than allowed, or a tool keeps failing. Raise `SetMaxIterations` or fix the tool. |
| HTTP 400 when continuing a conversation | Stored history has an assistant `tool_calls` entry without matching tool results. Persist `tool_call_id` verbatim for every tool message. |
| The run never finishes | No end tool is registered, or its name does not match what the model calls. Check `agent.EndToolNames()`. |
| The model's own final text is missing from the answer | Only the end tool's `ModelContent` becomes the answer; the runtime falls back to the last assistant text only when it is empty. |
| A tool result looks truncated | It exceeded `WithToolResultMaxBytes`. Raise the cap or shrink the tool output. |
| `Run` returns `agent is already running` | One agent instance = one run. Build a new instance per request. |
| Answers come back in the wrong language | Set `WithLang("zh-CN")` — the language is only controlled by this option. |
| Nothing shows up while the model is thinking | Only `reasoning` and `content` messages are emitted; with no reasoning content there is no early output. |
| Text appears all at once instead of streaming | The agent is in `OutputModeNonStreaming`. Switch to `OutputModeStreaming` (the default). |
| No `usage` messages, or all token counts are zero | The provider does not return usage. Streaming requests opt in with `stream_options.include_usage`; call `WithStreamUsage(false)` for upstreams that reject it. |

---

## Development

```bash
make fmt-check   # gofmt -l
make vet
make test
make test-race
make check       # fmt-check + vet + test
```

The suite (51 tests) runs against a local `httptest` fake LLM that streams SSE and records every
request, so it can assert `tool_choice`, message contents, headers and call counts. CI runs the
same checks on `main` and on pull requests.

## Known limitations

1. The fork dependency (see Installation) must be repeated by every consumer.
2. Compaction state (the previous summary and the verbatim user message list) lives in-process. A
   transcript restored with `WithHistory` still compacts correctly, because the block at its head
   is recognized structurally, but the counters and the summary fold start over from that block.

## Relationship to the backend copy

This SDK shares its origin with `golang-backend/agents/orchestratorv2/agent/base`. The backend
copy keeps platform dependencies (`llm.Client()` reading environment variables, `locales`,
`utils`); this one parameterizes token / base URL, memory block, logging, HTTP client and the
summarizer model. Both follow the same end-tool and context-compaction semantics — `compaction.go`
and `compress.go` are meant to stay line-by-line comparable, so keep them in sync when changing
either.
