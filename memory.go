package base

import "context"

// MemoryModule is the injection contract for modules that provide both
// memory-management tools and system-prompt enrichment. Once registered on a
// BaseAgent via WithMemory, the agent registers the module's tools and appends
// its prompt block to the system prompt.
//
// This package only defines the contract; memorykit is a ready-made
// implementation backed by SQLite.
type MemoryModule interface {
	Tools() []Tool
	BuildPromptBlock(ctx context.Context) string
}
