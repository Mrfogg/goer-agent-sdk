# Examples

[简体中文](#中文说明) · [English](#english)

---

## English

Runnable programs that show how the SDK is used. Every example is a `main`
package inside the module, so `go build ./...` and `go vet ./...` cover them
too.

| Example | What it shows | Needs an API key |
| --- | --- | --- |
| [quickstart](quickstart) | The smallest agent: one regular tool, one end tool, one turn, printing `reasoning` / `content` and the final answer. | yes |
| [tools](tools) | A strict JSON schema (`OpenAIFunctionDefinition`), structured `ModelData` for the model, product events for the frontend (`ctxkey.ToolEventEmitter`), and validation inside the end tool. | yes |
| [multiturn](multiturn) | A terminal REPL where each turn builds a fresh agent seeded with `WithHistory`, then stores the returned transcript. | yes |
| [streaming](streaming) | Thinking and answer text rendered separately, plus cancellation: Ctrl+C or the deadline ends the run with `Result().Stopped`. | yes |
| [httpapi](httpapi) | An HTTP handler that streams newline-delimited JSON to the client, one agent per request, cancellation on client disconnect. | yes |
| [localmock](localmock) | A complete run against a local mock of the OpenAI endpoint — **no credentials, no network**. Demonstrates both output modes and the corrective retry after a text-only reply. | no |

Run one against a real model:

```bash
export LLM_TOKEN=sk-...
export LLM_BASE_URL=https://api.openai.com/v1
export LLM_MODEL=gpt-4o            # optional, defaults to gpt-4o

go run ./examples/quickstart
```

Or run the offline demo, which needs nothing at all:

```bash
go run ./examples/localmock                     # streaming output (default)
MODE=non_streaming go run ./examples/localmock  # non-streaming output
```

The HTTP example listens on `:8080`:

```bash
go run ./examples/httpapi
curl -N -X POST localhost:8080/ask -d '{"input":"How many rows does this sheet have?"}'
```

Each example keeps its tools inline so the file can be copied as a starting
point; production code usually puts tools in their own packages.

---

## 中文说明

这里的每个程序都是可以直接运行的示例。它们都是模块内的 `main` 包，因此
`go build ./...` 和 `go vet ./...` 也会一起编译检查。

| 示例 | 演示内容 | 需要 API Key |
| --- | --- | --- |
| [quickstart](quickstart) | 最小可用 agent：一个普通工具 + 一个 end tool + 跑一轮，打印 `reasoning` / `content` 与最终答案。 | 需要 |
| [tools](tools) | 严格的 JSON Schema（`OpenAIFunctionDefinition`）、给模型的结构化 `ModelData`、给前端的产品事件（`ctxkey.ToolEventEmitter`），以及在 end tool 里做校验。 | 需要 |
| [multiturn](multiturn) | 终端 REPL：每轮都用 `WithHistory` 注入上一轮历史新建 agent，结束后把 transcript 存回来。 | 需要 |
| [streaming](streaming) | 思考内容与回答正文分开渲染；按 Ctrl+C 或超时取消 run，`Result().Stopped` 为 true。 | 需要 |
| [httpapi](httpapi) | HTTP handler 用 NDJSON 把消息流式写给客户端；每请求一个 agent 实例；客户端断开即取消 run。 | 需要 |
| [localmock](localmock) | 对着本地 mock 的 OpenAI 端点跑完整一轮——**不需要密钥、不需要网络**，同时演示两种输出模式和「纯文本回答被退回重试」。 | 不需要 |

对着真实模型运行：

```bash
export LLM_TOKEN=sk-...
export LLM_BASE_URL=https://api.openai.com/v1
export LLM_MODEL=gpt-4o            # 可选，默认 gpt-4o

go run ./examples/quickstart
```

完全离线跑一遍：

```bash
go run ./examples/localmock
```

示例里的工具都写在同一个文件里，方便直接复制成起点；真实项目里通常把工具拆成独立包。
