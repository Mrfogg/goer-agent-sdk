package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Mrfogg/goer-agent-sdk/memorykit"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

// ---------- 一个假的 OpenAI 服务 ----------
//
// 和第二课的教学目标一致：整条链路（记忆写入、跨会话、删除校验、合并）都能在没有
// key 的情况下断言。这一课比第一课多一种回复形状：合并记忆走的是**非流式**接口
// （CreateChatCompletion），和 agent 主循环的流式请求不是一种东西，所以假上游要认
// 得这两种回复。

// mergePrefix 标记「这是一条非流式的合并回复」。
const mergePrefix = "__merge__"

// knownMemoriesHeader 是记忆模块拼进 system prompt 的那段清单的表头。
// 判断「模型手里有没有记忆」就看它有没有出现。
const knownMemoriesHeader = "Known memories (from earlier sessions):"

type fakeLLM struct {
	mu       sync.Mutex
	replies  []string
	requests []fakeRequest
}

type fakeRequest struct {
	stream   bool
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
		Stream   bool             `json:"stream"`
		Messages []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(body, &request)

	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{stream: request.Stream, messages: request.Messages})
	next := ""
	if len(f.replies) > 0 {
		next, f.replies = f.replies[0], f.replies[1:]
	}
	f.mu.Unlock()

	if next == "" {
		http.Error(w, "fake llm: script exhausted", http.StatusInternalServerError)
		return
	}

	// 合并：回一个普通的 JSON chat completion，不是 SSE。
	if strings.HasPrefix(next, mergePrefix) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.TrimPrefix(next, mergePrefix))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, next)
}

// lastRequestText 把最近一次请求的整段 messages 拼成字符串，用来断言
// 「system prompt 里到底带了什么」——这一课最关键的证据就在这里。
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

// sseToolCall 造一段「模型要求调用工具」的回复。
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

// mergeReply 造一次「合并调用」的回复：非流式的 chat completion，内容是合并模型
// 按约定该吐的那段 JSON。
func mergeReply(t *testing.T, facts []string, merged string) string {
	t.Helper()

	content, err := json.Marshal(map[string]any{"facts": facts, "merged": merged})
	if err != nil {
		t.Fatalf("marshal merge content: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"id":      "chatcmpl-merge",
		"object":  "chat.completion",
		"created": 0,
		"model":   "fake-model",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": string(content)},
			"finish_reason": "stop",
		}},
	})
	if err != nil {
		t.Fatalf("marshal merge reply: %v", err)
	}
	return mergePrefix + string(payload)
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

// onlyResult 断言这一轮恰好结束一次，并返回它的 result 事件。
func onlyResult(t *testing.T, events []sseEvent) map[string]any {
	t.Helper()
	results := eventsNamed(events, "result")
	if len(results) != 1 {
		t.Fatalf("result events = %d, want exactly 1 (%v)", len(results), events)
	}
	if err := results[0].data["err"]; err != "" && err != nil {
		t.Fatalf("run failed: %v", err)
	}
	return results[0].data
}

// ---------- 测试环境 ----------

// testConfig 指向假模型服务，记忆库放在临时目录里。
func testConfig(baseURL, memoryDB string) Config {
	return Config{
		Addr:       ":0",
		Model:      "fake-model",
		Token:      "test-token",
		BaseURL:    baseURL + "/v1",
		MemoryDB:   memoryDB,
		MergeModel: "fake-model",
	}
}

// newTestEnv 起一个假模型服务 + 一个真的 router。
//
// 记忆库路径由调用方给：需要预置记忆的用例（删除、合并）先自己建库写数据，
// 这样脚本里就能引用确定的 memory id。
func newTestEnv(t *testing.T, memoryDB string, replies ...string) (*fakeLLM, *httptest.Server) {
	t.Helper()

	llm := newFakeLLM(replies...)
	llmServer := llm.start(t)

	router, err := newRouter(testConfig(llmServer.URL, memoryDB))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return llm, server
}

func tempMemoryDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "memories.db")
}

// seedMemory 直接往记忆库里写一条，绕过模型——测试要的是「库里已经有什么」，
// 而不是再看一遍模型会不会记。
func seedMemory(t *testing.T, memoryDB, content string) memorykit.Memory {
	t.Helper()

	module, err := memorykit.New(memoryDB, demoUserID)
	if err != nil {
		t.Fatalf("open memory db: %v", err)
	}
	record, _, err := module.Save(context.Background(), content, "")
	if err != nil {
		t.Fatalf("seed memory %q: %v", content, err)
	}
	return record
}

// ---------- 调用接口 ----------

// chat 发一次提问，收完整个 SSE 流再返回解析好的事件。
func chat(t *testing.T, baseURL, sessionID, input string) []sseEvent {
	t.Helper()

	body, _ := json.Marshal(map[string]string{"session_id": sessionID, "input": input})
	resp, err := http.Post(baseURL+"/api/chat", "application/json", strings.NewReader(string(body)))
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

func postJSON(t *testing.T, baseURL, path, body string, out any) {
	t.Helper()

	resp, err := http.Post(baseURL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: status = %d, want 200", path, resp.StatusCode)
	}
	decodeJSON(t, resp.Body, out, path)
}

func getJSON(t *testing.T, baseURL, path string, out any) {
	t.Helper()

	resp, err := http.Get(baseURL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", path, resp.StatusCode)
	}
	decodeJSON(t, resp.Body, out, path)
}

func decodeJSON(t *testing.T, reader io.Reader, out any, source string) {
	t.Helper()
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("parse %s: %v (body: %s)", source, err, raw)
	}
}

// listMemories 直接问接口要「记忆库现在有什么」，而不是去看模块内部状态：
// 记忆库是对话之外的东西，从外面看得见才算真的存下来了。
func listMemories(t *testing.T, baseURL string) []memoryView {
	t.Helper()

	var payload struct {
		Memories []memoryView `json:"memories"`
		Count    int          `json:"count"`
	}
	getJSON(t, baseURL, "/api/memories", &payload)
	if payload.Count != len(payload.Memories) {
		t.Fatalf("count = %d, want %d", payload.Count, len(payload.Memories))
	}
	return payload.Memories
}

func memoryContents(t *testing.T, baseURL string) []string {
	t.Helper()

	records := listMemories(t, baseURL)
	contents := make([]string, 0, len(records))
	for _, record := range records {
		contents = append(contents, record.Content)
	}
	return contents
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// contextEvent 取出这一轮开头那条 context 事件——历史条数和记忆条数都在里面。
func contextEvent(t *testing.T, events []sseEvent) map[string]any {
	t.Helper()
	contexts := eventsNamed(events, "context")
	if len(contexts) != 1 {
		t.Fatalf("context events = %d, want exactly 1 (%v)", len(contexts), events)
	}
	return contexts[0].data
}

// onlyMemoryEvent 断言这一轮恰好发生一次记忆动作，返回它的 payload。
func onlyMemoryEvent(t *testing.T, events []sseEvent) map[string]any {
	t.Helper()
	cards := eventsNamed(events, "memory")
	if len(cards) != 1 {
		t.Fatalf("memory events = %d, want exactly 1 (%v)", len(cards), events)
	}
	return cards[0].data
}

// ---------- 用例 ----------

// TestMemoryOutlivesTheConversation 是这一课的主线：
// 模型把一件事写进记忆库之后，即使会话历史被清空，下一轮它仍然知道。
func TestMemoryOutlivesTheConversation(t *testing.T) {
	const fact = "用户是 Excelmatic 的作者，做 AI 教学视频。"

	llm, server := newTestEnv(t, tempMemoryDB(t),
		// 第一轮：模型调用 memory_write 记下来，然后交付。
		sseToolCall(t, "call_1", "memory_write", `{"content":"`+fact+`"}`, 300, 40),
		sseToolCall(t, "call_2", "finish", `{"answer":"记住了。"}`, 340, 12),
		// 清空历史之后的第二轮：模型直接交付。
		sseToolCall(t, "call_3", "finish", `{"answer":"你是 Excelmatic 的作者。"}`, 360, 16),
	)

	first := chat(t, server.URL, "s1", "我是 Excelmatic 的作者，做 AI 教学视频。")
	if answer := onlyResult(t, first)["answer"]; answer != "记住了。" {
		t.Fatalf("answer = %v, want 记住了。", answer)
	}

	// 记忆卡片：action=saved 携带这条记忆的 id 和内容，模型看不到它。
	card := onlyMemoryEvent(t, first)
	if card["action"] != "saved" {
		t.Fatalf("memory action = %v, want saved", card["action"])
	}
	if card["content"] != fact {
		t.Fatalf("memory content = %v, want %q", card["content"], fact)
	}
	if stored := memoryContents(t, server.URL); len(stored) != 1 || stored[0] != fact {
		t.Fatalf("stored memories = %v, want exactly the fact", stored)
	}

	// 清空历史——记忆库不受影响，这正是 /api/reset 存在的意义。
	var reset struct {
		OK         bool `json:"ok"`
		MemoryKept bool `json:"memory_kept"`
	}
	postJSON(t, server.URL, "/api/reset", `{"session_id":"s1"}`, &reset)
	if !reset.OK || !reset.MemoryKept {
		t.Fatalf("reset = %+v, want ok with memories kept", reset)
	}
	if stored := memoryContents(t, server.URL); len(stored) != 1 {
		t.Fatalf("memories after reset = %v, want them kept", stored)
	}

	second := chat(t, server.URL, "s2", "我是做什么的？")
	ctx := contextEvent(t, second)
	if ctx["history_messages"] != float64(0) {
		t.Fatalf("history_messages = %v, want 0 after a reset", ctx["history_messages"])
	}
	if ctx["memories"] != float64(1) {
		t.Fatalf("memories = %v, want 1", ctx["memories"])
	}

	// 证据在 system prompt 里：记忆清单是模块每轮拼进去的，不在历史里。
	// 断言的是清单的表头，不是「Known memories」这个词——后者在本课的
	// systemPrompt 里也出现过，用它当证据会误判。
	request := llm.lastRequestText(t)
	if !strings.Contains(request, knownMemoriesHeader) {
		t.Fatalf("the system prompt should carry the known memories, got: %s", request)
	}
	if !strings.Contains(request, fact) {
		t.Fatalf("the remembered fact should reach the model, got: %s", request)
	}
}

// TestResetEmptiesTheHistoryAndNothingElse 用同一个 session id 走一遍课堂演示：
// 先让历史攒起来，点「新会话」之后历史归零，记忆一条没少。
func TestResetEmptiesTheHistoryAndNothingElse(t *testing.T) {
	llm, server := newTestEnv(t, tempMemoryDB(t),
		sseToolCall(t, "call_1", "memory_write", `{"content":"用户偏好用中文回复"}`, 280, 30),
		sseToolCall(t, "call_2", "finish", `{"answer":"好。"}`, 300, 8),
		sseToolCall(t, "call_3", "finish", `{"answer":"好的。"}`, 320, 8),
		sseToolCall(t, "call_4", "finish", `{"answer":"我是这样记的。"}`, 340, 8),
	)
	const session = "same-session"

	chat(t, server.URL, session, "以后都用中文回我。")
	second := chat(t, server.URL, session, "再说一遍？")
	if got := contextEvent(t, second)["history_messages"]; got == float64(0) {
		t.Fatal("the second turn should carry the first one as history")
	}

	postJSON(t, server.URL, "/api/reset", `{"session_id":"same-session"}`, &struct{}{})

	third := chat(t, server.URL, session, "我们聊过什么？")
	ctx := contextEvent(t, third)
	if ctx["history_messages"] != float64(0) {
		t.Fatalf("history_messages = %v, want 0 after a reset", ctx["history_messages"])
	}
	if ctx["memories"] != float64(1) {
		t.Fatalf("memories = %v, want the memory to survive", ctx["memories"])
	}

	// 历史真没了：第三次请求里看不到第一轮的用户原话。
	if request := llm.lastRequestText(t); strings.Contains(request, "以后都用中文回我") {
		t.Fatalf("the reset history should not reach the model, got: %s", request)
	}
}

// TestNothingIsRememberedWithoutAMemoryWrite 是「没调用就没记住」：
// memory_write 是唯一的写入口，模型不调它，用户说过的话下一轮就不存在。
func TestNothingIsRememberedWithoutAMemoryWrite(t *testing.T) {
	llm, server := newTestEnv(t, tempMemoryDB(t),
		sseToolCall(t, "call_1", "finish", `{"answer":"好的，我知道了。"}`, 200, 12),
		sseToolCall(t, "call_2", "finish", `{"answer":"这个我确实不知道。"}`, 220, 12),
	)

	const input = "我是做财务分析的，以后回答都用中文。"
	first := chat(t, server.URL, "s3", input)
	onlyResult(t, first)

	if cards := eventsNamed(first, "memory"); len(cards) != 0 {
		t.Fatalf("no memory tool was called, but got %v", cards)
	}
	if stored := listMemories(t, server.URL); len(stored) != 0 {
		t.Fatalf("memories = %v, want none", stored)
	}

	// 第二轮换个会话：模型手里既没有历史，也没有记忆。
	second := chat(t, server.URL, "s4", "我是做什么的？")
	ctx := contextEvent(t, second)
	if ctx["history_messages"] != float64(0) || ctx["memories"] != float64(0) {
		t.Fatalf("second turn carried something: %v", ctx)
	}
	if request := llm.lastRequestText(t); strings.Contains(request, knownMemoriesHeader) {
		t.Fatalf("there should be nothing known, got: %s", request)
	}
}

// TestForgetNeedsTheUsersOwnWords 是「删不删不是模型说了算」：
// memory_forget 要求模型引用用户原话，服务端拿对话核对，核对不过就删不掉。
func TestForgetNeedsTheUsersOwnWords(t *testing.T) {
	memoryDB := tempMemoryDB(t)
	record := seedMemory(t, memoryDB, "用户的邮箱是 me@example.com。")

	_, server := newTestEnv(t, memoryDB,
		// 第一次：模型编了一句用户没说过的话，被拒。
		sseToolCall(t, "call_1", "memory_forget",
			`{"memory_id":"`+record.ID+`","user_quote":"请忘掉这条记忆"}`, 260, 20),
		sseToolCall(t, "call_2", "finish", `{"answer":"我没能删掉它。"}`, 280, 12),
		// 第二次：用户真的说了那句话，引用生效，删除成功。
		sseToolCall(t, "call_3", "memory_forget",
			`{"memory_id":"`+record.ID+`","user_quote":"忘掉我的邮箱"}`, 300, 20),
		sseToolCall(t, "call_4", "finish", `{"answer":"已经忘掉了。"}`, 320, 12),
	)

	refused := chat(t, server.URL, "s5", "帮我删掉邮箱那条记忆。")
	onlyResult(t, refused)

	card := onlyMemoryEvent(t, refused)
	if card["action"] != "rejected" {
		t.Fatalf("memory action = %v, want rejected", card["action"])
	}
	if card["memory_id"] != record.ID {
		t.Fatalf("memory_id = %v, want %q", card["memory_id"], record.ID)
	}
	if reason, _ := card["error"].(string); !strings.Contains(reason, "does not appear") {
		t.Fatalf("the refusal should say the quote is missing, got %q", reason)
	}
	if stored := memoryContents(t, server.URL); len(stored) != 1 {
		t.Fatalf("memories = %v, want the memory untouched", stored)
	}

	accepted := chat(t, server.URL, "s5", "算了，直接忘掉我的邮箱吧。")
	onlyResult(t, accepted)

	deleted := onlyMemoryEvent(t, accepted)
	if deleted["action"] != "deleted" {
		t.Fatalf("memory action = %v, want deleted", deleted["action"])
	}
	if deleted["content"] != "用户的邮箱是 me@example.com。" {
		t.Fatalf("the deleted content should be echoed, got %v", deleted["content"])
	}
	if stored := listMemories(t, server.URL); len(stored) != 0 {
		t.Fatalf("memories = %v, want none", stored)
	}
}

// TestMergeFoldsOverlappingMemories 覆盖合并：模型只挑 id，合并文本由注入的
// 合并模型写，两个来源被一条新记忆替换掉。
func TestMergeFoldsOverlappingMemories(t *testing.T) {
	memoryDB := tempMemoryDB(t)
	first := seedMemory(t, memoryDB, "用户偏好用中文回复")
	second := seedMemory(t, memoryDB, "用户希望用中文回复")

	_, server := newTestEnv(t, memoryDB,
		sseToolCall(t, "call_1", "memory_merge",
			`{"memory_ids":["`+first.ID+`","`+second.ID+`"]}`, 300, 24),
		mergeReply(t, []string{"用户偏好用中文回复", "用户希望用中文回复"}, "用户偏好用中文回复"),
		sseToolCall(t, "call_2", "finish", `{"answer":"已经合并成一条了。"}`, 340, 12),
	)

	events := chat(t, server.URL, "s6", "把重复的记忆合一下。")
	onlyResult(t, events)

	card := onlyMemoryEvent(t, events)
	if card["action"] != "merged" {
		t.Fatalf("memory action = %v, want merged", card["action"])
	}
	folded, _ := card["merged_from"].([]any)
	if len(folded) != 2 {
		t.Fatalf("merged_from = %v, want two ids", card["merged_from"])
	}

	stored := memoryContents(t, server.URL)
	if len(stored) != 1 || stored[0] != "用户偏好用中文回复" {
		t.Fatalf("stored memories = %v, want the merged record only", stored)
	}
}

// TestFailedMergeLeavesTheMemoriesAlone 是合并的底线：
// 合并不了一半一半，失败时来源一条都不能少。
func TestFailedMergeLeavesTheMemoriesAlone(t *testing.T) {
	memoryDB := tempMemoryDB(t)
	first := seedMemory(t, memoryDB, "用户偏好用中文回复")
	second := seedMemory(t, memoryDB, "用户希望用中文回复")

	_, server := newTestEnv(t, memoryDB,
		sseToolCall(t, "call_1", "memory_merge",
			`{"memory_ids":["`+first.ID+`","`+second.ID+`"]}`, 300, 24),
		// 合并模型交回空内容，模块直接拒收。
		mergeReply(t, []string{"用户偏好用中文回复"}, ""),
		sseToolCall(t, "call_2", "finish", `{"answer":"这次没合成功，两条还在。"}`, 340, 12),
	)

	events := chat(t, server.URL, "s7", "把重复的记忆合一下。")
	onlyResult(t, events)

	card := onlyMemoryEvent(t, events)
	if card["action"] != "rejected" {
		t.Fatalf("memory action = %v, want rejected", card["action"])
	}
	if reason, _ := card["error"].(string); !strings.Contains(reason, "left untouched") {
		t.Fatalf("the refusal should say nothing changed, got %q", reason)
	}

	stored := memoryContents(t, server.URL)
	if len(stored) != 2 || !contains(stored, first.Content) || !contains(stored, second.Content) {
		t.Fatalf("stored memories = %v, want both sources kept", stored)
	}
}

// TestClearMemoriesEmptiesTheStore 覆盖产品侧自己的删除路径：
// 它不经过模型，所以不需要用户原话——权限的边界在服务端，不在提示词。
func TestClearMemoriesEmptiesTheStore(t *testing.T) {
	memoryDB := tempMemoryDB(t)
	seedMemory(t, memoryDB, "用户偏好用中文回复")
	seedMemory(t, memoryDB, "用户是数据分析师")

	_, server := newTestEnv(t, memoryDB)

	if stored := listMemories(t, server.URL); len(stored) != 2 {
		t.Fatalf("memories = %v, want two seeded", stored)
	}

	var cleared struct {
		OK      bool `json:"ok"`
		Deleted int  `json:"deleted"`
	}
	postJSON(t, server.URL, "/api/memories/clear", `{}`, &cleared)
	if !cleared.OK || cleared.Deleted != 2 {
		t.Fatalf("clear = %+v, want both memories deleted", cleared)
	}
	if stored := listMemories(t, server.URL); len(stored) != 0 {
		t.Fatalf("memories = %v, want none", stored)
	}
}
