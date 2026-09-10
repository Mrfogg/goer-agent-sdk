package base

import (
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

func TestNewBaseAgentRequiresEndTool(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewBaseAgent must panic when no end tool is provided")
		}
	}()
	NewBaseAgent("n", "d", "prompt", "model", "token", "")
}

func TestNewBaseAgentRegistersEndToolsAsCallableTools(t *testing.T) {
	agent := NewBaseAgent("n", "d", "prompt", "model", "token", "",
		newStubTool("finish", "a"), newStubTool("abort", "b"))

	if got := strings.Join(agent.EndToolNames(), ","); got != "abort,finish" {
		t.Fatalf("end tools = %q, want %q", got, "abort,finish")
	}
	for _, name := range []string{"finish", "abort"} {
		if _, ok := agent.GetTools()[name]; !ok {
			t.Fatalf("end tool %q must also be registered as a callable tool", name)
		}
	}
	if !agent.isEndTool("finish") || agent.isEndTool("unknown") {
		t.Fatal("isEndTool must only accept registered end tools")
	}
}

func TestAddToolIgnoresNilAndUnnamedTools(t *testing.T) {
	agent := NewBaseAgent("n", "d", "prompt", "model", "token", "", newStubTool("finish", "a"))

	agent.AddTool(nil)
	agent.AddTool(newStubTool("   ", "unnamed"))
	if got := len(agent.GetTools()); got != 1 {
		t.Fatalf("registered tools = %d, want 1", got)
	}

	tools := agent.GetTools()
	tools["injected"] = newStubTool("injected", "c")
	if _, ok := agent.GetTools()["injected"]; ok {
		t.Fatal("GetTools must return a copy, not the internal map")
	}
}

func TestEndToolPromptSectionListsEndTools(t *testing.T) {
	agent := NewBaseAgent("n", "d", "prompt", "model", "token", "",
		newStubTool("finish", "a"), newStubTool("abort", "b"))

	section := agent.endToolPromptSection()
	for _, want := range []string{"END-TOOL MODE", "finish", "abort", "Only a successful call"} {
		if !strings.Contains(section, want) {
			t.Fatalf("prompt section missing %q:\n%s", want, section)
		}
	}
	if !strings.Contains(agent.buildMessages("prompt")[0].Content, "END-TOOL MODE") {
		t.Fatal("system message must carry the end-tool rule")
	}
}

func TestBuildMessagesPrependsLanguageRequirement(t *testing.T) {
	agent := NewBaseAgent("n", "d", "base prompt", "model", "token", "", newStubTool("finish", "a")).
		WithLang("zh-CN")

	system := agent.buildMessages("base prompt")[0].Content
	if !strings.Contains(system, "Language Requirement") || !strings.Contains(system, "zh-CN") {
		t.Fatalf("language requirement missing:\n%s", system)
	}
	if !strings.Contains(system, "base prompt") {
		t.Fatal("base prompt must be preserved")
	}
}

func TestHistoryIsCopiedOnReadAndWrite(t *testing.T) {
	agent := NewBaseAgent("n", "d", "prompt", "model", "token", "", newStubTool("finish", "a"))

	seed := []openai.ChatCompletionMessage{{
		Role:      openai.ChatMessageRoleUser,
		Content:   "seed",
		ToolCalls: []openai.ToolCall{{ID: "call_seed"}},
	}}
	agent.WithHistory(seed)

	seed[0].Content = "mutated"
	seed[0].ToolCalls[0].ID = "mutated"

	history := agent.History()
	if history[0].Content != "seed" || history[0].ToolCalls[0].ID != "call_seed" {
		t.Fatalf("WithHistory must copy the caller's slice: %+v", history[0])
	}

	history[0].Content = "mutated again"
	if agent.History()[0].Content != "seed" {
		t.Fatal("History must return a copy")
	}
	if !agent.HasState() {
		t.Fatal("HasState must report the seeded history")
	}
}

func TestToolResultMessageIsTruncatedAboveBudget(t *testing.T) {
	agent := NewBaseAgent("n", "d", "prompt", "model", "token", "", newStubTool("finish", "a")).
		WithToolResultMaxBytes(512)

	message := agent.toolResultMessageForHistory(ToolResult{
		Success:      true,
		ModelContent: strings.Repeat("x", 10_000),
	}, nil)

	if len(message) > 512 {
		t.Fatalf("tool result message = %d bytes, want <= 512", len(message))
	}
	if !strings.Contains(message, "truncated") {
		t.Fatalf("truncated payload must be marked: %q", message)
	}
}

func TestToolResultMessageKeepsSmallPayloadsIntact(t *testing.T) {
	agent := NewBaseAgent("n", "d", "prompt", "model", "token", "", newStubTool("finish", "a"))

	message := agent.toolResultMessageForHistory(ToolResult{
		Success:      true,
		ModelContent: "small",
	}, nil)

	if !strings.Contains(message, `"content":"small"`) || strings.Contains(message, "truncated") {
		t.Fatalf("small payload must be passed through: %q", message)
	}
}
