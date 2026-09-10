package base

// NOTE: Skill/BaseSkill are not wired into BaseAgent. Nothing in this SDK
// consumes the Skill interface, and AllowedTools/ToolDescriptions are not
// enforced anywhere, while NewBaseSkill's model/maxIterations never reach a run.
// The type is kept as a placeholder: either wire it into BaseAgent (restrict
// openAITools to AllowedTools and apply Instruction/Model/MaxIterations) or drop
// it before publishing the SDK, so it does not look like a working feature.

// =========================
// Skill Implementation
// =========================
// Skill interface: a skill defines the current task, visible tools, and prompt behavior
type Skill interface {
	Name() string
	Description() string
	Instruction() string
	AllowedTools() []string
	ToolDescriptions() map[string]string
	Model() string
	MaxIterations() int
	GetAllTool() []Tool
}

// BaseSkill is a generic skill implementation
type BaseSkill struct {
	name          string
	description   string
	instruction   string
	tools         []Tool
	model         string
	maxIterations int
}

func NewBaseSkill(
	name string,
	description string,
	instruction string,
	tools []Tool,
	model string,
	maxIterations int,
) *BaseSkill {
	if maxIterations <= 0 {
		maxIterations = 5
	}

	return &BaseSkill{
		name:          name,
		description:   description,
		instruction:   instruction,
		tools:         tools,
		model:         model,
		maxIterations: maxIterations,
	}
}

func (s *BaseSkill) Name() string        { return s.name }
func (s *BaseSkill) Description() string { return s.description }
func (s *BaseSkill) Instruction() string { return s.instruction }
func (s *BaseSkill) AllowedTools() []string {
	allowed := make([]string, len(s.tools))
	for i, tool := range s.tools {
		allowed[i] = tool.Name()
	}
	return allowed
}
func (s *BaseSkill) GetAllTool() []Tool {
	return s.tools
}
func (s *BaseSkill) ToolDescriptions() map[string]string {
	descriptions := make(map[string]string)
	for _, tool := range s.tools {
		descriptions[tool.Name()] = tool.Description()
	}
	return descriptions
}
func (s *BaseSkill) Model() string      { return s.model }
func (s *BaseSkill) MaxIterations() int { return s.maxIterations }
