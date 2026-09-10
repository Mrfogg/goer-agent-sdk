package base

import (
	"context"

	"github.com/sashabaranov/go-openai"
	"github.com/sashabaranov/go-openai/jsonschema"
)

// Tool interface: tool implementations are registered globally
type Tool interface {
	Name() string
	Description() string
	Execute(ctx context.Context, args map[string]any) (ToolResult, error)
}

type OpenAIFunctionProvider interface {
	OpenAIFunctionDefinition() *openai.FunctionDefinition
}

type ToolResult struct {
	Success bool
	Error   string

	// Meta carries free-form metadata for the caller's own bookkeeping. The
	// runtime does not interpret it; it is serialized into the tool result
	// payload the model sees.
	Meta map[string]any

	// ModelContent/ModelData are the tool result that should enter the LLM context.
	ModelContent string
	ModelData    map[string]any

	// Events are external-facing messages for the product runtime, frontend, or
	// other consumers. They are dispatched by the agent runtime and are not
	// automatically inserted into the model context.
	Events []Msg
}

type Msg struct {
	Type    string         `json:"type"`
	Content string         `json:"content,omitempty"`
	Name    string         `json:"name,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

type ToolEventEmitter func(Msg)

const (
	MsgTypeProgressUpdate = "progress_update"
	MsgTypeHeartbeat      = "heartbeat"
	MsgTypeChartResult    = "chart_result"
	MsgTypeDashboardHTML  = "dashboard_html"
	MsgTypeStart          = "start"
	MsgTypeMarkdown       = "markdown"
	MsgTypeTaskCompleted  = "task_completed"
	MsgTypeReportStart    = "report_start"
	MsgTypeReportEnd      = "report_end"
	MsgTypeRunDone        = "run_done"
	MsgTypeRunError       = "run_error"
	MsgTypeRunStopped     = "run_stopped"
)

func OpenAIToolDefinition(tool Tool) openai.Tool {
	if provider, ok := tool.(OpenAIFunctionProvider); ok {
		if definition := provider.OpenAIFunctionDefinition(); definition != nil {
			return openai.Tool{
				Type:     openai.ToolTypeFunction,
				Function: definition,
			}
		}
	}

	return openai.Tool{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        tool.Name(),
			Description: tool.Description(),
			Parameters: jsonschema.Definition{
				Type:                 jsonschema.Object,
				Description:          "Tool arguments as a JSON object.",
				AdditionalProperties: true,
			},
		},
	}
}

func OpenAIObjectSchema(properties map[string]jsonschema.Definition, required ...string) jsonschema.Definition {
	schema := jsonschema.Definition{
		Type:                 jsonschema.Object,
		Properties:           properties,
		AdditionalProperties: false,
	}
	if len(required) > 0 {
		schema.Required = append([]string(nil), required...)
	}
	return schema
}

func OpenAIArraySchema(items jsonschema.Definition, description string) jsonschema.Definition {
	return jsonschema.Definition{
		Type:        jsonschema.Array,
		Description: description,
		Items:       &items,
	}
}

func OpenAIStringSchema(description string) jsonschema.Definition {
	return jsonschema.Definition{
		Type:        jsonschema.String,
		Description: description,
	}
}

func OpenAIIntegerSchema(description string) jsonschema.Definition {
	return jsonschema.Definition{
		Type:        jsonschema.Integer,
		Description: description,
	}
}

func OpenAIBooleanSchema(description string) jsonschema.Definition {
	return jsonschema.Definition{
		Type:        jsonschema.Boolean,
		Description: description,
	}
}
