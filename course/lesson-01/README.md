# 第一课：Hello World Agent（Gin + SSE）

一个能跑的问答 agent，配套一个用来讲课的前端页面。整节课的目标就一句话：

> **只有 `finish` 成功返回，这一轮才结束。纯文本回答结束不了任务。**

## 跑起来

```bash
cd course/lesson-01

export LLM_TOKEN=sk-xxx                          # 必填
export LLM_MODEL=gpt-4o-mini                     # 可选，默认 gpt-4o-mini
export LLM_BASE_URL=https://api.openai.com/v1    # 可选，留空用库的默认端点

go run .
# 打开 http://localhost:8080
```

不想 export 也可以写进 `.env`（用 [godotenv](https://github.com/joho/godotenv) 加载）：

```bash
cp .env.example .env    # 然后编辑 .env，把 LLM_TOKEN 填上
go run .
```

`.env` 已经被 git 忽略，不会提交；`.env.example` 是提交的模板。加载规则是
**真实环境变量 > `.env`**（`godotenv.Load` 不覆盖已有的变量），所以本地用 `.env`、线上直接给环境变量，
两边互不干扰。`ENV_FILE` 可以指定别的文件名。

环境变量一览：

| 变量 | 默认 | 作用 |
| --- | --- | --- |
| `LLM_TOKEN` | 无 | 必填，模型服务的 token |
| `LLM_MODEL` | `gpt-4o-mini` | 模型名 |
| `LLM_BASE_URL` | 空 | base URL，留空用库的默认端点 |
| `LISTEN_ADDR` | `:8080` | 监听地址 |
| `MAX_CONTEXT_TOKENS` | 不设置 | 传给 `WithMaxContextTokens`，用来演示上下文压缩 |
| `ENV_FILE` | `.env` | 环境变量文件路径 |

不需要 API key 的验证方式：直接跑测试，它自带一个假的 OpenAI 服务。

```bash
go test ./... -v
```

## 页面上的三块东西

| 区域 | 讲什么 |
| --- | --- |
| 左边对话区 | 思考过程（灰色块）、模型输出、工具卡片、最后标着「交付答案」的那一块 |
| 右边第 1 栏 | 本轮走到哪一步：请求 → 模型思考 → 工具调用 → end tool 交付 |
| 右边第 2 栏 | 原始 SSE 事件（点「显示」打开），学员能直接看到协议长什么样 |

建议的课堂顺序：

1. 问「现在几点了？」——让学员看到 `now` 工具卡片，以及 `finish` 交付答案。
2. 打开「原始 SSE 事件」，逐帧对一遍协议。
3. 把 `agent.go` 里 `nowTool` 删掉或改个名字再问一次，看模型报错、继续试。
4. 改 `finishTool` 加一条交付约束，看它被打回重来。
5. 运行中点「停止」，看前端显示中断、以及服务端没有把半截的回合写进历史。

## 两个工具的分工

`agent.go` 里就两个工具，刚好覆盖这一课要讲的两类：

| 工具 | 类型 | 作用 |
| --- | --- | --- |
| `now` | 普通工具 | 干活：拿到当前时间。结果写回 transcript，循环继续 |
| `finish` | end tool | 交付：提交最终答案。它成功返回，run 结束 |

注意 `nowTool` 的返回值里有两个通道：`ModelContent` 给模型看，`Events` 只发给前端。
页面上的工具卡片就是 `Events` 画出来的——模型上下文里没有它。

## 接口与 SSE 协议

| 接口 | 作用 |
| --- | --- |
| `GET /` | 教学页面（`web/` 用 `go:embed` 打进二进制） |
| `GET /api/health` | 健康检查，返回当前模型名 |
| `POST /api/chat` | 提问，返回 SSE 流 |
| `POST /api/reset` | 清空某个会话的历史 |

`POST /api/chat` 的请求体是 `{"session_id": "...", "input": "..."}`，响应是 `text/event-stream`：

| event | data | 说明 |
| --- | --- | --- |
| `text` | `{kind, text, new_block, delivered?}` | `kind` 是 `reasoning` 或 `content`；`new_block` 表示另起一段；`delivered` 表示这就是 end tool 交付的答案 |
| `tool` | `{name, ok, args, result}` | 工具事件（来自 `ToolResult.Events`） |
| `usage` | `{prompt_tokens, completion_tokens, ...}` | 单次模型调用的用量 |
| `result` | `{answer, stopped, err, llm_calls, usage}` | 这一轮结束时的结果 |
| `error` | `{message}` | 调用方式有问题（比如 body 不合法） |

命令行验证（不需要浏览器）：

```bash
curl -N -X POST http://localhost:8080/api/chat \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"demo","input":"现在几点了？"}'
```

## 三个容易踩的点

这三条本身就是讲课素材：

1. **为什么不用 `EventSource`**：它只能发 GET，而我们要 POST 一个 JSON body。所以前端用 `fetch` + 手写解析（`web/app.js`，约 30 行）。
2. **SDK 给的是累积文本**：`reasoning` / `content` 消息带的是「到目前为止的全部文本」，而且每次模型调用都会从零重新累积。服务端用 `textStream` 换算成增量再发。
3. **SDK 的消息流没有结束事件**：channel 关闭就是结束。服务端在关闭后补一条 `result`，浏览器才知道这一轮的结果。

## 状态归谁

`server.go` 里的 `sessionStore` 是这块的教学样本：

- `BaseAgent` 一次只跑一个 run，所以**每轮请求都新建一个实例**；
- 历史由**我们自己**持有：`agent.History()` 存起来，下一轮 `WithHistory()` 灌回去；
- 只有正常结束的回合才写进历史——被中断或失败的回合丢掉，免得下一轮拿到半截 transcript。

## 文件

| 文件 | 内容 |
| --- | --- |
| `main.go` | 读环境变量、起 Gin |
| `server.go` | 路由、SSE、会话历史 |
| `agent.go` | 两个工具 + 每轮构造 agent |
| `web/` | 教学页面（`index.html` / `app.js` / `style.css`） |
| `main_test.go` | 假 OpenAI 服务的端到端测试，不需要 key |
| `.env.example` | 环境变量模板，`cp .env.example .env` 后即可使用 |

这个目录是**独立的 Go module**，所以 Gin 不会进 SDK 的依赖；同时它的 `go.mod` 里也演示了 SDK 要求的那两行 `replace`。
