// Package base provides a small model-agnostic agent runtime: a BaseAgent drives
// an OpenAI-compatible chat model through a tool-calling loop until one of its
// registered end tools succeeds.
//
// The runtime owns the conversation for the duration of a run and keeps the
// transcript in memory; persisting it between turns is the caller's job (see
// WithHistory, History and AgentContextSnapshot).
package base

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/sashabaranov/go-openai"
)

// Agent generic agent interface
type Agent interface {
	Name() string
	Description() string
	Run(ctx context.Context, input string) (chan Msg, error)
}

// AgentContextSnapshot is a serializable view of an agent's conversation state,
// so callers can persist it between turns and restore it with WithHistory.
type AgentContextSnapshot struct {
	History []openai.ChatCompletionMessage `json:"history,omitempty"`
}

// BaseAgent base agent implementation.
//
// Configuration (model, tools, prompt, end tools) is set before a run; per-run
// state is owned by the run itself. One BaseAgent therefore handles a single run
// at a time: build one instance per conversation turn, seed it with WithHistory
// and read the updated transcript back through History.
type BaseAgent struct {
	// Configuration.
	name               string
	description        string
	systemPrompt       string
	model              string
	reasoningEffort    string
	lang               string
	maxIterations      int
	authToken          string
	baseURL            string
	httpClient         *http.Client
	httpHeaders        map[string]string
	toolResultMaxBytes int
	client             *openai.Client
	tools              map[string]Tool // global tool registry
	endTools           map[string]Tool // the only tools allowed to finish a run

	planModule   PlanModule
	memoryModule MemoryModule

	// Per-run state, guarded by runMu.
	runMu        sync.Mutex
	running      bool
	runResult    RunResult
	agentHistory []openai.ChatCompletionMessage
	cancelFunc   context.CancelFunc
	memoryBlock  string
}

// NewBaseAgent creates a new base agent.
//
// name, description, and systemPrompt identify the agent and its behavior.
// model, authToken, and baseURL are required for the agent to make LLM calls; an
// empty baseURL falls back to the OpenAI default endpoint.
//
// At least one end tool is required. The agent always runs in end-tool mode: only
// a successful call to one of the end tools can finish a run, and a text-only
// reply is always sent back to the model to continue. It panics when no usable end
// tool is provided.
func NewBaseAgent(name, description, systemPrompt, model, authToken, baseURL string, endTools ...Tool) *BaseAgent {
	agent := &BaseAgent{
		name:               name,
		description:        description,
		systemPrompt:       systemPrompt,
		model:              model,
		maxIterations:      defaultMaxIterations,
		authToken:          authToken,
		baseURL:            baseURL,
		toolResultMaxBytes: defaultToolResultMaxBytes,
		tools:              make(map[string]Tool),
		endTools:           make(map[string]Tool),
	}
	agent.rebuildLLMClient()
	agent.WithEndTools(endTools...)
	if len(agent.endTools) == 0 {
		panic("base agent must be constructed with at least one end tool")
	}
	return agent
}

func (a *BaseAgent) Name() string {
	return a.name
}

func (a *BaseAgent) Description() string {
	return a.description
}

// SystemPrompt returns the base system prompt, without the sections the runtime
// adds per request (language requirement, memory block, end-tool rule).
func (a *BaseAgent) SystemPrompt() string {
	return a.systemPrompt
}

// Lang returns the language the agent must answer in.
func (a *BaseAgent) Lang() string {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return a.lang
}

// WithLang pins the language the agent must answer in.
func (a *BaseAgent) WithLang(lang string) *BaseAgent {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.lang = lang
	return a
}

// WithModel overrides the model used for LLM calls.
func (a *BaseAgent) WithModel(model string) *BaseAgent {
	a.model = model
	return a
}

// WithReasoningEffort enables reasoning-model configuration for LLM calls.
// Passing an empty string disables the reasoning request block.
func (a *BaseAgent) WithReasoningEffort(effort string) *BaseAgent {
	a.reasoningEffort = effort
	return a
}

// WithSystemPrompt replaces the base system prompt.
func (a *BaseAgent) WithSystemPrompt(prompt string) *BaseAgent {
	a.systemPrompt = prompt
	return a
}

// WithMemory injects a memory module and registers its tools.
func (a *BaseAgent) WithMemory(module MemoryModule) *BaseAgent {
	if module == nil {
		return a
	}
	for _, tool := range module.Tools() {
		a.AddTool(tool)
	}
	a.memoryModule = module
	return a
}

// WithPlanModule registers a self-contained plan module. The module's tool is
// registered, its prompt section is appended to the system prompt, and its state
// is reset at the start of every run.
func (a *BaseAgent) WithPlanModule(module PlanModule) *BaseAgent {
	if module == nil {
		return a
	}
	a.planModule = module
	if tool := module.Tool(); tool != nil {
		a.AddTool(tool)
	}
	if prompt := strings.TrimSpace(module.Prompt()); prompt != "" {
		a.systemPrompt = strings.TrimSpace(a.systemPrompt)
		if a.systemPrompt != "" {
			a.systemPrompt += "\n\n" + prompt
		} else {
			a.systemPrompt = prompt
		}
	}
	return a
}

// WithEndTool registers a single tool as one of the agent's explicit termination
// tools. Once any registered end tool succeeds, the agent finishes immediately
// instead of returning to the LLM for another turn.
func (a *BaseAgent) WithEndTool(tool Tool) *BaseAgent {
	return a.WithEndTools(tool)
}

// WithEndTools registers one or more tools as the agent's explicit termination
// tools, on top of the ones passed to NewBaseAgent. A successful end-tool call is
// the only way for a run to finish.
func (a *BaseAgent) WithEndTools(tools ...Tool) *BaseAgent {
	if a.endTools == nil {
		a.endTools = make(map[string]Tool)
	}
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		toolName := strings.TrimSpace(tool.Name())
		if toolName == "" {
			continue
		}
		a.AddTool(tool)
		a.endTools[toolName] = tool
	}
	return a
}

// WithHTTPClient overrides the HTTP client used for LLM calls, for example to
// configure a proxy, a custom transport or connection pool limits.
//
// Note that a per-client http.Client.Timeout also bounds streaming responses;
// prefer passing a deadline on the context given to Run.
func (a *BaseAgent) WithHTTPClient(client *http.Client) *BaseAgent {
	if client == nil {
		return a
	}
	a.httpClient = client
	a.rebuildLLMClient()
	return a
}

// WithHTTPHeaders attaches extra headers (for example referer or title headers
// required by a gateway) to every LLM request.
func (a *BaseAgent) WithHTTPHeaders(headers map[string]string) *BaseAgent {
	if len(headers) == 0 {
		return a
	}
	merged := make(map[string]string, len(headers))
	for key, value := range headers {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		merged[key] = value
	}
	if len(merged) == 0 {
		return a
	}
	a.httpHeaders = merged
	a.rebuildLLMClient()
	return a
}

// WithToolResultMaxBytes caps the JSON payload of a single tool result that
// enters the model context. Oversized results are replaced by a truncated
// payload that keeps the success flag, an error if any, and a clipped preview.
// Pass 0 to disable the cap.
func (a *BaseAgent) WithToolResultMaxBytes(maxBytes int) *BaseAgent {
	a.toolResultMaxBytes = maxBytes
	return a
}

// SetMaxIterations bounds how many agent turns a single run may take.
func (a *BaseAgent) SetMaxIterations(n int) *BaseAgent {
	if n > 0 {
		a.maxIterations = n
	}
	return a
}

// AddTool registers a tool. Nil tools and tools without a name are ignored.
func (a *BaseAgent) AddTool(tool Tool) {
	if tool == nil {
		return
	}
	toolName := strings.TrimSpace(tool.Name())
	if toolName == "" {
		return
	}
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.tools[toolName] = tool
}

// GetTool returns a registered tool by name.
func (a *BaseAgent) GetTool(name string) Tool {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return a.tools[strings.TrimSpace(name)]
}

// GetTools returns all registered tools.
func (a *BaseAgent) GetTools() map[string]Tool {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	tools := make(map[string]Tool, len(a.tools))
	for name, tool := range a.tools {
		tools[name] = tool
	}
	return tools
}

// EndToolNames returns the sorted names of the tools that may finish a run.
func (a *BaseAgent) EndToolNames() []string {
	return a.endToolNames()
}

func (a *BaseAgent) isEndTool(toolName string) bool {
	name := strings.TrimSpace(toolName)
	return name != "" && a.endTools != nil && a.endTools[name] != nil
}

func (a *BaseAgent) endToolNames() []string {
	names := make([]string, 0, len(a.endTools))
	for name := range a.endTools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (a *BaseAgent) memoryPromptBlock() string {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return a.memoryBlock
}
