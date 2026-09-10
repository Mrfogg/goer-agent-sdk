# goer-agent-sdk

**English** · [简体中文](README.zh-CN.md)

A small, model-agnostic agent runtime. `BaseAgent` drives any OpenAI-compatible chat
model through a tool-calling loop until one of its **end tools** succeeds.

The design takes one position and holds it: **only an end tool can finish a run.** A
text-only reply never ends a run — the model has to hand the task over by calling a tool.
That keeps completion logic (validation, producing the final content, deciding to retry)
inside the tool, while the runtime only owns the loop, the events and the transcript.

---

## 1. Core concepts

| Concept | Description |
| --- | --- |
| `BaseAgent` | One agent instance = one conversation turn. Holds a transcript and runs one `Run`. |
| `Tool` | `Name` / `Description` / `Execute`; registered on the agent and exposed to the model as an OpenAI function. |
| **end tool** | The termination tool required by the constructor. Success ends the run; failure keeps it going. |
| `Msg` | Events the runtime emits to the caller (progress, heartbeat, terminal state, …). |
| `ToolResult` | What a tool returns, split into model context (`ModelContent`/`ModelData`) and product events (`Events`). |
| `MemoryModule` | Optional module: registers tools and appends a memory block to the system prompt. |
| `PlanModule` | Optional module: registers a tool, appends a prompt section, resets per run, validates before finishing. |

One run looks like this:

```
Run(ctx, input)
  └─ loop (66 turns by default)
       ├─ assemble messages (system prompt + language rule + memory block + end-tool rule + transcript)
       ├─ stream the LLM call (3 attempts with backoff on transport failure)
       ├─ assistant reply has no tool_calls → corrective message, continue
       │                                    (after 2 in a row, force tool_choice=required)
       └─ assistant reply has tool_calls → execute them in order
            ├─ end tool called and succeeded → finish, emit the final content (run_done)
            ├─ end tool called but failed  → keep looping
            └─ regular tool                → write result back to the transcript, keep looping
```

---

## 2. Layout

Module `github.com/excelmatic/goer-agent-sdk`; the root package is `base` (sub-packages
`ctxkey` and `xlog`).

| File | Responsibility |
| --- | --- |
| `agent.go` | `Agent` interface, `BaseAgent` struct, constructor, all `With*` options, tool registration |
| `run.go` | `Run` / `Stop`, the main loop, run lifecycle (concurrency guard, terminal events), plan validation |
| `toolcall.go` | Executing one turn's tool calls, end-tool decision, skipped-call placeholders, tool context |
| `llm.go` | LLM client construction, streaming call and delta merging, retry/backoff, tool schema, message sanitizing |
| `prompt.go` | System prompt assembly and every prompt template, stop message |
| `history.go` | Transcript read/write (deep copies), tool-result serialization and size cap |
| `events.go` | Event delivery (cancellable / bounded for terminal events), heartbeat |
| `log.go` | Logging with `chat_id`, JSON compaction and text clipping for logs |
| `constants.go` | All runtime defaults (iteration cap, buffer, retries, thresholds, …) |
| `tool.go` | `Tool`/`ToolResult`/`Msg`/`ToolEventEmitter`, `MsgType*` constants, OpenAI schema helpers |
| `summary.go` | `SummarizeMessages`: turns one run's events into answer / visible content / dashboard HTML |
| `attachment.go` | Product-facing message conventions (chart attachments, task_completed records) |
| `skill.go` | **Not wired up yet**: `Skill`/`BaseSkill`, see "Known limitations" |
| `ctxkey/` | Context keys tools read: `ChatID`, `AgentHistory`, `ToolEventEmitter` |
| `xlog/` | Tiny leveled logger, `info` by default, `SetLevel` to change |
| `*_test.go` | Unit tests plus end-to-end run tests against a local `httptest` fake LLM |

---

## 3. Dependency: the `replace` directive is mandatory

The SDK needs `ChatCompletionRequest.ExtraBody` and the OpenAI-style `reasoning` field,
which only exist in the `github.com/neugls/go-openai` fork. That fork still declares the
upstream module path, so the only way to select it is a `replace` directive:

```
replace github.com/sashabaranov/go-openai => github.com/neugls/go-openai v1.42.0-reasoning-extra-body
```

**`replace` directives are not inherited.** Every module that imports this SDK must repeat
that line in its own `go.mod`; otherwise Go resolves upstream `sashabaranov/go-openai` and
the build fails on the missing `ExtraBody` / `reasoning` fields. If the SDK directory is not
inside the same repository, a second line is needed:
`replace github.com/excelmatic/goer-agent-sdk => ../goer-agent-sdk`.

Long term: publish the fork under its own module path (e.g. `github.com/excelmatic/go-openai`),
or upstream the two capabilities.

---

## 4. Getting started

> In the examples, `base` is this SDK and `ctxkey` is its sub-package:
>
> ```go
> import (
> 	base   "github.com/excelmatic/goer-agent-sdk"
> 	"github.com/excelmatic/goer-agent-sdk/ctxkey"
> 	"github.com/sashabaranov/go-openai"
> )
> ```

### 4.1 Write an end tool

```go
type finishTool struct{}

func (finishTool) Name() string        { return "finish" }
func (finishTool) Description() string { return "Call when the task is complete and submit the final answer" }

// Implement OpenAIFunctionProvider for a custom JSON schema:
//   func (finishTool) OpenAIFunctionDefinition() *openai.FunctionDefinition { ... }
func (finishTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	answer, _ := args["answer"].(string)
	if strings.TrimSpace(answer) == "" {
		// Validation failed → return a failure; the run feeds it back to the model and continues
		return base.ToolResult{Success: false, Error: "answer is required"}, nil
	}
	return base.ToolResult{Success: true, ModelContent: answer}, nil
}
```

> Validation belongs here. An end tool decides whether the task is done: success finishes the
> run, failure keeps it going. There is no separate "final answer validator" hook.

### 4.2 Run it

```go
agent := base.NewBaseAgent(
	"report-agent",                 // name
	"Agent that generates reports", // description
	"You are a data analyst...",    // system prompt
	"gpt-4o",                       // model
	os.Getenv("LLM_TOKEN"),         // auth token
	os.Getenv("LLM_BASE_URL"),      // base URL; empty falls back to the library default
	finishTool{},                   // required: at least one end tool
)

agent.WithEndTools(abortTool{})              // optional: register more end tools
agent.WithModel("gpt-4.1")                   // optional: override the model
agent.WithLang("zh-CN")                      // optional: pin the answer language
agent.WithReasoningEffort("medium")          // optional: enable reasoning request fields
agent.WithToolResultMaxBytes(128 * 1024)     // optional: cap one tool result entering the context
agent.WithHTTPClient(httpClient)             // optional: custom transport / proxy
agent.WithHTTPHeaders(map[string]string{     // optional: extra headers for a gateway
	"X-OpenRouter-Title": "excelmatic",
})
agent.WithMemory(memoryModule)               // optional: memory module
agent.WithPlanModule(planModule)             // optional: plan module
agent.SetMaxIterations(40)                   // optional: turn cap for a single run

ctx, cancel := context.WithCancel(context.Background())
defer cancel()

for msg := range agent.Run(ctx, "summarize this sheet for me") {
	switch msg.Type {
	case base.MsgTypeProgressUpdate:
		// streaming delta, forward to your frontend if you like
	case base.MsgTypeMarkdown:
		// final answer
	case base.MsgTypeRunDone:
		fmt.Println("answer:", msg.Content)
	case base.MsgTypeRunError:
		fmt.Println("error:", msg.Content)
	case base.MsgTypeRunStopped:
		fmt.Println("stopped:", msg.Content)
	}
}

history := agent.History() // persist this yourself
```

### 4.3 Emit events and read context from a tool

```go
func (t chartTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	// 1) Emit a product event (this does NOT enter the model context)
	if emit, ok := ctx.Value(ctxkey.ToolEventEmitter).(base.ToolEventEmitter); ok {
		emit(base.Msg{Type: base.MsgTypeChartResult, Data: map[string]any{
			"chart_id":     "sales",
			"chart_option": option,
		}})
	}

	// 2) Read the current transcript (a copy; you cannot mutate runtime state)
	history, _ := ctx.Value(ctxkey.AgentHistory).([]openai.ChatCompletionMessage)
	_ = history

	// 3) Tell the runtime which language the user wrote in; later turns answer in it
	return base.ToolResult{
		Success:      true,
		ModelContent: "chart generated",             // enters the model context
		ModelData:    map[string]any{"rows": 120},   // enters the model context (structured)
		Meta:         map[string]any{base.ToolMetaUserQueryLanguageKey: "zh-CN"},
	}, nil
}
```

---

## 5. Run contract

1. **The constructor requires at least one end tool**; it panics otherwise.
2. **Only a successful end-tool call finishes a run.** A text-only reply gets a corrective
   message and the loop continues.
3. An end tool that returns `Success=false` or an error does not finish the run; its result is
   fed back to the model.
4. Two consecutive text-only replies force `tool_choice=required` on the next request; five in
   a row fail the run with `run_error`.
5. Tool calls queued after an end tool in the same turn are not executed, but they still get a
   placeholder "skipped" result so every `tool_calls` entry has a matching tool message
   (otherwise the next request would be rejected with HTTP 400 by OpenAI-compatible APIs).
6. LLM transport failures retry three times with 500ms → 1s backoff; the cause is preserved in
   the `run_error` via `%w`.
7. A panicking tool is recovered into a `run_error`; the process survives and the run slot is
   always released.
8. The channel returned by `Run` is closed after the terminal event, which is always one of
   `run_done`, `run_error` or `run_stopped`.
9. Event delivery is cancelled with the run context; terminal events have a 5s fallback
   delivery, so a consumer that stops reading cannot leak the run goroutine.

### Messages sent to the model are sanitized

`sanitizeMessagesForLLM` applies three provider-compatibility tweaks before sending: it clears
every `Name`, replaces an empty `Content` with a single space, and replaces an empty
`ReasoningContent` with a single space. The in-memory transcript and the wire payload therefore
differ slightly.

---

## 6. Events and terminal states

| Event | When | Key fields |
| --- | --- | --- |
| `start` | the run begins | — |
| `progress_update` | streaming answer / reasoning delta | `Content` |
| `heartbeat` | every 1s (dropped when the consumer lags) | `Data["timestamp"]` |
| `markdown` | the final answer is emitted | `Content` |
| `run_done` | terminal: success | `Content` and `Data["answer"]` |
| `run_error` | terminal: failure | `Content` holds the reason |
| `run_stopped` | terminal: `Stop()` or ctx cancellation | `Content` is the stop message, `Data["reason"]` the cause |

Tools may also emit product events: `chart_result`, `task_completed`, `dashboard_html`,
`report_start`, `report_end`. `SummarizeMessages` folds a run's events into a persistable
record:

```go
summary := base.SummarizeMessages(msgs)
summary.Answer         // final answer (visible content wins when present)
summary.VisibleContent // markdown / task_completed / chart attachments / stop message
summary.HtmlContent    // dashboard_html
```

---

## 7. API reference

### Constructor

```go
func NewBaseAgent(name, description, systemPrompt, model, authToken, baseURL string, endTools ...Tool) *BaseAgent
```

An empty `baseURL` keeps the library default endpoint. At least one usable end tool is required
(`nil` tools and tools with an empty name are skipped).

### Options (all return `*BaseAgent`, chainable)

| Method | Effect |
| --- | --- |
| `WithModel(model)` | Override the model |
| `WithSystemPrompt(prompt)` | Replace the base system prompt |
| `WithLang(lang)` | Pin the answer language (added to the system prompt) |
| `WithReasoningEffort(effort)` | Enable reasoning request fields; empty string disables |
| `WithEndTool(tool)` / `WithEndTools(tools...)` | Register more end tools |
| `WithMemory(module)` | Inject a memory module (tools + memory block) |
| `WithPlanModule(module)` | Inject a plan module (tool + prompt + per-run reset + final validation) |
| `WithHTTPClient(client)` | Custom HTTP client (proxy, transport, pool) |
| `WithHTTPHeaders(headers)` | Extra headers on every LLM request |
| `WithToolResultMaxBytes(n)` | Cap one tool result entering the context; `0` disables |
| `SetMaxIterations(n)` | Turn cap for a single run (default 66) |

### Running

| Method | Description |
| --- | --- |
| `Run(ctx, input) chan Msg` | Start a run and return the event channel; drain it until closed |
| `Stop()` | Cancel the current run; the caller receives `run_stopped` |

### History and introspection

| Method | Description |
| --- | --- |
| `WithHistory(history)` | Seed the previous transcript (**deep copy**) |
| `History()` | Read the current transcript (**returns a copy**) |
| `HasState()` | Whether history exists already |
| `AddTool(tool)` | Register a regular tool (`nil`/unnamed are ignored) |
| `GetTool(name)` / `GetTools()` | Look up tools (`GetTools` returns a copy) |
| `EndToolNames()` | Sorted names of the end tools |
| `Name()` / `Description()` / `SystemPrompt()` / `Lang()` | Basic metadata |

### Key types

```go
type Tool interface {
	Name() string
	Description() string
	Execute(ctx context.Context, args map[string]any) (ToolResult, error)
}

type ToolResult struct {
	Success      bool            // end tool success is what finishes a run
	Error        string          // failure reason (enters the model context)
	Meta         map[string]any  // runtime-recognized metadata, e.g. user_query_language
	ModelContent string          // content entering the model context
	ModelData    map[string]any  // structured data entering the model context
	Events       []Msg           // product-facing events, not sent to the model
}

type Msg struct {
	Type    string
	Content string
	Name    string
	Data    map[string]any
}

type ToolEventEmitter func(Msg)
```

### Runtime defaults (`constants.go`)

| Constant | Value | Meaning |
| --- | --- | --- |
| `defaultMaxIterations` | 66 | Assistant turns allowed per run |
| `eventChannelBuffer` | 256 | Buffer of the channel returned by `Run` |
| `finalEventDeliveryTimeout` | 5s | Fallback delivery timeout for terminal events |
| `heartbeatInterval` | 1s | Heartbeat interval |
| `logContentMaxRunes` | 2000 | Content clipping length in logs |
| `defaultToolResultMaxBytes` | 256 KiB | Cap for one tool result entering the context |
| `noToolCallEscalateAfter` | 2 | Text-only replies before forcing `tool_choice=required` |
| `noToolCallFailAfter` | 5 | Text-only replies before failing the run |
| `llmMaxAttempts` | 3 | Attempts per LLM call |
| `llmRetryBaseDelay` / `llmRetryMaxDelay` | 500ms / 5s | Backoff bounds |

---

## 8. History and persistence

The SDK owns **state for the duration of a run only**; persisting it across turns is the
caller's job (database, Redis, whatever fits):

```go
// after a run
save(chatID, agent.History())

// before the next run
agent := base.NewBaseAgent(..., finishTool{}).WithHistory(load(chatID))
```

Prefer "one conversation turn = one agent instance". When storing history, keep `role`,
`content`, `tool_calls` and `tool_call_id` intact: a transcript with an assistant `tool_calls`
entry but no matching tool results is rejected with HTTP 400 by OpenAI-compatible APIs on the
next request (the in-run "skipped" placeholders only protect a single run).

`AgentContextSnapshot` offers a serializable shape:

```go
type AgentContextSnapshot struct {
	History []openai.ChatCompletionMessage `json:"history,omitempty"`
}
```

Context only grows; for long conversations trim it yourself, or put the trimming policy in a
`MemoryModule`.

---

## 9. Concurrency model

`BaseAgent` holds run-scoped state (transcript, language, memory block, cancel func), so:

- **one run at a time**: a concurrent `Run` immediately returns a `run_error` reading
  `"agent is already running: one BaseAgent instance handles a single run at a time"`;
- `WithHistory` deep-copies its input and `History()`/`GetTools()` return copies, so the caller
  never shares mutable state with the agent;
- tools execute inside the run goroutine — concurrency inside a tool is the tool's business.

---

## 10. Logging and observability

```go
xlog.SetLevel(xlog.ParseLevel(os.Getenv("GOER_AGENT_LOG_LEVEL"))) // debug/info/warn/error/off
```

- `info` by default; `logMessageSizes` (per-request size and token estimate) is `debug`;
- every line carries `chat_id=` (read from `ctxkey.ChatID`);
- model output, tool arguments and tool results are clipped to `logContentMaxRunes`.

---

## 11. Tests

```bash
go test ./...
go test -race ./...

# or via the Makefile
make check
```

20 tests. The core of the suite is a local `httptest` fake LLM (SSE streaming, recording every
request), which allows asserting `tool_choice`, message contents, headers and call counts. They
cover the constructor contract, the end-tool contract, text-only retries and escalation, a
failed end tool not finishing the run, skipped-call placeholders, the stopped terminal event,
panic recovery, LLM retries, tool-result truncation, history copying, custom headers, concurrent
rejection and the tool-reported language.

---

## 12. Known limitations

1. **`skill.go` is not wired up**: nothing uses `Skill`/`BaseSkill`; `AllowedTools` is not
   enforced and `Instruction`/`Model`/`MaxIterations` never reach a run. Either wire it into
   `BaseAgent` (restrict the tool whitelist, apply instructions/model) or delete it.
2. **Fork dependency**: see section 3 — every consumer must repeat the `replace` line.
3. **Unbounded context**: the runtime does not trim the transcript (tool results are capped,
   history is not).
4. **Product conventions inside the SDK**: the chart/dashboard helpers in `attachment.go` belong
   to the product layer; generic consumers can ignore or replace them.

---

## 13. Relationship to `orchestratorv2/agent/base`

This SDK shares its origin with `golang-backend/agents/orchestratorv2/agent/base`. The backend
copy keeps platform dependencies (`llm.Client()` reading environment variables, `locales`,
`utils`), while this SDK parameterizes the token/base URL, memory block, logging and HTTP client
so it can be reused standalone. Both follow the same end-tool semantics (required in the
constructor, text-only never finishes, success finishes, failure continues); keep them in sync
when changing either.
