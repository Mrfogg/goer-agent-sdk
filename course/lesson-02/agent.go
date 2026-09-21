package main

import (
	"context"
	"strings"

	base "github.com/Mrfogg/goer-agent-sdk"

	"github.com/sashabaranov/go-openai"
)

// systemPrompt 故意写得很短——这一课的教学点之一就在这里：
//
// 里面没有一个字讲「什么时候该记、什么时候该忘」。那些内容由记忆模块自己追加：
// WithMemory 会在每次 run 开始时调用 BuildPromptBlock，把 MEMORY POLICY 和
// 「Known memories」清单拼到 system prompt 后面。所以修记忆策略要去改模块，不是在
// 这里再写一遍。
const systemPrompt = `你是一个了解用户的助手。

规则：
1. 回答用中文，直接给结论，不要啰嗦。
2. 涉及用户的身份、偏好或长期要求时，先看 system prompt 里「Known memories」那一段
   已经写下来的内容，不要反过来问用户「你是做什么的」。
3. 回答完成后必须调用 finish 提交最终答案。只有 finish 成功返回才能结束这一轮，
   只输出文本会被系统退回并要你重新调用工具。`

// finishTool 是本轮唯一的结束方式，和第一课完全一样。
//
// 注意这一课没有再自己写记忆工具：memory_write / memory_merge / memory_forget
// 是 memorykit 提供的，WithMemory 一次性注册进来。
type finishTool struct{}

func (finishTool) Name() string { return "finish" }

func (finishTool) Description() string {
	return "Submit the final answer to the user. This is the only way to finish the turn."
}

func (finishTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	answer, _ := args["answer"].(string)
	answer = strings.TrimSpace(answer)

	if answer == "" {
		return base.ToolResult{
			Success: false,
			Error:   "answer is required",
			Events: []base.Msg{{
				Type: "tool",
				Data: map[string]any{"name": "finish", "ok": false, "args": args, "result": "answer is required"},
			}},
		}, nil
	}

	return base.ToolResult{
		Success:      true,
		ModelContent: answer,
		Events: []base.Msg{{
			Type: "tool",
			Data: map[string]any{"name": "finish", "ok": true, "args": args, "result": "已交付"},
		}},
	}, nil
}

// newAgent 为「一轮对话」构造一个 agent，和第一课的结构一样，多了一行 WithMemory。
//
// 两个生命周期在这一行上分开了：
//   - agent 每轮新建（BaseAgent 持有本轮状态，一次只跑一个 run）；
//   - memory module 常驻（它握着 SQLite 连接，跟哪一轮无关），由 newRouter 建一次，
//     每轮传进来。所以历史清空之后，记忆还在——这就是这一课要讲的差异。
func newAgent(cfg Config, memory base.MemoryModule, history []openai.ChatCompletionMessage) *base.BaseAgent {
	agent := base.NewBaseAgent(
		"memory-agent",
		"An agent that remembers the user across sessions",
		systemPrompt,
		cfg.Model,
		cfg.Token,
		cfg.BaseURL,
		finishTool{}, // end tool：构造函数至少要有一个
	)

	// 这一行做两件事：注册记忆工具，并把模块的提示词段落接到 system prompt 后面。
	agent.WithMemory(memory)

	if len(history) > 0 {
		agent.WithHistory(history)
	}
	if cfg.MaxContextTokens > 0 {
		agent.WithMaxContextTokens(cfg.MaxContextTokens)
	}
	return agent
}
