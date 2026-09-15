package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	base "github.com/Mrfogg/goer-agent-sdk"
	"github.com/gin-gonic/gin"
	"github.com/sashabaranov/go-openai"
)

// 前端页面用 go:embed 打进二进制：教学时只要一个可执行文件，不用管静态目录路径。
//
//go:embed web
var webFiles embed.FS

// Config 是这一课的全部配置，都从环境变量来。
type Config struct {
	Addr             string
	Model            string
	Token            string
	BaseURL          string
	MaxContextTokens int
}

func ConfigFromEnv() Config {
	return Config{
		Addr:             envOrDefault("LISTEN_ADDR", ":8080"),
		Model:            envOrDefault("LLM_MODEL", "gpt-4o-mini"),
		Token:            os.Getenv("LLM_TOKEN"),
		BaseURL:          os.Getenv("LLM_BASE_URL"), // 留空就用库的默认端点
		MaxContextTokens: envInt("MAX_CONTEXT_TOKENS"),
	}
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return 0
	}
	return value
}

// sessionStore 保存每个会话的 transcript。
//
// 这一层是「状态归谁」的答案：SDK 只负责把 History() 交给你、再用 WithHistory() 收回去，
// 存哪里、存多久、要不要落库都是调用方的事。这里为了教学用内存 map 就够了。
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string][]openai.ChatCompletionMessage
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string][]openai.ChatCompletionMessage)}
}

func (s *sessionStore) history(sessionID string) []openai.ChatCompletionMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[sessionID] // History()/WithHistory() 自己会深拷贝，这里不必再拷
}

func (s *sessionStore) save(sessionID string, history []openai.ChatCompletionMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sessionID] = history
}

func (s *sessionStore) reset(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

func newRouter(cfg Config) *gin.Engine {
	router := gin.New()
	router.Use(gin.Logger(), gin.Recovery())

	store := newSessionStore()

	web, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	assets := http.FileServer(http.FS(web))
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		router.GET(path, gin.WrapH(assets))
	}

	router.GET("/api/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true, "model": cfg.Model})
	})
	router.POST("/api/chat", chatHandler(cfg, store))
	router.POST("/api/reset", func(c *gin.Context) {
		var req chatRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		store.reset(sessionIDOr(req.SessionID))
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	return router
}

type chatRequest struct {
	SessionID string `json:"session_id"`
	Input     string `json:"input"`
}

func sessionIDOr(sessionID string) string {
	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		return sessionID
	}
	return "default"
}

// chatHandler 是这一课的核心：一次提问 = 一次 run，run 的输出用 SSE 实时推给浏览器。
//
// 用 POST + SSE 而不是 EventSource，是因为 EventSource 只能发 GET（提问内容塞不进 URL）。
// 协议本身还是标准 SSE 帧，前端的解析见 web/app.js。
func chatHandler(cfg Config, store *sessionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req chatRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
			return
		}
		input := strings.TrimSpace(req.Input)
		if input == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "input is required"})
			return
		}
		req.SessionID = sessionIDOr(req.SessionID)

		writer := newSSEWriter(c.Writer)

		// 一轮一个 agent 实例，并把该会话的历史灌回去。
		agent := newAgent(cfg, store.history(req.SessionID))

		// 用请求的 context 跑 run：浏览器断开（或点了停止）时它会被取消，
		// run 随即结束，Result().Stopped 为 true。
		stream, err := agent.Run(c.Request.Context(), input)
		if err != nil {
			writer.event("error", gin.H{"message": err.Error()})
			return
		}

		// 哪些工具是 end tool：用来判断"交付答案"这一刻到了没有。
		endTools := make(map[string]bool)
		for _, name := range agent.EndToolNames() {
			endTools[name] = true
		}

		text := &textStream{}
		endToolSucceeded := false
		for msg := range stream {
			switch msg.Type {
			case base.MsgTypeReasoning, base.MsgTypeContent:
				kind := "reasoning"
				if msg.Type == base.MsgTypeContent {
					kind = "content"
				}

				// end tool 成功之后，run 会把它的 ModelContent 作为最后一条 content 发出来。
				// 它就是"交付答案"，前端要单独成块显示，所以这里整段发过去。
				if kind == "content" && endToolSucceeded {
					writer.event("text", gin.H{
						"kind": kind, "text": msg.Content, "new_block": true, "delivered": true,
					})
					continue
				}

				chunk, newBlock := text.delta(kind, msg.Content)
				writer.event("text", gin.H{"kind": kind, "text": chunk, "new_block": newBlock})
			case base.MsgTypeUsage:
				writer.event("usage", msg.Data)
			default:
				// 工具通过 ToolResult.Events / ctxkey.ToolEventEmitter 抛出来的事件。
				writer.event("tool", msg.Data)
				if ok, _ := msg.Data["ok"].(bool); ok {
					if name, _ := msg.Data["name"].(string); endTools[name] {
						endToolSucceeded = true
					}
				}
			}
		}

		result := agent.Result()

		// 只有正常结束的回合才落库：被中断或失败的回合丢掉，
		// 免得下一轮拿到半截 transcript。
		if result.Err == nil && !result.Stopped {
			store.save(req.SessionID, agent.History())
		}

		writer.event("result", gin.H{
			"answer":    result.Answer,
			"stopped":   result.Stopped,
			"err":       errText(result.Err),
			"llm_calls": result.LLMCalls,
			"usage":     result.Usage,
		})
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// sseWriter 负责把一条消息写成一个 SSE 帧并立刻 flush。
// 不 flush 的话前端会一直等缓冲，看起来就像"没有流式"。
type sseWriter struct {
	w gin.ResponseWriter
}

func newSSEWriter(w gin.ResponseWriter) *sseWriter {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 让 nginx 之类的反代别缓冲
	w.WriteHeader(http.StatusOK)
	w.Flush()
	return &sseWriter{w: w}
}

func (s *sseWriter) event(name string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	// 帧格式：event 行 + data 行 + 空行。json.Marshal 出来是单行的，不会撑破 data 行。
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, data)
	s.w.Flush()
}

// textStream 把 SDK 的累积文本换算成增量，顺便告诉前端什么时候该另起一段。
//
// 为什么要换算：SDK 的 reasoning / content 消息给的是「到目前为止的全部文本」，
// 而且每次 LLM 调用都会从零重新累积（一轮 run 里可能调用模型多次）。
// 前端要的是增量，所以在这里做一次减法：新文本是旧文本的续写就发差值，
// 否则说明新的一次回复开始了，标记 new_block。
type textStream struct {
	reasoning string
	content   string
}

func (t *textStream) delta(kind, accumulated string) (text string, newBlock bool) {
	last := t.content
	if kind == "reasoning" {
		last = t.reasoning
	}

	switch {
	case last == "":
		// 本轮第一段内容：前端要开一个新块。
		text, newBlock = accumulated, true
	case strings.HasPrefix(accumulated, last):
		// 同一段内容的续写：只发差值。
		text, newBlock = strings.TrimPrefix(accumulated, last), false
	default:
		// 累积序列重置：新的一次回复开始了。
		text, newBlock = accumulated, true
	}

	if kind == "reasoning" {
		t.reasoning = accumulated
	} else {
		t.content = accumulated
	}
	return text, newBlock
}
