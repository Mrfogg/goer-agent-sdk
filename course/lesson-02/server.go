package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	base "github.com/Mrfogg/goer-agent-sdk"
	"github.com/Mrfogg/goer-agent-sdk/memorykit"

	"github.com/gin-gonic/gin"
	"github.com/sashabaranov/go-openai"
)

// 前端页面用 go:embed 打进二进制：教学时只要一个可执行文件，不用管静态目录路径。
//
//go:embed web
var webFiles embed.FS

// demoUserID 是这一课唯一用户的 id。
//
// memorykit 的一个 Module 只负责一个用户的记忆（构造时就绑定了 uid），所以多用户的
// 产品要给每个用户各建一个 Module，uid 从登录态取。这一课只有一个演示用户，写死即可。
const demoUserID uint = 1

// Config 是这一课的全部配置，都从环境变量来。
type Config struct {
	Addr             string
	Model            string
	Token            string
	BaseURL          string
	MemoryDB         string // 记忆库文件（SQLite）
	MergeModel       string // 合并记忆用哪个模型
	MaxContextTokens int
}

func ConfigFromEnv() Config {
	return Config{
		Addr:             envOrDefault("LISTEN_ADDR", ":8080"),
		Model:            envOrDefault("LLM_MODEL", "gpt-4o-mini"),
		Token:            os.Getenv("LLM_TOKEN"),
		BaseURL:          os.Getenv("LLM_BASE_URL"), // 留空就用库的默认端点
		MemoryDB:         envOrDefault("MEMORY_DB", "memories.db"),
		MergeModel:       envOrDefault("MERGE_MODEL", envOrDefault("LLM_MODEL", "gpt-4o-mini")),
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

// newMemoryModule 建这一课的记忆模块。
//
// 它在 newRouter 里建一次，然后每轮请求复用——和「agent 每轮新建」正好相反，
// 因为记忆不属于某一次对话，它属于这个用户。
func newMemoryModule(cfg Config) (*memorykit.Module, error) {
	clientConfig := openai.DefaultConfig(cfg.Token)
	if cfg.BaseURL != "" {
		clientConfig.BaseURL = cfg.BaseURL
	}

	// 合并记忆是**另一次模型调用**，不在主循环里，也不共用 agent 的上下文：
	// 它只拿到要合并的几条记忆文本，回答一段 JSON。
	// SDK 不会把 agent 内部的 client 给你，所以这里自己建一个（同样的 token / base URL），
	// 模型也可以换（MERGE_MODEL）——比如主流程用大模型、合并用便宜的小模型。
	merger := memorykit.NewOpenAIMerger(openai.NewClientWithConfig(clientConfig), cfg.MergeModel)

	// 记忆落在 SQLite 文件里，所以进程重启后还在。
	return memorykit.New(cfg.MemoryDB, demoUserID, memorykit.WithMerger(merger))
}

// sessionStore 保存每个会话的 transcript，和第一课一样。
//
// 这一课要看清它和记忆库的区别：这里的东西**随会话清空**，记忆库里的不会。
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

// memoryView 是记忆在接口上的样子：只暴露前端要显示的字段，不把存储结构直接抖出去。
type memoryView struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"`
}

func newRouter(cfg Config) (*gin.Engine, error) {
	memory, err := newMemoryModule(cfg)
	if err != nil {
		return nil, err
	}

	router := gin.New()
	router.Use(gin.Logger(), gin.Recovery())

	store := newSessionStore()

	web, err := fs.Sub(webFiles, "web")
	if err != nil {
		return nil, err
	}
	assets := http.FileServer(http.FS(web))
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		router.GET(path, gin.WrapH(assets))
	}

	router.GET("/api/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"ok":          true,
			"model":       cfg.Model,
			"merge_model": cfg.MergeModel,
			"memory_db":   cfg.MemoryDB,
		})
	})
	router.POST("/api/chat", chatHandler(cfg, store, memory))

	// 只清会话历史，**不动记忆**。这个接口就是这一课的主线演示：
	// 清完之后模型仍然认得你，因为记忆根本不在历史里。
	router.POST("/api/reset", func(c *gin.Context) {
		var req chatRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		store.reset(sessionIDOr(req.SessionID))
		c.JSON(http.StatusOK, gin.H{"ok": true, "memory_kept": true})
	})

	router.GET("/api/memories", func(c *gin.Context) {
		records, err := memory.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		views := make([]memoryView, 0, len(records))
		for _, record := range records {
			views = append(views, memoryView{
				ID:        record.ID,
				Content:   record.Content,
				CreatedAt: record.CreatedAt,
			})
		}
		c.JSON(http.StatusOK, gin.H{"memories": views, "count": len(views)})
	})

	// 清空记忆库（演示用）。
	//
	// 注意这里和模型那条路的差别：模型只能通过 memory_forget 删，而那个工具要求它
	// 引用用户的原话，服务端会核对。这里是我们自己（产品侧）在删，不经过模型，
	// 所以也就不需要那句话——权限的边界在服务端，不在提示词。
	router.POST("/api/memories/clear", func(c *gin.Context) {
		records, err := memory.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		// memorykit 没有批量删除，只有一条一条的 Forget；循环调用即可。
		deleted := 0
		for _, record := range records {
			if _, err := memory.Forget(c.Request.Context(), record.ID); err != nil {
				log.Printf("[警告] 删除记忆失败：id=%s err=%v", record.ID, err)
				continue
			}
			deleted++
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "deleted": deleted})
	})

	return router, nil
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
// 和第一课相比只多了一件事：每轮开始先推一条 context 事件，把「本轮带了多少历史、
// 多少记忆」报给前端。这条数字是整节课的对照物——历史可以是 0，记忆不会是 0。
func chatHandler(cfg Config, store *sessionStore, memory *memorykit.Module) gin.HandlerFunc {
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

		history := store.history(req.SessionID)
		memories, err := memory.List(c.Request.Context())
		if err != nil {
			// 读不到记忆不该挡住这一轮：模块自己也会降级（BuildPromptBlock 出错时
			// 只返回策略段），这里最多少一条统计。
			log.Printf("[警告] 读取记忆失败：%v", err)
		}
		writer.event("context", gin.H{
			"history_messages": len(history),
			"memories":         len(memories),
		})

		// 一轮一个 agent 实例，把该会话的历史灌回去；记忆模块是常驻的那个。
		agent := newAgent(cfg, memory, history)

		// 用请求的 context 跑 run：浏览器断开（或点了停止）时它会被取消，
		// run 随即结束，Result().Stopped 为 true。
		stream, err := agent.Run(c.Request.Context(), input)
		if err != nil {
			writer.event("error", gin.H{"message": err.Error()})
			return
		}

		// 哪些工具是 end tool：用来判断「交付答案」这一刻到了没有。
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
				// 它就是「交付答案」，前端要单独成块显示，所以这里整段发过去。
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
			case memorykit.MemoryEventType:
				// 记忆变了。这是工具发的产品侧事件，模型看不到它——
				// 前端靠它实时画出「AI 刚才记住了什么」。
				writer.event("memory", msg.Data)
			default:
				// 其他工具通过 ToolResult.Events / ctxkey.ToolEventEmitter 抛出来的事件。
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
// 不 flush 的话前端会一直等缓冲，看起来就像「没有流式」。
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
