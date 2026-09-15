package main

import (
	"context"
	"strings"
	"time"

	base "github.com/Mrfogg/goer-agent-sdk"
	"github.com/sashabaranov/go-openai"
)

// systemPrompt 是这一课唯一的提示词，重点在最后一句：
// 模型必须知道「只有 finish 能结束这一轮」，否则它会用纯文本回答，然后被打回重来。
const systemPrompt = `你是一个问答助手。

规则：
1. 需要「现在」的时间或日期时，调用 now 工具，不要凭记忆猜。
2. 想清楚之后再回答，答案用中文，直接给结论，不要啰嗦。
3. 回答完成后必须调用 finish 提交最终答案。只有 finish 成功返回才能结束这一轮，
   只输出文本会被系统退回并要你重新调用工具。`

// nowTool 是一个「普通工具」：模型干活过程中可以调它。
//
// 注意 ToolResult.Events：它是发给产品侧（也就是我们的前端）的事件，
// 不会进入模型上下文。所以这里既有给模型看的 ModelContent，
// 也有一条给前端看的消息——这就是设计里的「两条通道」。
type nowTool struct{}

func (nowTool) Name() string { return "now" }

func (nowTool) Description() string {
	return "Get the current local date and time. Call it whenever the answer depends on today's date or the current time."
}

func (nowTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	now := time.Now().Format("2006-01-02 15:04:05 Monday")
	return base.ToolResult{
		Success:      true,
		ModelContent: "current local time: " + now, // 进模型上下文
		Events: []base.Msg{{ // 只发给前端
			Type: "tool",
			Data: map[string]any{
				"name":   "now",
				"ok":     true,
				"args":   args,
				"result": now,
			},
		}},
	}, nil
}

// finishTool 是本轮唯一的结束方式。
//
// 它成功返回的那一刻，run 结束，ModelContent 就是交付给用户的答案；
// 它返回失败则 run 继续，模型会看到失败原因并重新组织答案。
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

// newAgent 为「一轮对话」构造一个 agent。
//
// 为什么每轮都新建：BaseAgent 实例持有本轮的状态，一次只跑一个 run；
// 历史由调用方（我们的 sessionStore）持有，用 WithHistory 灌进来。
func newAgent(cfg Config, history []openai.ChatCompletionMessage) *base.BaseAgent {
	agent := base.NewBaseAgent(
		"hello-agent",
		"Hello world Q&A agent",
		systemPrompt,
		cfg.Model,
		cfg.Token,
		cfg.BaseURL,
		finishTool{}, // end tool：构造函数至少要有一个
	)
	agent.AddTool(nowTool{})

	if len(history) > 0 {
		agent.WithHistory(history)
	}
	if cfg.MaxContextTokens > 0 {
		agent.WithMaxContextTokens(cfg.MaxContextTokens)
	}
	return agent
}
