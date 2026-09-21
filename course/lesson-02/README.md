# 第二课：让 Agent 记住你（记忆模块）

在第一课的问答 agent 上加记忆。整节课的目标也是一句话：

> **记忆不在对话里。清空历史之后它还记得你，因为记忆存在对话之外。**

跑完之后你会看到：模型自己决定记什么，`memory_write` / `memory_merge` / `memory_forget` 是它唯一的写入口，
而删不删、能不能删的最终裁决在服务端。

## 跑起来

```bash
cd course/lesson-02

export LLM_TOKEN=sk-xxx                          # 必填
export LLM_MODEL=gpt-4o-mini                     # 可选，默认 gpt-4o-mini
export LLM_BASE_URL=https://api.openai.com/v1    # 可选，留空用库的默认端点
export MERGE_MODEL=gpt-4o-mini                   # 可选，合并记忆用哪个模型（默认同 LLM_MODEL）

go run .
# 打开 http://localhost:8080
```

`MEMORY_DB` 可以换记忆库文件的位置（默认当前目录的 `memories.db`，SQLite）。
也可以写进 `.env`：

```bash
cp .env.example .env    # 然后编辑 .env，把 LLM_TOKEN 填上
go run .
```

环境变量一览：

| 变量 | 默认 | 作用 |
| --- | --- | --- |
| `LLM_TOKEN` | 无 | 必填，模型服务的 token |
| `LLM_MODEL` | `gpt-4o-mini` | 主流程模型名 |
| `LLM_BASE_URL` | 空 | base URL，留空用库的默认端点 |
| `MERGE_MODEL` | 同 `LLM_MODEL` | 合并记忆那一次调用的模型 |
| `MEMORY_DB` | `memories.db` | 记忆库文件（SQLite） |
| `LISTEN_ADDR` | `:8080` | 监听地址 |
| `MAX_CONTEXT_TOKENS` | 不设置 | 传给 `WithMaxContextTokens` |
| `ENV_FILE` | `.env` | 环境变量文件路径 |

不需要 API key 的验证方式：跑测试，它自带一个假的模型服务（流式和非流式都认）。

```bash
go test ./... -v
```

## 记忆是怎么接进来的

`agent.go` 里只有一行：

```go
agent.WithMemory(memory)
```

这一行做两件事：

1. 把记忆工具注册给模型（`memory_write`、`memory_merge`、`memory_forget`）；
2. 每轮 run 开始时调用模块的 `BuildPromptBlock`，把提示词段落拼到 system prompt 后面。

所以 `systemPrompt` 里**一个字都没提记忆策略**——策略、工具路由、「已知记忆清单」都在模块里。
要改记忆的行为，去改模块，不是在这一课再写一遍提示词。

`memory`（模块）由 `newRouter` 建一次，每轮请求复用；agent 才是每轮新建的。
两个生命周期不同，这就是「清空历史之后记忆还在」的实现层原因。

存储用的是 SDK 自带的 `memorykit`（`../../memorykit`）：gorm + 纯 Go 的 SQLite 驱动，
一个模块绑定一个用户（`demoUserID`），记忆落在 `MEMORY_DB` 指定的文件里，进程重启还在。

## 页面上的三块东西

| 区域 | 讲什么 |
| --- | --- |
| 左边对话区 | 每轮开头的 `context` 条（历史 / 记忆各几条）、思考过程、模型输出、**记忆卡片**、`finish` 交付的那一块 |
| 右上「记忆库」 | 直接读 `GET /api/memories`：现在库里到底有什么、几条 |
| 右下 | 本轮走到哪一步 + 原始 SSE 事件 |

顺带一提：记忆卡片不是模型上下文的一部分。工具通过 `ToolResult.Events` 把 `memory` 事件发给产品侧，
模型永远看不到——所以卡片上能写的（比如「替换了：……」）比模型知道的还多。

建议的课堂顺序：

1. 说「我是做财务分析的，以后回答都用中文，尽量简短」。看模型调 `memory_write`，卡片出现，记忆库从 0 变 1。
2. 点右上角**「新会话（保留记忆）」**，再问「我是做什么的？」。下一轮开头的 `context` 报「历史 0 / 记忆 1」，
   但它答得出来——`system prompt` 里有记忆清单。
3. 刷新页面，只问一句「我是做什么的？」。进程还在、库还在，答案一样（记忆不依赖任何一次对话）。
4. 说一句「把刚才那条忘掉」。看 `memory_forget` 被服务端拒绝的卡片——模型引用的不是你的原话，删不掉；
   换成你真正说过的说法再试一次，才删得掉。
5. 故意让它记一条密码，看 `memory_write` 被拒（敏感信息不落库）。
6. 打开「原始 SSE 事件」对一遍：`context` → `memory` → `text` → `result`。

## 记忆工具的分工

| 工具 | 什么时候调 | 谁说了算 |
| --- | --- | --- |
| `memory_write` | 记一条新事实；带 `memory_id` 则是改写已有的那条 | 模型自己判断「以后还用得上吗」 |
| `memory_merge` | 两条以上记忆重叠/重复，或库太大要压缩 | 模型只挑 `memory_ids`，合并文本由**注入的合并模型**写 |
| `memory_forget` | 用户明确说要忘掉某条 | 模型必须引用用户原话，服务端核对通过才删 |
| `finish` | 交付答案（end tool） | 只有它成功返回，这一轮才结束 |

两个细节值得展开：

- **`memory_merge` 不是白送的**。它需要一次额外的模型调用，所以只有在宿主注入了 `Merger` 时才注册
  （`memorykit.WithMerger`）。没注入时，工具不下发、提示词也不提，模型拿不到一个注定失败的动作。
  这一课在 `server.go` 里自己建 client 注入，`MERGE_MODEL` 还能换一个更便宜的模型。
- **合并是原子的**。合并结果由模块校验（不能比来源加起来还长、不能没有事实清单），
  然后「删来源 + 建新条」在一个事务里完成。任一步失败，来源一条都不能少——
  测试 `TestFailedMergeLeavesTheMemoriesAlone` 盯的就是这件事。

## 接口与 SSE 协议

| 接口 | 作用 |
| --- | --- |
| `GET /` | 教学页面（`web/` 用 `go:embed` 打进二进制） |
| `GET /api/health` | 健康检查，返回模型名和记忆库路径 |
| `POST /api/chat` | 提问，返回 SSE 流 |
| `POST /api/reset` | **只清某个会话的历史**，不动记忆（返回 `memory_kept: true`） |
| `GET /api/memories` | 记忆库当前内容 |
| `POST /api/memories/clear` | 清空记忆库（产品侧自己的删除，不经过模型） |

`POST /api/chat` 的请求体是 `{"session_id": "...", "input": "..."}`，响应是 `text/event-stream`：

| event | data | 说明 |
| --- | --- | --- |
| `context` | `{history_messages, memories}` | 本轮带上了多少历史、多少记忆。整节课的对照物 |
| `text` | `{kind, text, new_block, delivered?}` | `kind` 是 `reasoning` 或 `content`；`delivered` 表示这是 end tool 交付的答案 |
| `tool` | `{name, ok, args, result}` | 工具事件（来自 `ToolResult.Events`） |
| `memory` | `{action, memory_id?, content?, ...}` | 记忆变更，见下表 |
| `usage` | `{prompt_tokens, completion_tokens, ...}` | 单次模型调用的用量 |
| `result` | `{answer, stopped, err, llm_calls, usage}` | 这一轮结束时的结果 |
| `error` | `{message}` | 调用方式有问题（比如 body 不合法） |

`memory` 事件的 `action` 取值：

| action | 附带字段 | 含义 |
| --- | --- | --- |
| `saved` | `memory_id`, `content` | 新记了一条 |
| `rewritten` | `memory_id`, `content`, `replaced` | 改写了已有的那条，`replaced` 是被覆盖掉的旧文本 |
| `merged` | `memory_id`, `content`, `merged_from`, `folded` | 折掉了 `merged_from` 里的几条 |
| `deleted` | `memory_id`, `content` | 删掉了一条 |
| `rejected` | `error`（+ 相关 id） | **模型想动手，被服务端拦下来了**，`error` 是给模型和前端看的同一个理由 |

命令行验证（不需要浏览器）：

```bash
curl -N -X POST http://localhost:8080/api/chat \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"demo","input":"我是做财务分析的，以后回答都用中文"}'

curl -s http://localhost:8080/api/memories
curl -s -X POST http://localhost:8080/api/reset -H 'Content-Type: application/json' -d '{"session_id":"demo"}'
```

## 四个容易踩的点

1. **历史 ≠ 记忆，别把两件事混在一起设计**。历史是「这次对话说过什么」，随会话走；
   记忆是「这个人是谁」，跨会话、跨设备。`/api/reset` 只清前者，`/api/memories/clear` 才动后者。
2. **不调用就没记住**。`memory_write` 是唯一的写入口，模型没调它，用户说的话下一轮就不存在。
   想让 agent 更主动地记，改的是模块的记录策略（`memorykit/prompt.go`），不是在这里加提示词。
3. **删除的权限边界在服务端，不在提示词**。`memory_forget` 要求 `user_quote`，
   服务端拿最近 20 条用户消息核对（忽略空格、标点、大小写）；对不上就拒。提示词可以被绕过，代码不行。
4. **删除和合并都会毁掉旧文本**，所以结果里必须回显「没了什么」：
   改写回显 `replaced`，合并回显 `folded`，删除回显 `content`。这些都是产品侧事件，模型看不到。

## 状态归谁

| 状态 | 谁持有 | 生命周期 |
| --- | --- | --- |
| 本轮 run 的中间状态 | `BaseAgent`（每轮新建） | 一次 run |
| 对话历史 | `sessionStore`（`server.go`） | 一次会话，`/api/reset` 清掉 |
| 记忆 | `memorykit.Module`（`newRouter` 建一次） | 一个用户，跨会话、跨进程 |

只有正常结束的回合才写进历史——被中断或失败的回合丢掉，和第一课一样。

## 文件

| 文件 | 内容 |
| --- | --- |
| `main.go` | 读环境变量、起 Gin |
| `server.go` | 路由、SSE、会话历史、`/api/memories` |
| `agent.go` | `finish` 工具 + 每轮构造 agent（`WithMemory` 就在这里） |
| `web/` | 教学页面（对话区 + 记忆库面板 + 步骤 + 原始 SSE） |
| `main_test.go` | 假模型服务的端到端测试：跨会话、没调用就没记住、删除校验、合并回滚 |
| `.env.example` | 环境变量模板，`cp .env.example .env` 后即可使用 |

这个目录是**独立的 Go module**，`go.mod` 里那两行 `replace`（SDK 自己 + go-openai 的 fork）
是每个使用方都要写的。
