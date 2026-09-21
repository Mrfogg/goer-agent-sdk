package memorykit

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mrfogg/goer-agent-sdk/ctxkey"

	"github.com/glebarez/sqlite"
	"github.com/sashabaranov/go-openai"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newTestDB opens a throwaway SQLite file with the memory table already
// migrated, so tests can share one database across modules or keep it private.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "memory.db")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	return db
}

func newTestModule(t *testing.T, uid uint, opts ...Option) *Module {
	t.Helper()
	return NewWithDB(newTestDB(t), uid, opts...)
}

func contextWithUserMessages(messages ...string) context.Context {
	history := make([]openai.ChatCompletionMessage, 0, len(messages))
	for _, message := range messages {
		history = append(history, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: message,
		})
	}
	return context.WithValue(context.Background(), ctxkey.AgentHistory, history)
}

func addMemory(t *testing.T, module *Module, content string) Memory {
	t.Helper()

	record, _, err := module.Save(context.Background(), content, "")
	if err != nil {
		t.Fatalf("seed memory %q: %v", content, err)
	}
	return record
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// A merge needs a model call, so memory_merge is only offered when the host
// supplied one: the model must never be handed an action that cannot work.
func TestToolsOnlyIncludeMergeWhenAMergerIsConfigured(t *testing.T) {
	without := newTestModule(t, 7).Tools()
	names := make([]string, 0, len(without))
	for _, tool := range without {
		names = append(names, tool.Name())
	}
	if got := strings.Join(names, ","); got != "memory_write,memory_forget" {
		t.Fatalf("expected write and forget only, got %q", got)
	}

	with := newTestModule(t, 7, WithMerger(MergerFunc(nil))).Tools()
	names = names[:0]
	for _, tool := range with {
		names = append(names, tool.Name())
	}
	if got := strings.Join(names, ","); got != "memory_write,memory_merge,memory_forget" {
		t.Fatalf("expected all three memory tools, got %q", got)
	}
}

// New is the SQLite entry point: it opens the file, migrates the schema on its
// own, and the module works immediately.
func TestNewOpensMigratesAndStores(t *testing.T) {
	module, err := New(filepath.Join(t.TempDir(), "memory.db"), 7)
	if err != nil {
		t.Fatalf("open memory module: %v", err)
	}

	if _, _, err := module.Save(context.Background(), "用户偏好用中文回复", ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	stored, err := module.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 1 || stored[0].Content != "用户偏好用中文回复" {
		t.Fatalf("expected the memory to survive a round trip, got %+v", stored)
	}
	if stored[0].UID != 7 {
		t.Fatalf("expected the memory to belong to uid 7, got %d", stored[0].UID)
	}
}

// Memories are per user: one module must never read or delete another's.
func TestListReturnsOnlyTheModulesUsersMemories(t *testing.T) {
	db := newTestDB(t)
	mine := NewWithDB(db, 7)
	theirs := NewWithDB(db, 9)
	ctx := context.Background()

	mineRecord := addMemory(t, mine, "用户偏好用中文回复")
	theirsRecord := addMemory(t, theirs, "另一个人的偏好")

	stored, err := mine.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 1 || stored[0].ID != mineRecord.ID {
		t.Fatalf("expected only this user's memory, got %+v", stored)
	}

	// Naming somebody else's id reads as "not found" rather than revealing it.
	if _, err := mine.Forget(ctx, theirsRecord.ID); !errors.Is(err, errMemoryNotFound) {
		t.Fatalf("expected another user's id to be not found, got %v", err)
	}
	if _, _, err := mine.Save(ctx, "改成英文回复", theirsRecord.ID); !errors.Is(err, errMemoryNotFound) {
		t.Fatalf("expected another user's id to be not found on write, got %v", err)
	}
	survived, err := theirs.List(ctx)
	if err != nil {
		t.Fatalf("list the other user: %v", err)
	}
	if len(survived) != 1 || survived[0].Content != "另一个人的偏好" {
		t.Fatalf("expected the other user's memory untouched, got %+v", survived)
	}
}

func TestWriteToolRecordsAndRewrites(t *testing.T) {
	module := newTestModule(t, 7)
	write := &writeTool{module: module}
	ctx := context.Background()

	saved, err := write.Execute(ctx, map[string]any{"content": "用户偏好用中文回复"})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !saved.Success {
		t.Fatalf("expected the memory to be saved, got %q", saved.Error)
	}
	if !strings.Contains(saved.ModelContent, "Saved") {
		t.Fatalf("expected a save to be reported as one, got %q", saved.ModelContent)
	}
	firstID, _ := saved.ModelData["memory_id"].(string)
	if firstID == "" {
		t.Fatal("expected the write to return an id")
	}
	if _, ok := saved.ModelData["replaced"]; ok {
		t.Fatalf("expected no replaced content on a fresh write, got %v", saved.ModelData)
	}

	rewritten, err := write.Execute(ctx, map[string]any{
		"memory_id": firstID,
		"content":   "用户偏好用中文回复，并且回答尽量简短",
	})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !rewritten.Success {
		t.Fatalf("expected the rewrite to succeed, got %q", rewritten.Error)
	}
	if rewritten.ModelData["memory_id"] != firstID {
		t.Fatalf("expected the same record to be rewritten, got %v", rewritten.ModelData)
	}
	if !strings.Contains(rewritten.ModelContent, "Rewrote") {
		t.Fatalf("expected a rewrite to be reported as one, got %q", rewritten.ModelContent)
	}
	// The replaced text has to come back: it no longer exists anywhere else.
	if !strings.Contains(rewritten.ModelContent, "用户偏好用中文回复") {
		t.Fatalf("expected the replaced content to be echoed, got %q", rewritten.ModelContent)
	}

	stored, err := module.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("expected one record, got %+v", stored)
	}

	// A no-op rewrite replaces nothing, so there is nothing to report.
	same, err := write.Execute(ctx, map[string]any{
		"memory_id": firstID,
		"content":   "用户偏好用中文回复，并且回答尽量简短",
	})
	if err != nil {
		t.Fatalf("no-op rewrite: %v", err)
	}
	if _, ok := same.ModelData["replaced"]; ok {
		t.Fatalf("expected an unchanged rewrite to replace nothing, got %v", same.ModelData)
	}
}

func TestDetectSensitive(t *testing.T) {
	sensitive := []string{
		"用户的 API key 是 sk-abcdefghijklmnopqrstuvwxyz",
		"用户的密码是 hunter2secret",
		"用户的卡号 4111 1111 1111 1111",
		"-----BEGIN RSA PRIVATE KEY-----",
	}
	for _, content := range sensitive {
		if _, found := detectSensitive(content); !found {
			t.Errorf("expected %q to be flagged as sensitive", content)
		}
	}

	allowed := []string{
		"用户偏好用中文回复，回答尽量简短",
		"用户提到 token 过期会让登录失败，希望提前提醒",
		"用户是数据分析师，日常使用 Excel 和 SQL",
	}
	for _, content := range allowed {
		if kind, found := detectSensitive(content); found {
			t.Errorf("expected %q to be allowed, got %s", content, kind)
		}
	}
}

func TestWriteToolRefusesSecretsWithoutStoring(t *testing.T) {
	module := newTestModule(t, 7)
	write := &writeTool{module: module}

	result, err := write.Execute(context.Background(), map[string]any{
		"content": "用户的密码是 hunter2secret",
	})
	if err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if result.Success {
		t.Fatal("expected the secret to be rejected")
	}
	if !strings.Contains(result.Error, "never stored in memory") {
		t.Fatalf("expected an actionable refusal, got %q", result.Error)
	}

	stored, err := module.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("expected nothing to be stored, got %+v", stored)
	}
}

func TestWriteRefusesAMemoryIDThatDoesNotExist(t *testing.T) {
	module := newTestModule(t, 7)
	write := &writeTool{module: module}

	// Naming an id means "rewrite this"; a miss is not a licence to insert.
	result, err := write.Execute(context.Background(), map[string]any{
		"memory_id": "does-not-exist",
		"content":   "任何内容",
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if result.Success {
		t.Fatal("expected an unknown id to fail")
	}
	if !strings.Contains(result.Error, "not found") {
		t.Fatalf("expected a not-found error, got %q", result.Error)
	}

	stored, err := module.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("expected nothing to be written, got %+v", stored)
	}
}

func TestForgetToolRequiresAQuoteFromTheUser(t *testing.T) {
	module := newTestModule(t, 7)
	forget := &forgetTool{module: module}

	record := addMemory(t, module, "用户偏好用中文回复")

	// A quote nobody said must not delete anything.
	result, err := forget.Execute(context.Background(), map[string]any{
		"memory_id":  record.ID,
		"user_quote": "请忘掉这条记忆",
	})
	if err != nil {
		t.Fatalf("forget without evidence: %v", err)
	}
	if result.Success {
		t.Fatal("expected a quote outside the conversation to be rejected")
	}
	stored, err := module.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("expected the memory to survive, got %+v", stored)
	}

	// The user's own words, with different spacing and punctuation, must pass.
	ctx := contextWithUserMessages("以后别用中文回我了，请忘掉这条记忆。")
	result, err = forget.Execute(ctx, map[string]any{
		"memory_id":  record.ID,
		"user_quote": "请忘掉这条记忆",
	})
	if err != nil {
		t.Fatalf("forget with evidence: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected the quoted request to delete the memory, got %q", result.Error)
	}
	if !strings.Contains(result.ModelContent, "用户偏好用中文回复") {
		t.Fatalf("expected the deleted content to be echoed, got %q", result.ModelContent)
	}

	stored, err = module.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("expected the memory to be gone, got %+v", stored)
	}
}

func TestMergeToolNeedsAtLeastTwoDistinctIDs(t *testing.T) {
	module := newTestModule(t, 7, WithMerger(MergerFunc(nil)))
	merge := &mergeTool{module: module}

	result, err := merge.Execute(context.Background(), map[string]any{
		"memory_ids": []any{"only-one"},
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if result.Success {
		t.Fatal("expected a single id to be rejected")
	}
	if !strings.Contains(result.Error, "at least 2") {
		t.Fatalf("expected a minimum-count error, got %q", result.Error)
	}
}

func TestMergeToolFoldsMemoriesWithTheInjectedMerger(t *testing.T) {
	var handed []string
	merger := MergerFunc(func(_ context.Context, contents []string) (Merged, error) {
		handed = append([]string(nil), contents...)
		return Merged{
			Facts:  []string{"用户偏好用中文回复", "用户希望用中文回复"},
			Merged: "用户偏好用中文回复",
		}, nil
	})
	module := newTestModule(t, 7, WithMerger(merger))
	merge := &mergeTool{module: module}
	ctx := context.Background()

	first := addMemory(t, module, "用户偏好用中文回复")
	second := addMemory(t, module, "用户希望用中文回复")

	result, err := merge.Execute(ctx, map[string]any{
		"memory_ids": []any{first.ID, second.ID},
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected the merge to succeed, got %q", result.Error)
	}
	// The sources come back ordered by creation time, which is only second
	// precision, so both texts are checked for presence rather than position.
	if len(handed) != 2 || !contains(handed, first.Content) || !contains(handed, second.Content) {
		t.Fatalf("expected both source texts to reach the merger, got %v", handed)
	}
	if result.ModelData["source_count"] != 2 {
		t.Fatalf("expected the source count in the result, got %v", result.ModelData)
	}
	if !strings.Contains(result.ModelContent, "Merged 2 memories") {
		t.Fatalf("expected the merge to be reported, got %q", result.ModelContent)
	}

	stored, err := module.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 1 || stored[0].Content != "用户偏好用中文回复" {
		t.Fatalf("expected the sources to be replaced by one record, got %+v", stored)
	}
	if stored[0].ID == first.ID || stored[0].ID == second.ID {
		t.Fatalf("expected a fresh record for the merge, got %q", stored[0].ID)
	}
}

// A merge that cannot be completed must leave the sources exactly as they were:
// losing them would lose the memories the user still has.
func TestMergeToolLeavesMemoriesUntouchedWhenTheMergerFails(t *testing.T) {
	merger := MergerFunc(func(context.Context, []string) (Merged, error) {
		return Merged{}, errors.New("model is down")
	})
	module := newTestModule(t, 7, WithMerger(merger))
	merge := &mergeTool{module: module}
	ctx := context.Background()

	first := addMemory(t, module, "用户偏好用中文回复")
	second := addMemory(t, module, "用户希望用中文回复")

	result, err := merge.Execute(ctx, map[string]any{"memory_ids": []any{first.ID, second.ID}})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if result.Success {
		t.Fatal("expected a failed merge to be reported as failed")
	}
	if !strings.Contains(result.Error, "left untouched") {
		t.Fatalf("expected the refusal to say nothing changed, got %q", result.Error)
	}

	stored, err := module.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("expected both memories to survive, got %+v", stored)
	}
}

// A merger that invents detail is refused by the module, not trusted.
func TestMergeToolRefusesAnExpandedMerge(t *testing.T) {
	merger := MergerFunc(func(context.Context, []string) (Merged, error) {
		return Merged{
			Facts:  []string{"用户偏好用中文回复"},
			Merged: strings.Repeat("用户偏好用中文回复并且要求很多额外的细节", 5),
		}, nil
	})
	module := newTestModule(t, 7, WithMerger(merger))
	merge := &mergeTool{module: module}
	ctx := context.Background()

	first := addMemory(t, module, "用户偏好用中文回复")
	second := addMemory(t, module, "用户希望用中文回复")

	result, err := merge.Execute(ctx, map[string]any{"memory_ids": []any{first.ID, second.ID}})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if result.Success {
		t.Fatal("expected a longer-than-sources merge to be refused")
	}

	stored, err := module.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("expected both memories to survive, got %+v", stored)
	}
}

func TestLoadRejectsUnknownIDs(t *testing.T) {
	module := newTestModule(t, 7)
	ctx := context.Background()

	first := addMemory(t, module, "用户偏好用中文回复")
	second := addMemory(t, module, "用户希望回答简短一些")

	if _, err := module.load(ctx, []string{first.ID, second.ID}); err != nil {
		t.Fatalf("load known ids: %v", err)
	}

	if _, err := module.load(ctx, []string{first.ID, "missing"}); err == nil {
		t.Fatal("expected a missing id to fail the load")
	} else if !strings.Contains(err.Error(), "could not load") {
		t.Fatalf("expected a load error, got %v", err)
	}
}

func TestReplaceWithFoldsSourcesInOneTransaction(t *testing.T) {
	module := newTestModule(t, 7)
	ctx := context.Background()

	first := addMemory(t, module, "用户偏好用中文回复")
	second := addMemory(t, module, "用户希望用中文回复")
	untouched := addMemory(t, module, "用户是数据分析师")

	merged, err := module.replaceWith(ctx, []Memory{first, second}, "用户偏好用中文回复")
	if err != nil {
		t.Fatalf("replace: %v", err)
	}

	stored, err := module.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("expected two sources to collapse into one, got %+v", stored)
	}
	ids := []string{stored[0].ID, stored[1].ID}
	if ids[0] != merged.ID && ids[1] != merged.ID {
		t.Fatalf("expected the merged record to be stored, got %+v", stored)
	}
	if ids[0] != untouched.ID && ids[1] != untouched.ID {
		t.Fatalf("expected the untouched record to survive, got %+v", stored)
	}
}

func TestReplaceWithAbortsWhenASourceIsStale(t *testing.T) {
	module := newTestModule(t, 7)
	ctx := context.Background()

	first := addMemory(t, module, "用户偏好用中文回复")
	second := addMemory(t, module, "用户希望用中文回复")

	stale := second
	if _, err := module.Forget(ctx, stale.ID); err != nil {
		t.Fatalf("forget: %v", err)
	}

	before, err := module.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if _, err := module.replaceWith(ctx, []Memory{first, stale}, "合并结果"); err == nil {
		t.Fatal("expected a stale source to abort the merge")
	}

	after, err := module.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("expected nothing to change, before=%d after=%d", len(before), len(after))
	}
	if len(after) != 1 || after[0].ID != first.ID {
		t.Fatalf("expected the surviving source to be untouched, got %+v", after)
	}
}

func TestValidateMergeOutputEnforcesTheMergeContract(t *testing.T) {
	sources := []Memory{
		{ID: "a", Content: "用户偏好用中文回复"},
		{ID: "b", Content: "用户希望用中文回复"},
	}

	valid, err := validateMergeOutput(sources, Merged{
		Facts:  []string{"用户偏好用中文回复", "用户希望用中文回复"},
		Merged: "用户偏好并且希望用中文回复",
	})
	if err != nil {
		t.Fatalf("expected a valid merge to pass, got %v", err)
	}
	if valid == "" {
		t.Fatal("expected merged content")
	}

	// The limit is the combined length of the sources, so an expansion that
	// adds invented detail is what this check exists to stop.
	cases := map[string]Merged{
		"empty content": {Facts: []string{"x"}, Merged: "   "},
		"no facts":      {Facts: nil, Merged: "用户偏好用中文回复"},
		"too long": {
			Facts:  []string{"x", "y"},
			Merged: strings.Repeat("很长的合并结果", 20),
		},
	}
	for name, merged := range cases {
		if _, err := validateMergeOutput(sources, merged); err == nil {
			t.Errorf("expected %s to be rejected", name)
		}
	}
}

func TestExtractJSONObjectToleratesProseAndFences(t *testing.T) {
	cases := map[string]string{
		"```json\n{\"merged\":\"a\"}\n```": "{\"merged\":\"a\"}",
		"Here you go: {\"merged\":\"a\"}":  "{\"merged\":\"a\"}",
		"{\"merged\":\"a\"}":               "{\"merged\":\"a\"}",
	}
	for raw, want := range cases {
		if got := extractJSONObject(raw); got != want {
			t.Errorf("extractJSONObject(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestPromptBlockListsKnownMemories(t *testing.T) {
	module := newTestModule(t, 7)
	record := addMemory(t, module, "用户偏好用中文回复")

	block := module.BuildPromptBlock(context.Background())
	if !strings.Contains(block, memoryRef(record)) {
		t.Fatalf("expected the memory marker in the prompt block, got:\n%s", block)
	}
	if !strings.Contains(block, "用户偏好用中文回复") {
		t.Fatalf("expected the memory text in the prompt block, got:\n%s", block)
	}
	if !strings.Contains(block, "MEMORY POLICY") {
		t.Fatalf("expected the recording policy in the prompt block, got:\n%s", block)
	}
}

func TestPromptBlockOnlyOffersMergeWhenConfigured(t *testing.T) {
	ctx := context.Background()

	plain := newTestModule(t, 7).BuildPromptBlock(ctx)
	if strings.Contains(plain, "memory_merge") {
		t.Fatalf("did not expect memory_merge without a merger, got:\n%s", plain)
	}

	merged := newTestModule(t, 7, WithMerger(MergerFunc(nil))).BuildPromptBlock(ctx)
	if !strings.Contains(merged, "memory_merge") {
		t.Fatalf("expected memory_merge once a merger is configured, got:\n%s", merged)
	}
	if !strings.Contains(merged, "memory_forget") || !strings.Contains(merged, "memory_write") {
		t.Fatalf("expected every registered tool in the routing policy, got:\n%s", merged)
	}
}

func TestPromptBlockAsksForCompressionOnceTheStoreIsLarge(t *testing.T) {
	const hint = 5
	module := newTestModule(t, 7, WithMerger(MergerFunc(nil)), WithCapacityHint(hint))
	ctx := context.Background()

	for i := 0; i < hint-1; i++ {
		addMemory(t, module, fmt.Sprintf("用户的第 %d 条背景信息", i))
	}
	if block := module.BuildPromptBlock(ctx); strings.Contains(block, "CAPACITY") {
		t.Fatalf("did not expect a capacity instruction below the hint, got:\n%s", block)
	}

	addMemory(t, module, "用户的最后一条背景信息")

	block := module.BuildPromptBlock(ctx)
	if !strings.Contains(block, "CAPACITY") {
		t.Fatalf("expected a capacity instruction once the store is full, got:\n%s", block)
	}
	if !strings.Contains(block, fmt.Sprintf("%d memories", hint)) {
		t.Fatalf("expected the count in the capacity instruction, got:\n%s", block)
	}
}

func TestPromptBlockStaysQuietBelowCapacity(t *testing.T) {
	module := newTestModule(t, 7, WithCapacityHint(10))
	addMemory(t, module, "用户偏好用中文回复")

	if block := module.BuildPromptBlock(context.Background()); strings.Contains(block, "CAPACITY") {
		t.Fatalf("did not expect a capacity instruction for a single memory, got:\n%s", block)
	}
}

// A failed load must not fail a run: the agent still gets the policy, just
// without the list of what it knows.
func TestPromptBlockDegradesToThePolicyWhenTheStoreIsUnavailable(t *testing.T) {
	module := NewWithDB(nil, 7)

	block := module.BuildPromptBlock(context.Background())
	if !strings.Contains(block, "MEMORY POLICY") {
		t.Fatalf("expected the policy to survive a broken store, got:\n%s", block)
	}
	if strings.Contains(block, "Known memories") {
		t.Fatalf("did not expect a memory list without a store, got:\n%s", block)
	}
}

// Ids must not reveal when or where a memory was written, and consecutive ids
// must not look alike.
func TestNewIDIsOpaqueAndUnrelatedToSiblings(t *testing.T) {
	seen := make(map[string]struct{}, 256)
	ids := make([]string, 0, 256)

	for i := 0; i < 256; i++ {
		id := newID()
		if len(id) != 16 {
			t.Fatalf("expected a 16 character id, got %q", id)
		}
		if strings.Trim(id, "0123456789abcdefghijklmnopqrstuv") != "" {
			t.Fatalf("expected %q to be lowercase base32hex", id)
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("generated %q twice", id)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	// Two ids sharing eight leading characters would be a 1-in-4-billion
	// accident, so it means the generator went back to encoding a timestamp.
	const prefixLength = 8
	for i := 1; i < len(ids); i++ {
		if ids[i-1][:prefixLength] == ids[i][:prefixLength] {
			t.Fatalf("neighbouring ids share a %d character prefix: %q and %q", prefixLength, ids[i-1], ids[i])
		}
	}
}

func TestMemoriesGetDistinctOpaqueIDs(t *testing.T) {
	module := newTestModule(t, 7)

	first := addMemory(t, module, "用户偏好用中文回复")
	second := addMemory(t, module, "用户希望回答简短")

	if first.ID == second.ID {
		t.Fatalf("expected distinct ids, got %q twice", first.ID)
	}
	if first.ID[:8] == second.ID[:8] {
		t.Fatalf("expected ids written in the same second to differ early, got %q and %q", first.ID, second.ID)
	}
}
