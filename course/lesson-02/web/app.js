// 第二课的前端：在第一课的对话页面上加了两块和记忆有关的东西。
//
// 1. 对话区里的「记忆卡片」：来自 SSE 的 memory 事件（工具的产品侧通道）；
// 2. 右侧的「记忆库」：来自 GET /api/memories，直接读 SQLite 里现在有什么。
//
// 两块合起来才能讲清这一课：卡片说明「模型什么时候动了记忆」，记忆库说明
// 「对话之外到底存着什么」。

const els = {
  messages: document.getElementById("messages"),
  form: document.getElementById("composer"),
  input: document.getElementById("input"),
  send: document.getElementById("send"),
  stop: document.getElementById("stop"),
  reset: document.getElementById("reset"),
  clearMemories: document.getElementById("clear-memories"),
  memoryCount: document.getElementById("memory-count"),
  memoryDB: document.getElementById("memory-db"),
  memories: document.getElementById("memories"),
  raw: document.getElementById("raw"),
  toggleRaw: document.getElementById("toggle-raw"),
  steps: document.getElementById("steps"),
  showReasoning: document.getElementById("show-reasoning"),
};

const state = {
  sessionId: readSessionId(),
  controller: null,
  turn: null,
};

// 模型对记忆做的动作，对应 memorykit 发出来的 action 字段。
// 前四个是「真的改动了记忆库」，最后一个是「模型想动，但被服务端拦下来了」。
const MEMORY_ACTIONS = {
  saved: "记住了一条",
  rewritten: "改写了记忆",
  merged: "合并了记忆",
  deleted: "删掉了一条记忆",
  rejected: "拒绝了一条记忆操作",
};

function readSessionId() {
  let id = localStorage.getItem("lesson02.session");
  if (!id) {
    id = (crypto.randomUUID && crypto.randomUUID()) || `s-${Date.now()}`;
    localStorage.setItem("lesson02.session", id);
  }
  return id;
}

/* ---------------- 右侧讲解面板 ---------------- */

function setStep(name, status) {
  const li = els.steps.querySelector(`[data-step="${name}"]`);
  if (!li) return;
  li.classList.toggle("active", status === "active");
  li.classList.toggle("done", status === "done");
}

function resetSteps() {
  els.steps.querySelectorAll("li").forEach((li) => li.classList.remove("active", "done"));
}

function logRaw(title, data) {
  if (els.raw.hidden) return;
  const text = typeof data === "string" ? data : JSON.stringify(data);
  els.raw.textContent += `${title}\n  ${text}\n`;
  els.raw.scrollTop = els.raw.scrollHeight;
}

/* ---------------- 记忆库 ---------------- */

async function loadMemories() {
  let payload;
  try {
    const response = await fetch("/api/memories");
    payload = await response.json();
  } catch (err) {
    els.memories.innerHTML = `<div class="mem-empty">读取失败：${err.message}</div>`;
    return;
  }

  const list = payload.memories || [];
  els.memoryCount.textContent = `${list.length} 条`;

  if (!list.length) {
    els.memories.innerHTML = '<div class="mem-empty">还没有任何记忆</div>';
    return;
  }

  els.memories.innerHTML = "";
  for (const memory of list) {
    const item = document.createElement("div");
    item.className = "memory-item";

    const id = document.createElement("span");
    id.className = "memory-id";
    id.textContent = `#${memory.id}`;

    const content = document.createElement("div");
    content.className = "memory-content";
    content.textContent = memory.content;

    item.append(id, content);
    els.memories.appendChild(item);
  }
}

/* ---------------- 对话区渲染 ---------------- */

function scrollToBottom() {
  els.messages.scrollTop = els.messages.scrollHeight;
}

function clearEmptyState() {
  const empty = els.messages.querySelector(".empty");
  if (empty) empty.remove();
}

function addUserBubble(text) {
  clearEmptyState();
  const wrap = document.createElement("div");
  wrap.className = "turn user";
  const bubble = document.createElement("div");
  bubble.className = "bubble";
  bubble.textContent = text;
  wrap.appendChild(bubble);
  els.messages.appendChild(wrap);
  scrollToBottom();
}

function startTurn() {
  clearEmptyState();
  const el = document.createElement("div");
  el.className = "turn assistant";
  const label = document.createElement("div");
  label.className = "label";
  label.textContent = "assistant";
  el.appendChild(label);
  els.messages.appendChild(el);

  const turn = { el, open: { reasoning: null, content: null }, done: false };
  state.turn = turn;
  return turn;
}

// context 事件：本轮请求带上了多少历史、多少记忆。
// 这是整节课的对照物，所以直接钉在对话区最上面。
function appendContextBanner(turn, data) {
  const banner = document.createElement("div");
  banner.className = "context-banner";

  const history = document.createElement("span");
  history.className = "ctx-item";
  history.innerHTML = `历史 <b>${data.history_messages || 0}</b> 条`;

  const memories = document.createElement("span");
  memories.className = "ctx-item";
  memories.innerHTML = `记忆 <b>${data.memories || 0}</b> 条`;

  const note = document.createElement("span");
  note.className = "ctx-note";
  note.textContent = "一起拼进 system prompt";

  banner.append(history, memories, note);
  turn.el.appendChild(banner);
  scrollToBottom();
}

// appendText 把增量接到对应的块上；new_block 表示这是新的一次回复，要另起一块；
// delivered 表示这一段就是 end tool 交付的答案。
function appendText(turn, kind, text, newBlock, delivered) {
  if (!text) return null;

  let block = newBlock ? null : turn.open[kind];
  if (!block) {
    block = document.createElement("div");
    block.className = `block ${kind}`;
    if (kind === "reasoning") block.classList.toggle("hidden", !els.showReasoning.checked);

    const label = document.createElement("div");
    label.className = "block-label";
    label.textContent = kind === "reasoning" ? "思考过程" : "模型输出";
    block.appendChild(label);

    const body = document.createElement("div");
    body.className = "block-body";
    block.appendChild(body);

    turn.el.appendChild(block);
  }

  // 用 textContent 累加：模型输出永远当纯文本处理，不解析成 HTML。
  block.querySelector(".block-body").textContent += text;
  turn.open[kind] = block;
  if (delivered) markDeliveredBlock(block);
  scrollToBottom();
  return block;
}

function markDeliveredBlock(block) {
  if (!block || block.classList.contains("delivered")) return;
  block.classList.add("delivered");

  const tag = document.createElement("span");
  tag.className = "delivered-tag";
  tag.textContent = "交付答案 · end tool 的 ModelContent";
  block.querySelector(".block-label").appendChild(tag);
}

function appendTool(turn, data) {
  const ok = data.ok !== false;

  const card = document.createElement("div");
  card.className = `tool${ok ? "" : " fail"}`;

  const head = document.createElement("div");
  head.className = "tool-head";
  const name = document.createElement("span");
  name.className = "tool-name";
  name.textContent = data.name || "tool";
  const stateEl = document.createElement("span");
  stateEl.className = "tool-state";
  stateEl.textContent = ok ? "成功" : "失败（run 继续）";
  head.append(name, stateEl);
  card.appendChild(head);

  const body = document.createElement("div");
  body.className = "tool-body";
  appendToolLine(body, "参数", JSON.stringify(data.args || {}));
  appendToolLine(body, "结果", String(data.result ?? ""));
  card.appendChild(body);

  turn.el.appendChild(card);

  // 工具卡片之后模型会继续说话，下一段文本要另起一块。
  turn.open.reasoning = null;
  turn.open.content = null;
  scrollToBottom();
}

// 记忆卡片：工具的产品侧事件。模型看不到这上面的内容，前端看得到。
//
// 成功和失败走的是同一条通道：被拒的动作也画一张卡，否则「模型想删但服务端
// 没让」这件事在前端就是一片空白。
function appendMemoryCard(turn, data) {
  const action = MEMORY_ACTIONS[data.action] || data.action || "记忆变更";

  const card = document.createElement("div");
  card.className = `memory-card ${data.action || ""}`;

  const head = document.createElement("div");
  head.className = "memory-head";
  const title = document.createElement("span");
  title.className = "memory-action";
  title.textContent = action;
  head.appendChild(title);
  if (data.memory_id) {
    const id = document.createElement("span");
    id.className = "memory-id";
    id.textContent = `#${data.memory_id}`;
    head.appendChild(id);
  }
  card.appendChild(head);

  if (data.content) {
    const content = document.createElement("div");
    content.className = "memory-content";
    content.textContent = data.content;
    card.appendChild(content);
  }

  // 改写：旧的那条已经被覆盖掉了，标出来。
  if (data.replaced) {
    const replaced = document.createElement("div");
    replaced.className = "memory-replaced";
    replaced.textContent = `替换了：${data.replaced}`;
    card.appendChild(replaced);
  }

  // 合并成功：列出被折掉的那几条。
  if (Array.isArray(data.merged_from) && data.merged_from.length) {
    const folded = document.createElement("div");
    folded.className = "memory-folded";
    folded.textContent = `折掉了 ${data.merged_from.length} 条：${data.merged_from.map((x) => `#${x}`).join("、")}`;
    card.appendChild(folded);
  } else if (Array.isArray(data.memory_ids) && data.memory_ids.length) {
    const involved = document.createElement("div");
    involved.className = "memory-folded";
    involved.textContent = `涉及 ${data.memory_ids.length} 条：${data.memory_ids.map((x) => `#${x}`).join("、")}`;
    card.appendChild(involved);
  }

  // 拒绝：服务端把理由同时给了模型和前端，这里原样显示。
  if (data.error) {
    const error = document.createElement("div");
    error.className = "memory-error";
    error.textContent = `未生效：${data.error}`;
    card.appendChild(error);
  }

  turn.el.appendChild(card);

  // 记忆变了，右边的记忆库要跟着变；被拒的那次没变，拉了也是同一份。
  loadMemories();

  turn.open.reasoning = null;
  turn.open.content = null;
  scrollToBottom();
}

function appendToolLine(parent, key, value) {
  const line = document.createElement("div");
  line.className = "tool-line";
  const k = document.createElement("span");
  k.className = "k";
  k.textContent = key;
  const v = document.createElement("span");
  v.className = "v";
  v.textContent = value;
  line.append(k, v);
  parent.appendChild(line);
}

function finishTurn(turn, result) {
  if (!turn || turn.done) return;
  turn.done = true;

  const status = document.createElement("div");
  status.className = "turn-status";

  if (result.stopped) {
    status.classList.add("error");
    status.textContent = "已中断：服务端这一轮 run 被取消（Result().Stopped = true），半截的回合不写进历史。";
  } else if (result.err) {
    status.classList.add("error");
    status.textContent = `失败：${result.err}`;
  } else {
    const usage = result.usage || {};
    status.textContent =
      `完成 · 模型调用 ${result.llm_calls || 0} 次 · ` +
      `tokens 输入 ${usage.prompt_tokens || 0} / 输出 ${usage.completion_tokens || 0}`;

    // 正常路径上「交付答案」已经在 text 帧里标记过了；这里只是兜底：
    // 万一一个 content 块都没有（例如答案没有经过流式输出），补一块出来。
    if (!turn.el.querySelector(".block.content.delivered") && result.answer) {
      markDeliveredBlock(appendText(turn, "content", result.answer, true));
    }
    setStep("model", "done");
    setStep("deliver", "done");
  }

  turn.el.appendChild(status);
  scrollToBottom();
}

/* ---------------- SSE ---------------- */

function parseFrame(frame) {
  let name = "message";
  const dataLines = [];

  for (const line of frame.split("\n")) {
    if (line.startsWith("event:")) name = line.slice(6).trim();
    else if (line.startsWith("data:")) dataLines.push(line.slice(5).trim());
  }
  if (!dataLines.length) return null;

  const raw = dataLines.join("\n");
  let data = raw;
  try {
    data = JSON.parse(raw);
  } catch (_) {
    // 不是 JSON 就按原样传下去
  }
  return { name, data };
}

async function readSSE(response, onEvent) {
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";

  for (; ;) {
    const { value, done } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });

    // 一个 SSE 帧以空行结束；一个网络包可能含多帧，也可能只有半帧。
    let index;
    while ((index = buffer.indexOf("\n\n")) !== -1) {
      const frame = buffer.slice(0, index);
      buffer = buffer.slice(index + 2);
      const event = parseFrame(frame);
      if (event) onEvent(event.name, event.data);
    }
  }
}

function handleEvent(name, data, turn) {
  logRaw(`← ${name}`, data);

  switch (name) {
    case "context":
      appendContextBanner(turn, data);
      setStep("request", "done");
      setStep("model", "active");
      break;
    case "text":
      if (data.new_block) setStep("model", "active");
      appendText(turn, data.kind, data.text, data.new_block, data.delivered);
      break;
    case "memory":
      appendMemoryCard(turn, data);
      setStep("tool", "done");
      setStep("model", "active"); // 工具跑完，模型会接着说话
      break;
    case "tool":
      appendTool(turn, data);
      setStep("tool", "done");
      setStep("model", "active");
      break;
    case "usage":
      break; // 用量在 result 里汇总展示
    case "result":
      finishTurn(turn, data);
      break;
    case "error":
      finishTurn(turn, { err: data.message });
      break;
  }
}

/* ---------------- 交互 ---------------- */

function setBusy(busy) {
  els.send.disabled = busy;
  els.stop.hidden = !busy;
  els.input.disabled = busy;
}

async function send(input) {
  const turn = startTurn();
  resetSteps();
  setStep("request", "active");

  state.controller = new AbortController();
  setBusy(true);
  logRaw("→ POST /api/chat", { session_id: state.sessionId, input });

  try {
    const response = await fetch("/api/chat", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ session_id: state.sessionId, input }),
      signal: state.controller.signal,
    });

    if (!response.ok) {
      throw new Error(`HTTP ${response.status} · ${await response.text()}`);
    }

    setStep("request", "done");
    setStep("model", "active");
    await readSSE(response, (name, data) => handleEvent(name, data, turn));
  } catch (err) {
    if (err && err.name === "AbortError") {
      finishTurn(turn, { stopped: true });
    } else {
      finishTurn(turn, { err: (err && err.message) || String(err) });
    }
  } finally {
    setBusy(false);
    state.controller = null;
    loadMemories();
  }
}

els.form.addEventListener("submit", (event) => {
  event.preventDefault();
  const input = els.input.value.trim();
  if (!input || state.controller) return;
  els.input.value = "";
  els.input.style.height = "auto";
  addUserBubble(input);
  send(input);
});

els.input.addEventListener("keydown", (event) => {
  if (event.key === "Enter" && !event.shiftKey) {
    event.preventDefault();
    els.form.requestSubmit();
  }
});

els.input.addEventListener("input", () => {
  els.input.style.height = "auto";
  els.input.style.height = `${Math.min(els.input.scrollHeight, 140)}px`;
});

els.stop.addEventListener("click", () => {
  // 断开连接 → 服务端请求的 context 被取消 → run 结束（Result().Stopped 为 true）。
  if (state.controller) state.controller.abort();
});

els.showReasoning.addEventListener("change", () => {
  const hidden = !els.showReasoning.checked;
  els.messages.querySelectorAll(".block.reasoning").forEach((block) => {
    block.classList.toggle("hidden", hidden);
  });
});

els.toggleRaw.addEventListener("click", () => {
  els.raw.hidden = !els.raw.hidden;
  els.toggleRaw.textContent = els.raw.hidden ? "显示" : "隐藏";
  if (!els.raw.hidden) els.raw.scrollTop = els.raw.scrollHeight;
});

// 「新会话」只清历史。记忆库那一栏在清完之后会重新拉一次——条数不变，
// 这就是这一课最想让人看到的一眼。
els.reset.addEventListener("click", async () => {
  if (state.controller) state.controller.abort();

  await fetch("/api/reset", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ session_id: state.sessionId }),
  }).catch(() => { });

  localStorage.removeItem("lesson02.session");
  state.sessionId = readSessionId();
  state.turn = null;

  els.messages.innerHTML =
    '<div class="empty"><h2>新会话已开始（记忆保留）</h2>' +
    "<p>服务端的历史已经清空，记忆库一条没少。现在问「我是做什么的？」，下一轮开头的 <code>context</code> 事件里历史是 0，但它照样答得出来。</p></div>";
  els.raw.textContent = "";
  resetSteps();
  loadMemories();
});

els.clearMemories.addEventListener("click", async () => {
  await fetch("/api/memories/clear", { method: "POST" }).catch(() => { });
  loadMemories();
});

// 带 ?q= 打开时自动问一次，课堂演示可以直接甩一个链接：
//   http://localhost:8080/?q=我是做财务分析的，以后回答都用中文
const preset = new URLSearchParams(location.search).get("q");
if (preset && preset.trim()) {
  const question = preset.trim();
  addUserBubble(question);
  send(question);
}

fetch("/api/health")
  .then((response) => response.json())
  .then((health) => {
    if (health.memory_db) els.memoryDB.textContent = health.memory_db;
  })
  .catch(() => { });

loadMemories();
