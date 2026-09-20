/* 变更准备工作台（由 ChangeGuard 服务同源提供）。
 *
 * 两条与部署形态相关的硬约束：
 *  1. **身份不由本页面声明。** 页面不发送任何身份头，组织范围由 Go 服务从
 *     已认证会话解析后注入给下游 Agent 服务。页面只显示"我是谁"，不决定"我是谁"。
 *  2. **写请求必须带 CSRF 令牌**，与治理后端其他接口一致。
 *
 * 另有一条贯穿全文的表述约束：**不能让"生成完成"看起来像"变更通过"。**
 */
"use strict";

const TERMINAL = new Set(["DRAFT_READY", "CHECK_BLOCKED", "INPUT_REJECTED", "FAILED", "CANCELLED"]);
const RESUMABLE = new Set(["NEEDS_INFO", "FAILED", "CHECK_BLOCKED"]);
const ACTIVE = new Set(["RECEIVED", "RUNNING"]);
const POLL_MS = 1200;

const STATUS_META = {
  RECEIVED: { label: "已接收 · 排队中", tone: "info" },
  NEEDS_INFO: { label: "需要补充信息", tone: "warn" },
  RUNNING: { label: "生成中", tone: "info" },
  // "待人工确认"必须和"排队中/生成中"在视觉上区分开：它是本轮唯一需要人做决定的时刻。
  DRAFT_READY: { label: "草案已生成 · 待人工确认", tone: "confirm" },
  CHECK_BLOCKED: { label: "确定性检查未通过或未完成 · 已停止", tone: "danger" },
  INPUT_REJECTED: { label: "输入被拒绝", tone: "danger" },
  FAILED: { label: "失败", tone: "danger" },
  CANCELLED: { label: "已取消", tone: "muted" },
};

const CHECK_META = {
  NOT_RUN: { label: "未运行", badge: "badge-muted", note: "没有确定性检查结论，不能据此判断是否可以继续。" },
  PASSED: { label: "静态检查通过", badge: "badge-ok", note: "仅为本地静态扫描的结论，不代表审批通过，也不代表可在生产执行。" },
  BLOCKED: { label: "存在阻断项", badge: "badge-danger", note: "存在阻断级问题，需先处理再重新检查。" },
  FAILED: { label: "检查失败 —— 不得视为通过", badge: "badge-danger", note: "扫描未成功完成，结论不可用。失败不等于没有问题。" },
};

// 停止原因用可读文案展示：它回答"为什么停下"，不该只留一个枚举值。
const STOP_REASON_LABELS = {
  EVIDENCE_SUFFICIENT: "证据足够",
  INSUFFICIENT_EVIDENCE: "证据不足",
  ROUNDS_EXHAUSTED: "轮次用尽",
  TOOL_CALLS_EXHAUSTED: "工具调用用尽",
  NO_PROGRESS: "无进展（重复调用）",
  PLANNER_UNAVAILABLE: "决策者不可用",
  PLANNER_FAILED: "决策者未能给出可执行动作",
  TOOL_FAILED: "工具失败",
};

const TOOL_KIND_LABELS = {
  search: "检索",
  material: "材料",
  timeout: "超时",
  generic: "其他",
};

const CLARIFY_FIELDS = [
  { name: "application", label: "应用", type: "text", placeholder: "order-service" },
  { name: "environment", label: "环境", type: "text", placeholder: "生产" },
  {
    name: "database", label: "数据库", type: "select",
    options: [["", "未知"], ["postgresql", "PostgreSQL"], ["mysql", "MySQL"]],
  },
  { name: "table", label: "表名", type: "text", placeholder: "orders" },
  { name: "query_sql", label: "触发查询 SQL", type: "textarea", placeholder: "慢查询原文" },
  { name: "planned_at", label: "计划时间", type: "datetime-local" },
  { name: "planned_at_timezone", label: "时区", type: "text", placeholder: "Asia/Shanghai" },
  {
    name: "schema_snapshot", label: "表结构快照", type: "textarea",
    placeholder: "粘贴字段列表或 CREATE TABLE。只使用导入的快照，不接生产库实时探索。",
  },
];

/** 与后端 `CreateTaskRequest.requirement` 的 max_length 保持一致。
 *  前端先拦一次：省一次往返，也避免把超长内容发出去。 */
const REQUIREMENT_MAX_CHARS = 4000;

const state = {
  authStatus: null,
  session: null,
  agentEnabled: true,
  health: null,
  task: null,
  timer: null,
  busy: false,
  editing: false,
  edits: { sql: null, rollback: null },
};

const $ = (id) => document.getElementById(id);

/* ---------- 基础工具 ---------- */

/**
 * 把服务端的错误体压成一句可读文本。
 *
 * 校验错误（FastAPI 是**数组**）以前被整体 `JSON.stringify` 后摊在错误横幅里：
 * 用户看到的是一大坨 JSON，而且里面带着后端回显的提交内容。这里只取可读的那部分，
 * 取不到才退回原始文本。
 */
function readableError(detail, fallback) {
  if (typeof detail === "string" && detail.trim()) return detail.trim();
  if (Array.isArray(detail)) {
    const parts = detail
      .map((item) => (item && typeof item.msg === "string" ? item.msg.trim() : ""))
      .filter(Boolean);
    if (parts.length) return parts.join("；");
  } else if (detail && typeof detail === "object" && typeof detail.msg === "string" && detail.msg.trim()) {
    return detail.msg.trim();
  }
  return fallback;
}

function esc(value) {
  return String(value === null || value === undefined ? "" : value)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

function formatTime(iso) {
  if (!iso) return "--:--:--";
  const parsed = new Date(iso);
  if (Number.isNaN(parsed.getTime())) return String(iso);
  return parsed.toLocaleTimeString("zh-CN", { hour12: false });
}

function formatDate(value) {
  if (!value) return "未提供";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return String(value);
  return parsed.toLocaleString("zh-CN", { hour12: false });
}

function badgeClass(tone) {
  if (tone === "ok") return "ok";
  if (tone === "warn") return "warn";
  if (tone === "danger") return "danger";
  if (tone === "confirm") return "confirm";
  if (tone === "info" || tone === "neutral") return "info";
  return "muted";
}

function statusBadge(status) {
  const meta = STATUS_META[status] || { label: status, tone: "muted" };
  return `<span class="badge badge-${badgeClass(meta.tone)}">${esc(meta.label)}</span>`;
}

async function copyText(text, button) {
  try {
    await navigator.clipboard.writeText(text);
    if (button) {
      const original = button.textContent;
      button.textContent = "已复制";
      setTimeout(() => { button.textContent = original; }, 1200);
    }
  } catch (error) {
    showError("复制失败，请手动选择文本复制。");
  }
}

/* ---------- HTTP ---------- */

async function api(path, options) {
  const opts = options || {};
  const method = opts.method || "GET";
  const headers = { Accept: "application/json" };
  if (opts.body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET") headers["X-CSRF-Token"] = (state.session && state.session.csrf_token) || "";

  const response = await fetch(path, {
    method,
    headers,
    cache: "no-store",
    body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
  });

  const raw = await response.text();
  let payload = null;
  if (raw) {
    try { payload = JSON.parse(raw); } catch (error) { payload = null; }
  }

  if (!response.ok) {
    const fallback = raw || `HTTP ${response.status}`;
    const detail = payload && payload.error !== undefined
      ? payload.error
      : (payload && payload.detail !== undefined ? payload.detail : fallback);
    const error = new Error(readableError(detail, fallback));
    error.status = response.status;
    // 保留机器可读的错误码：调用方需要靠它在**同为 503 的两种情况**之间区分，
    // 只按状态码判断会把应用层失败误诊成"功能未启用"。
    error.code = payload && typeof payload.code === "string" ? payload.code : null;
    throw error;
  }
  return payload;
}

/* ---------- 登录与提示 ---------- */

async function bootstrapSession() {
  const statusResponse = await fetch("/api/auth/status", { headers: { Accept: "application/json" }, cache: "no-store" });
  const status = await statusResponse.json().catch(() => ({}));
  state.authStatus = status;

  if (!status.enabled) {
    $("actorName").textContent = "演示模式（未启用认证）";
    return true;
  }

  const sessionResponse = await fetch("/api/auth/session", { headers: { Accept: "application/json" }, cache: "no-store" });
  if (sessionResponse.status === 401) {
    state.session = null;
    $("actorName").textContent = "未登录";
    renderIdentityBanner(
      '登录状态已失效。请先回到 <a href="/">控制台</a> 登录，再进入变更准备。'
    );
    return false;
  }
  const session = await sessionResponse.json().catch(() => ({}));
  if (!sessionResponse.ok) {
    renderIdentityBanner("无法读取登录状态：" + (session.error || "未知错误"));
    return false;
  }
  state.session = session;
  const user = session.user || {};
  const organization = session.organization || {};
  $("actorName").textContent = `${user.name || user.id || "未知成员"} · ${organization.name || user.organization_id || "未知组织"}`;
  return true;
}

function showError(message, hint) {
  const banner = $("errorBanner");
  banner.innerHTML = esc(message) + (hint ? ` <span class="note-inline">${esc(hint)}</span>` : "");
  banner.hidden = false;
}

function clearError() {
  const banner = $("errorBanner");
  banner.hidden = true;
  banner.textContent = "";
}

function renderIdentityBanner(html) {
  const banner = $("identityBanner");
  if (!html) {
    banner.hidden = true;
    banner.textContent = "";
    return;
  }
  banner.innerHTML = html;
  banner.hidden = false;
}

/* ---------- 健康检查 ---------- */

async function refreshHealth() {
  const dot = $("healthDot");
  const text = $("healthText");
  try {
    const health = await api("/api/agent/healthz");
    state.health = health;
    // 健康检查里的键是 provider.provider（见 agent-app/app/llm/provider.py 的 describe()）。
    const provider = health.provider || {};
    const realModel = Boolean(provider.llm_configured);
    const degraded = health.status === "degraded";
    dot.className = "dot " + (degraded || !realModel ? "bad" : "ok");
    if (degraded) {
      text.textContent = "存储降级";
    } else {
      text.textContent = realModel
        ? `${provider.model || "已配置模型"} · 语料 ${health.knowledge_chunks} 片`
        : "确定性生成器（未配置模型）";
    }
    $("healthChip").title = [
      `provider: ${provider.provider || "unknown"}`,
      `模型: ${provider.model || "unknown"}`,
      `模型已配置: ${realModel ? "是" : "否"}`,
      `服务状态: ${health.status || "unknown"}`,
      `检索: ${health.retriever || "unknown"}`,
      `语料片段: ${health.knowledge_chunks}`,
      `运行中任务: ${health.running_tasks}`,
      `未落盘任务: ${(health.unpersisted_tasks || []).length}`,
      health.degraded_reason ? `降级原因: ${health.degraded_reason}` : "降级原因: 无",
      "未配置模型时使用确定性生成器：这是可运行状态，不是残缺状态，但也不是真实模型的起草质量。",
    ].join("\n");
  } catch (error) {
    dot.className = "dot bad";
    // 503 有两种含义，必须靠**错误码**区分，不能只看状态码：
    //   - 治理代理在下游未配置时返回 SERVICE_UNAVAILABLE：功能没开，需要配置后重启；
    //   - 应用层 503 表示"本次操作未生效、可重试"（例如状态未能落盘）。
    // 把后者当成前者，一次存储抖动就会被误诊为"功能没启用"并禁用整个面板。
    if (error.status === 503 && error.code === "SERVICE_UNAVAILABLE") {
      text.textContent = "未启用";
      markAgentDisabled(error.message);
    } else if (error.status === 503) {
      text.textContent = "服务暂时不可用";
      showError("变更准备服务暂时不可用：" + error.message, "该操作未生效，可稍后重试；这不代表功能未启用。");
    } else {
      text.textContent = "服务不可达";
      showError("无法连接变更准备服务：" + error.message);
    }
  }
}

function markAgentDisabled(message) {
  state.agentEnabled = false;
  renderIdentityBanner(
    esc(message) +
    ' 下游 Agent 服务需要配置 <code>DBGUARD_AGENT_BASE_URL</code> 后重启本服务。'
  );
  const button = $("createButton");
  if (button) {
    button.disabled = true;
    button.textContent = "变更准备未启用";
  }
}

/* ---------- 任务动作 ---------- */

/**
 * 需求长度提示：只在接近上限时才显示，平时不占地方。
 * 上限由后端定义（`CreateTaskRequest.requirement` 的 max_length），这里只是提前告知。
 */
function wireRequirementCounter() {
  const input = $("requirement");
  const counter = $("requirementCount");
  if (!input || !counter) return;
  const update = () => {
    const size = (input.value || "").length;
    counter.textContent = size > REQUIREMENT_MAX_CHARS - 400 ? `${size} / ${REQUIREMENT_MAX_CHARS} 字` : "";
  };
  input.addEventListener("input", update);
  update();
}

async function createTask(event) {
  event.preventDefault();
  if (state.busy || !state.agentEnabled) return;

  const requirement = $("requirement").value.trim();
  if (!requirement) {
    showError("请先填写需求。");
    return;
  }
  if (requirement.length > REQUIREMENT_MAX_CHARS) {
    showError(
      `需求最多 ${REQUIREMENT_MAX_CHARS} 字，当前 ${requirement.length} 字。请精简后再提交（长内容请放「表结构快照」或补充说明）。`
    );
    return;
  }

  const payload = { requirement };
  const optional = {
    application: $("optApplication").value.trim(),
    environment: $("optEnvironment").value.trim(),
    database: $("optDatabase").value,
    table: $("optTable").value.trim(),
    query_sql: $("optQuerySql").value.trim(),
    planned_at_timezone: $("optTimezone").value.trim(),
    schema_snapshot: $("optSchema").value.trim(),
  };
  Object.keys(optional).forEach((key) => {
    if (optional[key]) payload[key] = optional[key];
  });

  const plannedAt = $("optPlannedAt").value;
  if (plannedAt) payload.planned_at = new Date(plannedAt).toISOString();

  state.busy = true;
  $("createButton").disabled = true;
  clearError();
  try {
    const task = await api("/api/agent/tasks", { method: "POST", body: payload });
    adoptTask(task);
  } catch (error) {
    handleActionError(error);
  } finally {
    state.busy = false;
    $("createButton").disabled = false;
  }
}

async function clarify(payload) {
  if (!state.task || state.busy) return;
  state.busy = true;
  clearError();
  try {
    const task = await api(`/api/agent/tasks/${state.task.task_id}/clarify`, {
      method: "POST",
      body: payload,
    });
    resetEdits();
    adoptTask(task);
  } catch (error) {
    handleActionError(error);
  } finally {
    state.busy = false;
  }
}

async function cancelTask() {
  if (!state.task || state.busy) return;
  state.busy = true;
  clearError();
  try {
    const task = await api(`/api/agent/tasks/${state.task.task_id}/cancel`, { method: "POST" });
    adoptTask(task);
    stopPolling();
  } catch (error) {
    handleActionError(error);
  } finally {
    state.busy = false;
  }
}

/** 重试：对可恢复状态重新执行一次（服务端会重置为 RECEIVED 并重新调度）。 */
async function retryTask() {
  await clarify({});
}

/** 从检查点恢复：由服务端重新校验归属与输入版本后，从中断点继续。 */
async function resumeTask() {
  if (!state.task || state.busy) return;
  state.busy = true;
  clearError();
  try {
    const task = await api(`/api/agent/tasks/${state.task.task_id}/resume`, { method: "POST" });
    resetEdits();
    adoptTask(task);
  } catch (error) {
    handleActionError(error);
  } finally {
    state.busy = false;
  }
}

/** 人工确认材料：只记录"谁确认了哪一版材料"，不构成审批，也不授予执行许可。 */
async function confirmMaterial() {
  if (!state.task || state.busy) return;
  state.busy = true;
  clearError();
  try {
    const task = await api(`/api/agent/tasks/${state.task.task_id}/confirm`, {
      method: "POST",
      // 带上"我所看到的"材料哈希：材料在别处被改过时，服务端会拒绝并要求刷新，
      // 避免停留在旧页面的人把已经更新的材料确认掉。
      body: { material_hash: state.task.material_hash || null },
    });
    adoptTask(task);
  } catch (error) {
    handleActionError(error);
  } finally {
    state.busy = false;
  }
}

function handleActionError(error) {
  if (error.status === 401) {
    state.session = null;
    renderIdentityBanner('登录状态已失效。请先回到 <a href="/">控制台</a> 登录。');
    return;
  }
  if (error.status === 503) {
    // 503 有两种含义，不能混为一谈：
    //   - 治理代理在下游未配置时返回 SERVICE_UNAVAILABLE：功能没开，需要配置后重启；
    //   - 应用层返回 503 表示"本次操作未生效、可重试"（例如取消时状态未能落盘，
    //     任务仍在运行）。
    // 把后者当成前者，一次存储抖动就会被误诊为"功能没启用"，并让整个面板不可用。
    if (error.code === "SERVICE_UNAVAILABLE") {
      markAgentDisabled(error.message);
    } else {
      showError(error.message);
    }
    return;
  }
  showError(error.message);
}

function adoptTask(task) {
  state.task = task;
  render();
  if (TERMINAL.has(task.status)) {
    stopPolling();
  } else {
    startPolling();
  }
}

function resetEdits() {
  state.edits = { sql: null, rollback: null };
  state.editing = false;
}

/* ---------- 轮询 ---------- */

function startPolling() {
  if (state.timer) return;
  state.timer = setInterval(pollOnce, POLL_MS);
}

function stopPolling() {
  if (state.timer) {
    clearInterval(state.timer);
    state.timer = null;
  }
}

async function pollOnce() {
  if (!state.task || state.busy) return;
  try {
    const task = await api(`/api/agent/tasks/${state.task.task_id}`);
    const previous = state.task;
    state.task = task;
    // 只有状态或草案发生变化才整体重绘，避免打断正在阅读或编辑的人。
    if (previous.status !== task.status || JSON.stringify(previous.draft || null) !== JSON.stringify(task.draft || null)) {
      resetEdits();
      render();
    }
    if (TERMINAL.has(task.status)) stopPolling();
  } catch (error) {
    stopPolling();
    handleActionError(error);
  }
}

/* ---------- 渲染总入口 ---------- */

function render() {
  renderPanel();
  renderConversation();
  renderDraft();
  renderEvidence();
}

function renderPanel() {
  const task = state.task;
  const meta = $("draftMeta");
  if (!task) {
    meta.textContent = "尚未生成";
    return;
  }
  const parts = [task.task_id];
  if (task.draft) parts.push(`v${task.draft.version}`, `修订 ${task.revisions} 次`);
  meta.textContent = parts.join(" · ");
}

/* ---------- 左栏：对话 ---------- */

function renderConversation() {
  const host = $("conversation");
  const task = state.task;
  if (!task) {
    host.innerHTML = "";
    return;
  }

  const blocks = [];

  blocks.push(`
    <article class="turn turn-user">
      <div class="turn-head"><span>需求</span><span>·</span><span>${esc(task.task_id)}</span></div>
      <div class="bubble">${esc(task.requirement)}</div>
      <div>${statusBadge(task.status)}</div>
    </article>
  `);

  if (task.error) {
    blocks.push(`
      <article class="card card-danger">
        <div class="card-title"><span>为什么停在这里</span></div>
        <p>${esc(task.error)}</p>
      </article>
    `);
  }

  if (task.status === "INPUT_REJECTED") {
    blocks.push(`
      <article class="card card-danger">
        <div class="card-title"><span>输入被拒绝</span></div>
        <p>输入命中提示注入检测，已停止处理，未生成任何草案。</p>
      </article>
    `);
  }

  if (task.planned_at_missing && task.status !== "DRAFT_READY") {
    blocks.push(`
      <article class="card card-warn">
        <div class="card-title"><span>必须补充</span></div>
        <p>计划时间尚未提供。它是生成草案的必要信息之一，不会被猜测填补。</p>
      </article>
    `);
  }

  if (task.status === "NEEDS_INFO" && (task.questions || []).length) {
    blocks.push(renderQuestions(task));
  }

  blocks.push(renderTimeline(task));

  const checkpointResumable = Boolean(task.awaiting_input) || task.restart_policy === "checkpoint_available";
  const actions = [];
  if (ACTIVE.has(task.status)) {
    actions.push('<button class="button button-small button-danger" type="button" id="cancelButton">停止</button>');
  }
  if (RESUMABLE.has(task.status)) {
    // 有检查点时是"从检查点恢复"（从等待点续跑），没有时才是"重新执行一次"。
    // 两者不能混为一谈：把重跑说成续跑是不诚实的。
    actions.push(
      checkpointResumable
        ? '<button class="button button-small" type="button" id="resumeButton">从检查点恢复</button>'
        : '<button class="button button-small" type="button" id="retryButton">重新执行一次</button>'
    );
  }
  if (actions.length) blocks.push(`<div class="sql-actions">${actions.join("")}</div>`);

  host.innerHTML = blocks.join("");

  const cancelButton = $("cancelButton");
  if (cancelButton) cancelButton.addEventListener("click", cancelTask);

  const retryButton = $("retryButton");
  if (retryButton) retryButton.addEventListener("click", retryTask);

  const resumeButton = $("resumeButton");
  if (resumeButton) resumeButton.addEventListener("click", resumeTask);

  wireQuestionForm();
}

function renderQuestions(task) {
  const rows = (task.questions || []).map((question, index) => {
    const field = CLARIFY_FIELDS.find((item) => item.name === question.field);
    const label = field ? field.label : question.field;
    const inputName = `q_${esc(question.field)}_${index}`;
    let control;
    if (field && field.type === "select") {
      const options = field.options
        .map(([value, text]) => `<option value="${esc(value)}">${esc(text)}</option>`)
        .join("");
      control = `<select data-field="${esc(question.field)}" id="${inputName}">${options}</select>`;
    } else if (field && field.type === "textarea") {
      control = `<textarea data-field="${esc(question.field)}" id="${inputName}" rows="3" placeholder="${esc(field.placeholder || "")}"></textarea>`;
    } else if (question.field === "planned_at") {
      control = `<input data-field="planned_at" id="${inputName}" type="datetime-local">`;
    } else {
      control = `<input data-field="${esc(question.field)}" id="${inputName}" type="text" placeholder="${esc((field && field.placeholder) || "")}">`;
    }
    const examples = question.examples || [];
    return `
      <label class="field">
        <span>${esc(question.question || label)}</span>
        ${control}
        <span class="note-inline">${esc(question.reason || "")}</span>
        ${examples.length ? `<span class="note-inline">例如：${esc(examples.join("、"))}</span>` : ""}
      </label>
    `;
  }).join("");

  return `
    <article class="card">
      <div class="card-title"><span>需要你补充</span><span class="badge badge-warn">缺失信息不会被猜测</span></div>
      <div class="stack" id="questionFields">${rows}</div>
      <button class="button button-primary" type="button" id="clarifyButton">提交并继续</button>
    </article>
  `;
}

function wireQuestionForm() {
  const button = $("clarifyButton");
  if (!button) return;
  button.addEventListener("click", () => {
    const payload = {};
    document.querySelectorAll("#questionFields [data-field]").forEach((node) => {
      const field = node.getAttribute("data-field");
      const value = (node.value || "").trim();
      if (!value) return;
      if (field === "planned_at") {
        payload.planned_at = new Date(value).toISOString();
      } else {
        payload[field] = value;
      }
    });
    if (!Object.keys(payload).length) {
      showError("请至少补充一项信息。");
      return;
    }
    clarify(payload);
  });
}

function renderTimeline(task) {
  const events = task.events || [];
  if (!events.length) return "";
  const items = events.map((item) => `
    <li>
      <span class="at">${esc(formatTime(item.at))}</span>
      <span class="kind">${esc(item.kind)}</span>
      <span class="detail">${esc(item.detail)}</span>
    </li>
  `).join("");
  return `
    <article class="card card-flat">
      <div class="card-title"><span>进度</span><span>${events.length} 步</span></div>
      <ol class="timeline">${items}</ol>
      <p class="note-inline">只记录步骤与结论，不记录模型思维链。</p>
    </article>
  `;
}

/* ---------- 中栏：草案 ---------- */

function draftSql() {
  if (!state.task || !state.task.draft) return "";
  return state.edits.sql === null ? state.task.draft.sql : state.edits.sql;
}

function draftRollback() {
  if (!state.task || !state.task.draft) return "";
  return state.edits.rollback === null ? state.task.draft.rollback_sql : state.edits.rollback;
}

function isLocallyEdited() {
  const draft = state.task && state.task.draft;
  if (!draft) return false;
  return draftSql() !== draft.sql || draftRollback() !== draft.rollback_sql;
}

function renderDraft() {
  const host = $("draftBody");
  const task = state.task;

  if (!task) {
    host.innerHTML = `<p class="empty">提交需求后，这里会显示待确认的草案。</p>`;
    return;
  }

  if (!task.draft) {
    const messages = {
      RECEIVED: "任务已接收，等待执行。",
      RUNNING: "正在生成草案……",
      NEEDS_INFO: "信息不完整，先补齐后再生成。草案不会在信息缺失时提前编造。",
      CHECK_BLOCKED: "确定性检查未通过或未完成，本轮没有可确认的草案。",
      INPUT_REJECTED: "输入被拒绝，未生成草案。",
      FAILED: "执行失败，未生成草案。",
      CANCELLED: "任务已取消。",
    };
    host.innerHTML = `<p class="empty">${esc(messages[task.status] || "暂无草案。")}</p>`;
    return;
  }

  const draft = task.draft;
  const parts = [];

  parts.push(`
    <article class="card card-flat">
      <div class="card-title">
        <span>草案 v${esc(draft.version)}</span>
        <span>修订 ${esc(task.revisions)} 次</span>
      </div>
      <p class="note"><strong>草案已生成，仅表示材料准备好可供人工确认。</strong>
      它不是审批结论，不会自动提交变更，也不代表可以在生产执行。</p>
      <dl class="kv">
        <dt>应用</dt><dd>${esc(draft.application || "未提供")}</dd>
        <dt>环境</dt><dd>${esc(draft.environment || "未提供")}</dd>
        <dt>数据库</dt><dd>${esc(draft.database || "unknown")}</dd>
        <dt>计划时间</dt><dd>${esc(formatDate(draft.planned_at))}${draft.planned_at_timezone ? " · " + esc(draft.planned_at_timezone) : ""}</dd>
      </dl>
    </article>
  `);

  if (isLocallyEdited()) {
    parts.push(`
      <article class="card card-warn" id="staleNoticeMiddle">
        <div class="card-title"><span>本地编辑未经验证</span></div>
        <p>下面显示的 SQL 已被本地修改。右侧的检查结果对应的是<strong>生成时的那一份 SQL</strong>，对当前文本已失效。</p>
        <p class="note-inline">本地编辑只用于审阅，不会回传服务端。需要正式修改请重新提交需求。</p>
      </article>
    `);
  }

  const readOnlyAttr = state.editing ? "" : "readonly";
  const staleClass = isLocallyEdited() ? " stale" : "";

  parts.push(`
    <article class="card">
      <div class="sql-head">
        <div class="card-title"><span>变更 SQL</span></div>
        <div class="sql-actions">
          <label class="note-inline"><input type="checkbox" id="editToggle" ${state.editing ? "checked" : ""}> 本地编辑</label>
          <button class="button button-small" type="button" id="copySql">复制</button>
          <button class="button button-small" type="button" id="resetSql" ${isLocallyEdited() ? "" : "disabled"}>恢复生成版本</button>
        </div>
      </div>
      <div class="sql-block">
        <textarea id="sqlText" class="mono${staleClass}" rows="12" ${readOnlyAttr} spellcheck="false">${esc(draftSql())}</textarea>
      </div>
    </article>

    <article class="card">
      <div class="sql-head">
        <div class="card-title"><span>回滚方案</span></div>
        <div class="sql-actions">
          <button class="button button-small" type="button" id="copyRollback">复制</button>
        </div>
      </div>
      <div class="sql-block">
        <textarea id="rollbackText" class="mono${staleClass}" rows="6" ${readOnlyAttr} spellcheck="false">${esc(draftRollback())}</textarea>
      </div>
    </article>
  `);

  parts.push(renderAssumptions(draft));
  parts.push(renderAdvice(draft));
  parts.push(renderConfirmation(task));

  if ((draft.revision_notes || []).length) {
    parts.push(`
      <article class="card card-flat">
        <div class="card-title"><span>本轮修订记录</span></div>
        <ul>${draft.revision_notes.map((note) => `<li>${esc(note)}</li>`).join("")}</ul>
      </article>
    `);
  }

  host.innerHTML = parts.join("");

  const sqlText = $("sqlText");
  const rollbackText = $("rollbackText");
  if (sqlText) {
    sqlText.addEventListener("input", () => {
      state.edits.sql = sqlText.value;
      markStale(sqlText, rollbackText);
    });
  }
  if (rollbackText) {
    rollbackText.addEventListener("input", () => {
      state.edits.rollback = rollbackText.value;
      markStale(sqlText, rollbackText);
    });
  }

  const editToggle = $("editToggle");
  if (editToggle) {
    editToggle.addEventListener("change", () => {
      state.editing = editToggle.checked;
      if (sqlText) sqlText.readOnly = !state.editing;
      if (rollbackText) rollbackText.readOnly = !state.editing;
    });
  }

  const copySql = $("copySql");
  if (copySql) copySql.addEventListener("click", () => copyText(draftSql(), copySql));

  const copyRollback = $("copyRollback");
  if (copyRollback) copyRollback.addEventListener("click", () => copyText(draftRollback(), copyRollback));

  const resetSql = $("resetSql");
  if (resetSql) {
    resetSql.addEventListener("click", () => {
      resetEdits();
      renderDraft();
      renderEvidence();
    });
  }

  const confirmButtonEl = $("confirmButton");
  if (confirmButtonEl) confirmButtonEl.addEventListener("click", confirmMaterial);
}

/** SQL 一改，旧检查结果必须立刻标为失效，而不是继续显示为当前结论。 */
function markStale(sqlNode, rollbackNode) {
  const stale = isLocallyEdited();
  if (sqlNode) sqlNode.classList.toggle("stale", stale);
  if (rollbackNode) rollbackNode.classList.toggle("stale", stale);
  renderEvidence();
}

function renderAssumptions(draft) {
  const assumptions = draft.assumptions || [];
  const openQuestions = draft.open_questions || [];
  if (!assumptions.length && !openQuestions.length) return "";

  const confirmed = assumptions.filter((item) => item.confirmed);
  const pending = assumptions.filter((item) => !item.confirmed);
  const renderList = (items) => items.map((item) => `<li>${esc(item.statement)}</li>`).join("");

  return `
    <article class="card card-warn">
      <div class="card-title"><span>假设与待确认项</span><span class="badge badge-warn">需人工确认</span></div>
      ${confirmed.length ? `<div><strong class="note-inline">已确认</strong><ul>${renderList(confirmed)}</ul></div>` : ""}
      ${pending.length ? `<div><strong class="note-inline">待确认</strong><ul>${renderList(pending)}</ul></div>` : ""}
      ${openQuestions.length ? `<div><strong class="note-inline">未解决问题</strong><ul>${openQuestions.map((item) => `<li>${esc(item)}</li>`).join("")}</ul></div>` : ""}
    </article>
  `;
}

function renderAdvice(draft) {
  const advice = draft.ai_advice || {};
  const reasons = advice.reasons || [];
  return `
    <article class="card card-flat">
      <div class="card-title">
        <span>AI 风险建议</span>
        <span class="badge badge-muted">不参与放行判定</span>
      </div>
      <dl class="kv">
        <dt>建议等级</dt><dd>${esc(advice.advisory_risk || "UNKNOWN")}</dd>
      </dl>
      ${advice.summary ? `<p>${esc(advice.summary)}</p>` : ""}
      ${reasons.length ? `<ul>${reasons.map((item) => `<li>${esc(item)}</li>`).join("")}</ul>` : ""}
      <p class="note-inline">风险等级与阻断项由确定性扫描产生，模型只能提供这段参考意见。</p>
    </article>
  `;
}

/** 材料确认：与"模型建议""确定性检查""治理审批"三者必须一眼可区分。 */
function renderConfirmation(task) {
  if (!task.draft) return "";
  const records = task.confirmations || [];
  const currentHash = task.material_hash || "";
  const confirmedCurrent = records.some((item) => !item.invalidated_at && item.material_hash === currentHash);

  const list = records.length
    ? `<div class="stack">${records.map((item) => {
        const invalidated = Boolean(item.invalidated_at);
        return `
          <div class="confirm-item${invalidated ? " confirm-stale" : ""}">
            <div class="confirm-head">
              <span class="badge ${invalidated ? "badge-muted" : "badge-confirm"}">${invalidated ? "已失效" : "有效"}</span>
              <span class="note-inline">${esc(formatDate(item.confirmed_at))} · ${esc(item.confirmed_by)}</span>
            </div>
            ${item.note ? `<p>${esc(item.note)}</p>` : ""}
            <div class="evidence-meta">
              <span>材料版本 ${esc((item.material_version || "").slice(0, 12))}</span>
              <span>内容哈希 ${esc((item.material_hash || "").slice(0, 12))}</span>
            </div>
            ${invalidated ? `<p class="note-inline">失效原因：${esc(item.invalidate_reason || "材料已变化")}</p>` : ""}
          </div>
        `;
      }).join("")}</div>`
    : `<p class="note-inline">还没有人工确认记录。确认只表示"有人看过这一版材料"，不构成审批。</p>`;

  const button = confirmedCurrent
    ? '<button class="button button-small" type="button" id="confirmButton" disabled>当前材料已确认</button>'
    : '<button class="button button-small button-primary" type="button" id="confirmButton">确认这一版材料</button>';

  return `
    <article class="card">
      <div class="card-title">
        <span>材料确认</span>
        <span class="badge badge-confirm">人工确认 ≠ 治理审批 ≠ 执行许可</span>
      </div>
      <p class="note-inline">这里只记录"谁在什么时候确认了哪一版材料（含内容哈希）"。
      它<strong>不改变放行判定</strong>，也<strong>不授予任何执行权限</strong>；审批与通行证签发由 ChangeGuard 治理服务完成。
      草案一旦重新生成且内容不同，旧确认会失效并保留痕迹。</p>
      ${list}
      <div class="sql-actions">${button}</div>
    </article>
  `;
}

/** 执行轨迹与预算：实际用了什么策略、为什么停下、花了多少。 */
function renderTrajectory(task) {
  const investigation = task.investigation || null;
  const strategy = task.strategy || (investigation && investigation.strategy) || "unknown";
  const usage = task.usage || (investigation && investigation.usage) || null;
  const observations = (investigation && investigation.tool_observations) || [];

  const usageText = !usage
    ? "未知（provider 未提供）"
    : usage.known
      ? `prompt ${usage.prompt_tokens} · completion ${usage.completion_tokens}` +
        (usage.cost_estimate === null || usage.cost_estimate === undefined ? " · 费用未知" : ` · 费用 ${usage.cost_estimate}`)
      : `${usage.missing_responses || 0}/${usage.requests || 0} 次响应未提供 usage：总量不完整（不填 0 冒充已知）`;

  const items = observations.length
    ? observations.map((item) => `
        <div class="check-item">
          <span class="check-code">${esc(item.tool)}</span>
          <span class="check-body">
            <span>
              ${item.ok ? '<span class="badge badge-ok">成功</span>' : '<span class="badge badge-danger">失败</span>'}
              <span class="note-inline">${esc(TOOL_KIND_LABELS[item.kind] || item.kind || "其他")}${item.data_version ? " · v" + esc(item.data_version) : ""}</span>
            </span>
            ${(item.summary || item.error) ? `<span class="check-suggestion">${esc(item.summary || item.error)}</span>` : ""}
          </span>
        </div>
      `).join("")
    : `<p class="note-inline">本轮没有工具调用观察。</p>`;

  return `
    <article class="card card-flat">
      <div class="card-title">
        <span>执行轨迹与预算</span>
        <span class="badge badge-muted">策略 ${esc(strategy)}</span>
      </div>
      <dl class="kv">
        <dt>决策者</dt><dd>${esc((investigation && investigation.planner) || "fixed")}</dd>
        <dt>停止原因</dt><dd>${esc(STOP_REASON_LABELS[(investigation && investigation.stop_reason)] || (investigation && investigation.stop_reason) || "-")}</dd>
        <dt>轮次 / 工具调用</dt><dd>${esc((investigation && investigation.rounds) || 0)} / ${esc((investigation && investigation.tool_calls) || 0)}</dd>
        <dt>预算（token）</dt><dd>${esc(usageText)}</dd>
      </dl>
      ${investigation && (investigation.missing_required || []).length
        ? `<p class="tone-danger"><strong>仍缺的必需证据：</strong>${esc(investigation.missing_required.join("；"))}</p>`
        : ""}
      <div class="stack">${items}</div>
      <p class="note-inline">工具结果是<strong>不可信数据</strong>，这里只展示有界摘要，不会被执行；也不记录模型思维链。</p>
    </article>
  `;
}

/** 把"治理审批"作为独立一栏显式说明：工作台不做审批，也不产生执行许可。 */
function renderGovernanceBoundary() {
  return `
    <article class="card card-flat">
      <div class="card-title"><span>治理审批</span><span class="badge badge-muted">不在此处</span></div>
      <p class="note-inline">审批、制品摘要、通行证签发与原子消费都由 <strong>ChangeGuard 治理服务</strong>完成。
      这个工作台只准备材料，<strong>不产生审批结论，也不产生执行许可</strong>。材料确认只是"有人看过这份材料"。</p>
    </article>
  `;
}

/** 存储降级与模型不可用必须可见，且不能与"功能未启用"混为一谈。 */
function renderServiceStatus() {
  const health = state.health;
  if (!health) return "";
  const blocks = [];
  const provider = health.provider || {};

  if (health.status === "degraded") {
    const tasks = health.unpersisted_tasks || [];
    blocks.push(`
      <article class="card card-danger">
        <div class="card-title"><span>存储降级</span><span class="badge badge-danger">degraded</span></div>
        <p>${esc(health.degraded_reason || "存在未能落盘的执行结果，存储可能不可用。")}</p>
        ${tasks.length ? `<p class="note-inline">未落盘任务：${esc(tasks.join("、"))}</p>` : ""}
        <p class="note-inline">这不是"功能未启用"：服务仍在运行，只是结果暂时未能落盘，请稍后重试该操作。</p>
      </article>
    `);
  }
  if (!provider.llm_configured) {
    blocks.push(`
      <article class="card card-warn">
        <div class="card-title"><span>模型不可用</span><span class="badge badge-warn">确定性生成器</span></div>
        <p>当前使用确定性生成器：这是可运行状态，不是残缺状态，但不代表真实模型的起草质量。</p>
      </article>
    `);
  }
  return blocks.join("");
}

/* ---------- 右栏：证据与检查 ---------- */

function renderEvidence() {
  const host = $("evidenceBody");
  const task = state.task;

  if (!task) {
    host.innerHTML = `<p class="empty">检查结果与引用来源会显示在这里。</p>`;
    return;
  }

  const draft = task.draft;
  const parts = [];

  parts.push(renderServiceStatus());
  parts.push(renderTrajectory(task));
  if (draft) {
    parts.push(renderCheck(draft));
    parts.push(renderEvidenceList(draft));
  } else {
    parts.push(`<p class="empty">尚无检查结果：本轮没有生成草案。</p>`);
  }

  parts.push(renderShadowNotice());
  parts.push(renderGovernanceBoundary());
  parts.push(renderProvenance());

  host.innerHTML = parts.join("");
}

function renderCheck(draft) {
  const check = draft.deterministic_check || {};
  const status = check.status || "NOT_RUN";
  const meta = CHECK_META[status] || CHECK_META.NOT_RUN;
  const items = check.items || [];
  const stale = isLocallyEdited();

  const staleBlock = stale
    ? `<div class="card card-warn" id="staleNoticeRight">
         <strong>当前 SQL 已被本地修改</strong>
         <p class="note-inline">以下结论对应生成时的 SQL，对当前文本<strong>已失效</strong>。请勿据此判断当前 SQL 的安全性。</p>
       </div>`
    : "";

  const itemBlock = items.length
    ? items.map((item) => `
        <div class="check-item">
          <span class="check-code">${esc(item.code)}</span>
          <span class="check-body">
            <span>${item.blocking ? '<span class="badge badge-danger">阻断</span> ' : ""}${esc(item.title || "")}</span>
            ${item.suggestion ? `<span class="check-suggestion">${esc(item.suggestion)}</span>` : ""}
          </span>
        </div>
      `).join("")
    : `<p class="note-inline">本次扫描没有产生条目：这只能说未命中已知规则，不代表不存在风险。</p>`;

  return `
    <article class="card">
      <div class="card-title">
        <span>确定性检查</span>
        <span class="badge ${meta.badge}">${esc(meta.label)}</span>
      </div>
      <p class="note-inline">${esc(meta.note)}</p>
      ${staleBlock}
      <dl class="kv">
        <dt>来源</dt><dd class="mono">${esc(check.source || "local_scan")}</dd>
        <dt>检查时间</dt><dd>${esc(check.checked_at ? formatDate(check.checked_at) : "未记录")}</dd>
        <dt>阻断项</dt><dd>${esc(check.blocking_count || 0)}</dd>
      </dl>
      ${check.error ? `<p class="tone-danger"><strong>执行错误：</strong>${esc(check.error)}</p>` : ""}
      <div>${itemBlock}</div>
      <p class="note-inline">这份结论由本地静态扫描产生，模型无权修改或清空。它只是静态结论，不替代隔离库演练与人工审批。</p>
    </article>
  `;
}

function renderEvidenceList(draft) {
  const evidence = draft.evidence || [];
  if (!evidence.length) {
    return `
      <article class="card card-warn">
        <div class="card-title"><span>引用证据</span><span class="badge badge-warn">依据不足</span></div>
        <p>检索没有命中可引用的规范片段。草案标注为依据不足，不会用模型经验补足。</p>
      </article>
    `;
  }

  const statusBadgeFor = (status) => {
    if (status === "active") return '<span class="badge badge-ok">有效</span>';
    if (status === "deprecated") return '<span class="badge badge-danger">已废弃</span>';
    return '<span class="badge badge-muted">状态未知</span>';
  };

  const items = evidence.map((item) => `
    <article class="evidence-item">
      <div class="evidence-head">
        <span class="evidence-title">${esc(item.title)}</span>
        ${statusBadgeFor(item.status)}
      </div>
      ${item.section ? `<span class="note-inline">章节：${esc(item.section)}</span>` : ""}
      <div class="evidence-snippet">${esc(item.snippet)}</div>
      <div class="evidence-meta">
        <span>${esc(item.evidence_id)}</span>
        <span>doc:${esc(item.doc_id)}</span>
        ${item.version ? `<span>v${esc(item.version)}</span>` : ""}
        <span>score ${esc(Number(item.score || 0).toFixed(3))}</span>
        <span>${esc(item.source)}</span>
      </div>
    </article>
  `).join("");

  const deprecated = evidence.filter((item) => item.status === "deprecated").length;

  return `
    <article class="card">
      <div class="card-title">
        <span>引用证据</span>
        <span>${evidence.length} 条${deprecated ? ` · ${deprecated} 条已废弃` : ""}</span>
      </div>
      ${deprecated ? `<p class="tone-danger">其中有已废弃的规范，需要人工确认是否仍适用。</p>` : ""}
      <div class="stack">${items}</div>
      <p class="note-inline">引用必须指向真实存在的片段；编造证据 ID 会直接失败。</p>
    </article>
  `;
}

function renderShadowNotice() {
  return `
    <article class="card card-flat">
      <div class="card-title"><span>隔离库演练（影子验证）</span><span class="badge badge-muted">本服务不触发</span></div>
      <p class="note-inline">变更准备<strong>不做</strong>隔离库演练：它不会执行任何 SQL，也不会接触数据库。</p>
      <p class="note-inline">真实演练由具备权限的人在 ChangeGuard 治理后端触发；只有
      <code>Mode=POSTGRES</code>、<code>Status=PASSED</code>、回滚验证通过、且摘要与规则版本一致时才算有效证据。</p>
      <p class="note-inline"><code>DEMO_ONLY</code> 与 <code>NOT_RUN</code> 在任何情况下都不能当作验证通过。</p>
    </article>
  `;
}

function renderProvenance() {
  return `
    <article class="card card-flat">
      <div class="card-title"><span>出处与边界</span></div>
      <ul class="note-inline">
        <li>确定性检查由<b>本地静态扫描</b>产生，模型只能提供参考建议。</li>
        <li>语料是合成示例，不是生产规范；结论不可直接用于生产决策。</li>
        <li>变更准备<b>不参与审批与发布</b>，也不判定变更能否上线。</li>
      </ul>
    </article>
  `;
}

/* ---------- 启动 ---------- */

async function init() {
  $("createForm").addEventListener("submit", createTask);
  wireRequirementCounter();
  $("healthChip").addEventListener("click", () => {
    window.alert($("healthChip").title || "无健康信息。");
  });

  render();

  let authenticated = true;
  try {
    authenticated = await bootstrapSession();
  } catch (error) {
    renderIdentityBanner("无法读取登录状态，请刷新页面重试。");
    authenticated = false;
  }

  if (!authenticated) {
    const button = $("createButton");
    if (button) button.disabled = true;
    return;
  }

  await refreshHealth();
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", init);
} else {
  init();
}
