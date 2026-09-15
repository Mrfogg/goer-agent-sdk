package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

// ---------- 一个假的 OpenAI 服务 ----------
//
// 这一课的教学目标之一是「怎么在没有 key 的情况下测一个 agent」：
// 假上游只要按顺序吐出脚本里的 SSE 帧就够了，测试就能断言整条链路。

// blockedReply 让假上游一直挂着，直到客户端断开——用来测「停止」。
const blockedReply = "__block__"

// errorReply 让假上游直接返回 500——用来测「失败的回合」。
const errorReply = "__error__"

type fakeLLM struct {
	mu       sync.Mutex
	replies  []string
	requests []fakeRequest
}

type fakeRequest struct {
	messages []map[string]any
}

func newFakeLLM(replies ...string) *fakeLLM {
	return &fakeLLM{replies: replies}
}

func (f *fakeLLM) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(server.Close)
	return server
}

func (f *fakeLLM) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var request struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(body, &request)

	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{messages: request.Messages})
	next := ""
	if len(f.replies) > 0 {
		next, f.replies = f.replies[0], f.replies[1:]
	}
	f.mu.Unlock()

	if next == blockedReply {
		<-r.Context().Done()
		return
	}
	if next == errorReply {
		http.Error(w, "fake llm: boom", http.StatusInternalServerError)
		return
	}
	if next == "" {
		http.Error(w, "fake llm: script exhausted", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, next)
}

func (f *fakeLLM) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// lastRequestText 把最近一次请求的整段 messages 拼成字符串，方便断言"历史有没有带上"。
func (f *fakeLLM) lastRequestText(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("fake llm received no request")
	}
	encoded, _ := json.Marshal(f.requests[len(f.requests)-1].messages)
	return string(encoded)
}

// ---------- 造 SSE 帧 ----------

func sseFrame(t *testing.T, payload any) string {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	return "data: " + string(encoded) + "\n\n"
}

func sseUsage(t *testing.T, promptTokens, completionTokens int) string {
	t.Helper()
	return sseFrame(t, map[string]any{
		"choices": []any{},
		"model":   "fake-model",
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}) + "data: [DONE]\n\n"
}

// sseToolCall 造一段"模型要求调用工具"的回复。
func sseToolCall(t *testing.T, id, name, arguments string, promptTokens, completionTokens int) string {
	t.Helper()
	return sseFrame(t, map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"index":    0,
					"id":       id,
					"type":     "function",
					"function": map[string]any{"name": name, "arguments": arguments},
				}},
			},
		}},
	}) + sseFrame(t, map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
	}) + sseUsage(t, promptTokens, completionTokens)
}

// sseText 造一段"模型只输出文本"的回复，文本分几个 chunk 吐出来。
func sseText(t *testing.T, chunks []string, promptTokens, completionTokens int) string {
	t.Helper()
	var b strings.Builder
	for i, chunk := range chunks {
		delta := map[string]any{"content": chunk}
		if i == 0 {
			delta["role"] = "assistant"
		}
		b.WriteString(sseFrame(t, map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": delta}},
		}))
	}
	b.WriteString(sseFrame(t, map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	}))
	b.WriteString(sseUsage(t, promptTokens, completionTokens))
	return b.String()
}

// ---------- 解析我们自己的 SSE 响应 ----------

type sseEvent struct {
	name string
	data map[string]any
}

func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var events []sseEvent
	var name string

	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var data map[string]any
			if err := json.Unmarshal([]byte(raw), &data); err != nil {
				t.Fatalf("SSE data is not a JSON object: %q", raw)
			}
			events = append(events, sseEvent{name: name, data: data})
		}
	}
	return events
}

func eventsNamed(events []sseEvent, name string) []sseEvent {
	var matched []sseEvent
	for _, event := range events {
		if event.name == name {
			matched = append(matched, event)
		}
	}
	return matched
}

// ---------- 调用 /api/chat ----------

func chat(t *testing.T, router http.Handler, sessionID, input string) []sseEvent {
	t.Helper()

	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	body, _ := json.Marshal(map[string]string{"session_id": sessionID, "input": input})
	resp, err := http.Post(server.URL+"/api/chat", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST /api/chat: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return parseSSE(t, string(raw))
}

func testConfig(baseURL string) Config {
	return Config{Addr: ":0", Model: "fake-model", Token: "test-token", BaseURL: baseURL + "/v1"}
}

// ---------- 用例 ----------

// TestChatCallsRegularToolThenDeliversWithEndTool 是这一课的主线：
// 模型先调普通工具（now），再调 end tool（finish）交付答案。
func TestChatCallsRegularToolThenDeliversWithEndTool(t *testing.T) {
	llm := newFakeLLM(
		sseToolCall(t, "call_1", "now", "{}", 120, 8),
		sseToolCall(t, "call_2", "finish", `{"answer":"现在是 2026-09-14 15:30。"}`, 140, 20),
	)
	server := llm.start(t)

	events := chat(t, newRouter(testConfig(server.URL)), "s1", "现在几点了？")

	tools := eventsNamed(events, "tool")
	if len(tools) != 2 {
		t.Fatalf("tool events = %d, want 2 (%v)", len(tools), events)
	}
	if tools[0].data["name"] != "now" || tools[1].data["name"] != "finish" {
		t.Fatalf("tool order = %v, %v; want now then finish", tools[0].data["name"], tools[1].data["name"])
	}

	results := eventsNamed(events, "result")
	if len(results) != 1 {
		t.Fatalf("result events = %d, want exactly 1", len(results))
	}
	if answer := results[0].data["answer"]; answer != "现在是 2026-09-14 15:30。" {
		t.Fatalf("answer = %v, want the end tool's ModelContent", answer)
	}
	if stopped := results[0].data["stopped"]; stopped != false {
		t.Fatalf("stopped = %v, want false", stopped)
	}
	if calls := results[0].data["llm_calls"]; calls != float64(2) {
		t.Fatalf("llm_calls = %v, want 2", calls)
	}

	if got := llm.requestCount(); got != 2 {
		t.Fatalf("LLM calls = %d, want 2 (one per tool round)", got)
	}
}

// TestChatPushesBackTextOnlyReply 验证这一课的核心认知：
// 模型只回文本时不结束 run，会被退回并要求调用工具。
func TestChatPushesBackTextOnlyReply(t *testing.T) {
	llm := newFakeLLM(
		// 第一次：模型只说话，不调工具。
		sseText(t, []string{"现在是", "下午三点"}, 100, 10),
		// 被退回之后：这次乖乖调 finish。
		sseToolCall(t, "call_1", "finish", `{"answer":"现在是下午三点。"}`, 160, 12),
	)
	server := llm.start(t)

	events := chat(t, newRouter(testConfig(server.URL)), "s2", "现在几点了？")

	// 文本是累积下发的（模型每吐一个 chunk，SDK 都给"到目前为止的全部文本"），
	// 服务端换算成增量后，这一段应该只开一个块。
	var contentEvents []sseEvent
	var draft strings.Builder
	for _, event := range eventsNamed(events, "text") {
		if event.data["kind"] != "content" {
			continue
		}
		contentEvents = append(contentEvents, event)
		if event.data["delivered"] != true {
			draft.WriteString(event.data["text"].(string))
		}
	}
	// 模型草稿按 chunk 换算成增量：拼起来必须等于原文。
	if got := draft.String(); got != "现在是下午三点" {
		t.Fatalf("streamed draft = %q, want %q", got, "现在是下午三点")
	}
	if contentEvents[0].data["new_block"] != true {
		t.Fatalf("the first content event must open a block, got %v", contentEvents[0].data)
	}

	delivered := contentEvents[len(contentEvents)-1]
	if got := delivered.data["text"]; got != "现在是下午三点。" {
		t.Fatalf("delivered content = %v, want the end tool's answer", got)
	}
	if delivered.data["delivered"] != true || delivered.data["new_block"] != true {
		t.Fatalf("the answer must be a separate delivered block, got %v", delivered.data)
	}

	results := eventsNamed(events, "result")
	if len(results) != 1 || results[0].data["answer"] != "现在是下午三点。" {
		t.Fatalf("result = %v, want the finish answer", results)
	}

	// 退回的证据：第二次请求里能看到"系统要求你调用工具"的那条消息。
	if got := llm.requestCount(); got != 2 {
		t.Fatalf("LLM calls = %d, want 2 (text-only reply must be retried)", got)
	}
	if request := llm.lastRequestText(t); !strings.Contains(request, "did not call any tool") {
		t.Fatalf("second request should carry the corrective message, got: %s", request)
	}
}

// TestChatKeepsHistoryPerSession 验证「状态归谁」：
// SDK 不持久化，是我们自己把 History() 存下来、下一轮用 WithHistory() 灌回去的。
func TestChatKeepsHistoryPerSession(t *testing.T) {
	llm := newFakeLLM(
		sseToolCall(t, "call_1", "finish", `{"answer":"你刚才问的是第一个问题。"}`, 90, 10),
		sseToolCall(t, "call_2", "finish", `{"answer":"第二个问题也答完了。"}`, 200, 10),
	)
	server := llm.start(t)
	router := newRouter(testConfig(server.URL))

	chat(t, router, "same-session", "第一个问题")
	chat(t, router, "same-session", "第二个问题")

	if request := llm.lastRequestText(t); !strings.Contains(request, "你刚才问的是第一个问题。") {
		t.Fatalf("second turn should carry the first answer as history, got: %s", request)
	}
}

// TestChatDoesNotStoreFailedTurn 验证落库时机：
// 失败的回合不写进历史，免得下一轮拿到半截 transcript。
func TestChatDoesNotStoreFailedTurn(t *testing.T) {
	llm := newFakeLLM(
		// SDK 对传输失败会重试 3 次，所以要让整轮失败就得连着失败 3 次。
		errorReply,
		errorReply,
		errorReply,
		sseToolCall(t, "call_1", "finish", `{"answer":"ok"}`, 100, 5),
	)
	server := llm.start(t)
	router := newRouter(testConfig(server.URL))

	events := chat(t, router, "s3", "会失败的第一次")
	if results := eventsNamed(events, "result"); len(results) != 1 || results[0].data["err"] == "" {
		t.Fatalf("expected a failed result, got %v", events)
	}

	chat(t, router, "s3", "第二次")
	if request := llm.lastRequestText(t); strings.Contains(request, "会失败的第一次") {
		t.Fatalf("failed turn must not be stored as history, got: %s", request)
	}
}

// TestChatStopsWhenClientDisconnects 验证「运行点停止」：
// 浏览器断开 → 请求 context 取消 → run 结束，handler 不会一直挂着。
func TestChatStopsWhenClientDisconnects(t *testing.T) {
	llm := newFakeLLM(blockedReply)
	server := llm.start(t)

	routerServer := httptest.NewServer(newRouter(testConfig(server.URL)))
	defer routerServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	body := strings.NewReader(`{"session_id":"s4","input":"会卡住的问题"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, routerServer.URL+"/api/chat", body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/chat: %v", err)
	}
	defer resp.Body.Close()

	cancel() // 相当于前端点了「停止」

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler kept streaming after the client disconnected")
	}
}
