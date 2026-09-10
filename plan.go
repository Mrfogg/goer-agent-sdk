package base

import "context"

// PlanModule is a self-contained progress-plan module. Once registered on a
// BaseAgent, the agent registers the module's tool, appends its prompt
// section, resets its state on every run, and checks the plan before a final
// answer is emitted.
type PlanModule interface {
	Tool() Tool
	Prompt() string
	Reset()
	ValidateFinalAnswer(ctx context.Context) error
	CompletePendingTasks() (Msg, bool)
}
