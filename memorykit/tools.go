package memorykit

import (
	"context"
	"fmt"
	"strings"

	base "github.com/Mrfogg/goer-agent-sdk"
	"github.com/Mrfogg/goer-agent-sdk/xlog"

	"github.com/sashabaranov/go-openai"
	"github.com/sashabaranov/go-openai/jsonschema"
)

// Tools returns the memory tools exposed to the model. memory_merge is only
// part of the set when a Merger was configured, because the merge text is
// written by a model call the host supplies.
func (m *Module) Tools() []base.Tool {
	tools := []base.Tool{
		&writeTool{module: m},
	}
	if m != nil && m.merger != nil {
		tools = append(tools, &mergeTool{module: m})
	}
	return append(tools, &forgetTool{module: m})
}

// =========================
// memory_write
// =========================

type writeTool struct {
	module *Module
}

func (t *writeTool) Name() string {
	return "memory_write"
}

func (t *writeTool) Description() string {
	return `Record a durable memory to recall in future sessions, or rewrite one that is already there.
Parameters:
- content (string, REQUIRED): concise, self-contained memory text.
- memory_id (string, OPTIONAL): the existing memory to rewrite. Omit it to record something new; an id that does not exist is refused rather than silently creating a memory.
Credentials and other secrets are rejected. If a known memory already covers this fact, pass its memory_id to rewrite that memory instead of adding a duplicate.`
}

func (t *writeTool) OpenAIFunctionDefinition() *openai.FunctionDefinition {
	return &openai.FunctionDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: base.OpenAIObjectSchema(map[string]jsonschema.Definition{
			"content":   base.OpenAIStringSchema("Concise, self-contained memory text."),
			"memory_id": base.OpenAIStringSchema("The existing memory to rewrite; omit to record a new one."),
		}, "content"),
	}
}

func (t *writeTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	content := strings.TrimSpace(stringArg(args["content"]))
	if content == "" {
		return base.ToolResult{Success: false, Error: "content is required"}, nil
	}
	if kind, found := detectSensitive(content); found {
		return base.ToolResult{Success: false, Error: sensitiveError(kind)}, nil
	}

	record, previous, err := t.module.Save(ctx, content, stringArg(args["memory_id"]))
	if err != nil {
		return base.ToolResult{Success: false, Error: err.Error()}, nil
	}

	modelContent := fmt.Sprintf("Saved memory %s: %s", memoryRef(record), record.Content)
	modelData := map[string]any{
		"memory_id": record.ID,
		"content":   record.Content,
	}
	if previous != nil {
		// A rewrite destroys the text it replaces, so say what went away.
		modelContent = fmt.Sprintf("Rewrote memory %s: %s", memoryRef(record), record.Content)
		modelContent += fmt.Sprintf("\nIt replaced: %s", previous.Content)
		modelData["replaced"] = map[string]any{"content": previous.Content}
	}

	return base.ToolResult{
		Success:      true,
		ModelContent: modelContent,
		ModelData:    modelData,
	}, nil
}

// =========================
// memory_merge
// =========================

type mergeTool struct {
	module *Module
}

func (t *mergeTool) Name() string {
	return "memory_merge"
}

func (t *mergeTool) Description() string {
	return `Fold two or more memories into a single memory: use it for memories that overlap, repeat, or describe the same thing, including when the store needs compressing.
Parameters:
- memory_ids (array of string, REQUIRED): two or more existing memory ids shown as [#id] in known memories.
The merged text is written for you and preserves every distinct fact, so do not supply it. The sources are deleted and replaced by the merged record in one step; if the merge cannot be completed, they are left untouched.`
}

func (t *mergeTool) OpenAIFunctionDefinition() *openai.FunctionDefinition {
	return &openai.FunctionDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: base.OpenAIObjectSchema(map[string]jsonschema.Definition{
			"memory_ids": base.OpenAIArraySchema(
				base.OpenAIStringSchema("An existing memory id."),
				"Two or more memory ids that describe the same fact.",
			),
		}, "memory_ids"),
	}
}

func (t *mergeTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	ids := uniqueIDs(stringSliceArg(args["memory_ids"]))
	if len(ids) < mergeMinSources {
		return base.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("memory_ids needs at least %d distinct ids", mergeMinSources),
		}, nil
	}

	sources, err := t.module.load(ctx, ids)
	if err != nil {
		return base.ToolResult{Success: false, Error: err.Error()}, nil
	}

	if t.module.merger == nil {
		return base.ToolResult{Success: false, Error: "memory merging is not configured"}, nil
	}

	contents := make([]string, 0, len(sources))
	for _, source := range sources {
		contents = append(contents, source.Content)
	}

	merged, err := t.module.merger.Merge(ctx, contents)
	if err != nil {
		xlog.Warn("memory merge failed: uid=%d ids=%v err=%v", t.module.uid, ids, err)
		return base.ToolResult{
			Success: false,
			Error:   "merge failed, memories left untouched: " + err.Error(),
		}, nil
	}

	text, err := validateMergeOutput(sources, merged)
	if err != nil {
		xlog.Warn("memory merge rejected: uid=%d ids=%v err=%v", t.module.uid, ids, err)
		return base.ToolResult{
			Success: false,
			Error:   "merge failed, memories left untouched: " + err.Error(),
		}, nil
	}

	record, err := t.module.replaceWith(ctx, sources, text)
	if err != nil {
		return base.ToolResult{Success: false, Error: err.Error()}, nil
	}

	foldedIDs := make([]string, 0, len(sources))
	folded := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		foldedIDs = append(foldedIDs, source.ID)
		folded = append(folded, map[string]any{
			"memory_id": source.ID,
			"content":   source.Content,
		})
	}

	return base.ToolResult{
		Success: true,
		ModelContent: fmt.Sprintf(
			"Merged %d memories into %s: %s",
			len(sources), memoryRef(record), record.Content,
		),
		ModelData: map[string]any{
			"memory_id":    record.ID,
			"content":      record.Content,
			"merged_from":  foldedIDs,
			"source_count": len(sources),
			"folded":       folded,
		},
	}, nil
}

// =========================
// memory_forget
// =========================

type forgetTool struct {
	module *Module
}

func (t *forgetTool) Name() string {
	return "memory_forget"
}

func (t *forgetTool) Description() string {
	return `Delete a memory because the user asked you to forget it.
Parameters:
- memory_id (string, REQUIRED): the memory id shown as [#id] in known memories.
- user_quote (string, REQUIRED): the user's own words asking to forget it, quoted verbatim from a user message. It is checked against the conversation before anything is deleted.
Deletion is permanent.`
}

func (t *forgetTool) OpenAIFunctionDefinition() *openai.FunctionDefinition {
	return &openai.FunctionDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: base.OpenAIObjectSchema(map[string]jsonschema.Definition{
			"memory_id":  base.OpenAIStringSchema("The existing memory id."),
			"user_quote": base.OpenAIStringSchema("The user's own words asking to forget it."),
		}, "memory_id", "user_quote"),
	}
}

func (t *forgetTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	memoryID := strings.TrimSpace(stringArg(args["memory_id"]))
	if memoryID == "" {
		return base.ToolResult{Success: false, Error: "memory_id is required"}, nil
	}

	// The quote is verified before the lookup so a rejected call cannot reveal
	// whether an id exists.
	if err := VerifyUserQuote(ctx, stringArg(args["user_quote"])); err != nil {
		return base.ToolResult{Success: false, Error: err.Error()}, nil
	}

	record, err := t.module.Forget(ctx, memoryID)
	if err != nil {
		return base.ToolResult{Success: false, Error: err.Error()}, nil
	}

	return base.ToolResult{
		Success:      true,
		ModelContent: fmt.Sprintf("Deleted memory %s: %s", memoryRef(record), record.Content),
		ModelData: map[string]any{
			"memory_id": record.ID,
			"content":   record.Content,
			"deleted":   true,
		},
	}, nil
}

// =========================
// Argument helpers
// =========================

// memoryRef is how a memory is named everywhere the model can see it: the
// prompt's known-memories list and every tool result.
func memoryRef(record Memory) string {
	return fmt.Sprintf("[#%s]", record.ID)
}

func stringArg(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func stringSliceArg(value any) []string {
	switch typed := value.(type) {
	case nil:
		return nil
	case []string:
		return typed
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			values = append(values, stringArg(item))
		}
		return values
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{typed}
	default:
		return nil
	}
}
