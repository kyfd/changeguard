"use strict";

const state = {
  users: [], apps: [], changes: [], policies: [], dashboard: null, audits: [], config: null, authStatus: null, session: null, organization: null, invites: [],
  actorId: localStorage.getItem("dbguard_actor") || "usr_developer",
  currentChange: null, review: null, findingAction: null, editingId: null, editingPolicy: null, editingMember: null, editingApp: null,
  assetFilters: {keyword:"", environment:""},
  riskFilters: {keyword:"", application:"", severity:"", status:""}
};

const icons = {
  overview: '<rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/>',
  database: '<ellipse cx="12" cy="5" rx="7.5" ry="3"/><path d="M4.5 5v6c0 1.7 3.4 3 7.5 3s7.5-1.3 7.5-3V5M4.5 11v6c0 1.7 3.4 3 7.5 3s7.5-1.3 7.5-3v-6"/>',
  flask: '<path d="M9 3h6M10 3v5l-5.6 9.2A2.5 2.5 0 0 0 6.5 21h11a2.5 2.5 0 0 0 2.1-3.8L14 8V3"/><path d="M7.5 15h9"/>',
  approval: '<path d="M9 11l2 2 4-5"/><rect x="4" y="3" width="16" height="18" rx="2"/><path d="M8 3V1M16 3V1"/>',
  apps: '<path d="m12 3 8 4.5-8 4.5-8-4.5L12 3Z"/><path d="m4 12 8 4.5 8-4.5M4 16.5l8 4.5 8-4.5"/>',
  audit: '<path d="M12 8v5l3 2"/><circle cx="12" cy="12" r="9"/>',
  settings: '<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.9l.1.1-2.8 2.8-.1-.1a1.7 1.7 0 0 0-1.9-.3 1.7 1.7 0 0 0-1 1.6v.2h-4V21a1.7 1.7 0 0 0-1-1.6 1.7 1.7 0 0 0-1.9.3l-.1.1L4.2 17l.1-.1a1.7 1.7 0 0 0 .3-1.9A1.7 1.7 0 0 0 3 14H2.8v-4H3a1.7 1.7 0 0 0 1.6-1 1.7 1.7 0 0 0-.3-1.9L4.2 7 7 4.2l.1.1A1.7 1.7 0 0 0 9 4.6a1.7 1.7 0 0 0 1-1.6v-.2h4V3a1.7 1.7 0 0 0 1 1.6 1.7 1.7 0 0 0 1.9-.3l.1-.1L19.8 7l-.1.1a1.7 1.7 0 0 0-.3 1.9 1.7 1.7 0 0 0 1.6 1h.2v4H21a1.7 1.7 0 0 0-1.6 1Z"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  arrow: '<path d="m9 18 6-6-6-6"/>',
  back: '<path d="m15 18-6-6 6-6"/>',
  check: '<path d="m5 12 4 4L19 6"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
  shield: '<path d="M12 3 20 6.5v5c0 4.7-3.1 8-8 9.5-4.9-1.5-8-4.8-8-9.5v-5L12 3Z"/><path d="m8.5 12 2.1 2.1 4.9-5"/>',
  code: '<path d="m8 9-3 3 3 3M16 9l3 3-3 3M14 5l-4 14"/>',
  lock: '<rect x="4" y="10" width="16" height="11" rx="2"/><path d="M8 10V7a4 4 0 0 1 8 0v3"/>',
  activity: '<path d="M3 12h4l2-6 4 12 2-6h6"/>',
  users: '<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.9M16 3.1a4 4 0 0 1 0 7.8"/>',
  file: '<path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8Z"/><path d="M14 2v6h6M8 13h8M8 17h5"/>',
  refresh: '<path d="M20 6v5h-5M4 18v-5h5"/><path d="M6.1 9a7 7 0 0 1 11.8-2.6L20 11M4 13l2.1 4.6A7 7 0 0 0 18 15"/>',
  server: '<rect x="3" y="4" width="18" height="6" rx="2"/><rect x="3" y="14" width="18" height="6" rx="2"/><path d="M7 7h.01M7 17h.01"/>',
  alert: '<path d="M10.3 3.7 2.7 17a2 2 0 0 0 1.7 3h15.2a2 2 0 0 0 1.7-3L13.7 3.7a2 2 0 0 0-3.4 0Z"/><path d="M12 9v4M12 17h.01"/>'
};

const navItems = [
  { group: "工作台", items: [
    { route: "dashboard", label: "概览", icon: "overview" },
    { route: "panorama", label: "治理全景", icon: "activity" },
    { route: "assets", label: "服务", icon: "apps" },
    { route: "risks", label: "风险项", icon: "alert", count: () => state.changes.flatMap(item => item.findings || []).filter(item => item.status !== "VERIFIED").length },
    { route: "changes", label: "变更单", icon: "code" },
    { route: "calendar", label: "发布日历", icon: "file" },
    { route: "experiments", label: "预发布验证", icon: "flask" },
    { route: "approvals", label: "待审批", icon: "approval", count: () => state.dashboard?.pending_approvals?.length || 0 },
    { route: "observability", label: "发布观测", icon: "activity" },
    { route: "incidents", label: "事故回溯", icon: "alert" }
  ]},
  { group: "配置", items: [
    { route: "policies", label: "规则", icon: "shield" },
    { route: "enterprise", label: "成员", icon: "users" },
    { route: "apps", label: "服务配置", icon: "apps" },
    { route: "audits", label: "审计报告", icon: "audit" },
    { route: "settings", label: "集成设置", icon: "settings" }
  ]}
];

function svg(name) { return `<svg viewBox="0 0 24 24" aria-hidden="true">${icons[name] || icons.file}</svg>`; }
function escapeHTML(value = "") { return String(value).replace(/[&<>"']/g, char => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[char])); }
function initials(name = "") { return [...name].slice(-2).join(""); }
function formatDate(value, withTime = true) {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "—";
  return new Intl.DateTimeFormat("zh-CN", withTime ? {month:"2-digit",day:"2-digit",hour:"2-digit",minute:"2-digit",hour12:false} : {year:"numeric",month:"2-digit",day:"2-digit"}).format(date).replaceAll("/", "-");
}
function duration(ms = 0) { return ms >= 1000 ? (ms / 1000).toFixed(ms >= 10000 ? 0 : 1) + "s" : ms + "ms"; }
function pct(value) { return Number(value || 0).toFixed(1) + "%"; }
function statusInfo(status) {
  const map = {
    DRAFT:["草稿","draft"], CHECKING:["规则检查中","running"], CHECK_FAILED:["检查未通过","failed"],
    READY_FOR_EXPERIMENT:["待预发布验证","ready"], EXPERIMENT_QUEUED:["验证排队中","queued"],
    EXPERIMENT_RUNNING:["验证进行中","running"], WAITING_APPROVAL:["待发布审批","waiting"],
    APPROVED:["已批准","approved"], REJECTED:["已驳回","rejected"], COMPLETED:["已完成","completed"]
  };
  return map[status] || [status || "未知","draft"];
}
function statusBadge(status) { const [label, cls] = statusInfo(status); return `<span class="status status-${cls}"><i></i>${label}</span>`; }
function riskBadge(risk) {
  const map = {LOW:["低风险","low"],MEDIUM:["中风险","medium"],HIGH:["高风险","high"],UNKNOWN:["待评估","unknown"]};
  const [label, cls] = map[risk] || map.UNKNOWN;
  return `<span class="risk risk-${cls}"><i></i>${label}</span>`;
}
function actor() { return state.users.find(user => user.id === state.actorId) || state.users[0] || {id:"usr_developer",name:"刘丰熙",role:"后端开发"}; }
function roleLabel(role) { return role === "数据库审核人" ? "发布审核人" : role; }

async function api(path, options = {}) {
  const headers = {"Content-Type":"application/json","X-Actor-ID":state.actorId,"X-CSRF-Token":state.session?.csrf_token || "",...options.headers};
  const response = await fetch(path, {...options, headers});
  const contentType = response.headers.get("content-type") || "";
  const data = contentType.includes("json") ? await response.json() : null;
  if (!response.ok) {
    if (response.status === 401 && state.authStatus?.enabled && !path.startsWith("/api/auth/")) {
      state.session = null;
      state.organization = null;
      renderAuthGate("登录状态已失效，请重新登录");
    }
    const error = new Error(data?.error || "请求失败，请稍后重试");
    error.status = response.status;
    throw error;
  }
  return data;
}

async function bootstrapAuthentication() {
  const statusResponse = await fetch("/api/auth/status", {headers:{"Accept":"application/json"}, cache:"no-store"});
  const status = await statusResponse.json().catch(() => ({}));
  if (!statusResponse.ok) throw new Error(status.error || "无法读取认证配置");
  state.authStatus = status;
  if (!status.enabled) {
    hideAuthGate();
    return true;
  }
  const sessionResponse = await fetch("/api/auth/session", {headers:{"Accept":"application/json"}, cache:"no-store"});
  if (sessionResponse.status === 401) {
    state.session = null;
    state.organization = null;
    renderAuthGate();
    return false;
  }
  const session = await sessionResponse.json().catch(() => ({}));
  if (!sessionResponse.ok) throw new Error(session.error || "无法验证登录状态");
  state.session = session;
  state.organization = session.organization || null;
  state.actorId = session.user?.id || state.actorId;
  if (state.actorId) localStorage.setItem("dbguard_actor", state.actorId);
  hideAuthGate();
  return true;
}

async function loadBase() {
  const [users, apps, dashboard, changesRaw, policies, audits, config] = await Promise.all([
    api("/api/users"), api("/api/apps"), api("/api/dashboard"),
    api("/api/changes?page=1&page_size=100"),
    api("/api/policies"), api("/api/audits?limit=100"), api("/api/config/status")
  ]);
  const changes = Array.isArray(changesRaw) ? changesRaw : (changesRaw?.items || []);
  Object.assign(state, {users, apps, dashboard, changes, policies, audits, config});
  if (!users.some(user => user.id === state.actorId)) state.actorId = users[0]?.id || "usr_developer";
  renderActor();
  renderNav();
  updateNotificationBadge();
}
function renderActor() {
  const select = document.querySelector("#actorSelect");
  const selectWrap = document.querySelector("#actorSelectWrap");
  const userChip = document.querySelector("#userChip");
  const current = actor() || { name: "—", role: "—", id: "" };
  if (select) {
    select.innerHTML = (state.users || []).map(user =>
      `<option value="${user.id}" ${user.id === state.actorId ? "selected" : ""}>${escapeHTML(user.name)} · ${escapeHTML(roleLabel(user.role))}</option>`
    ).join("");
  }
  const av = initials(current.name);
  document.querySelector("#headerAvatar") && (document.querySelector("#headerAvatar").textContent = av);
  document.querySelector("#userChipAvatar") && (document.querySelector("#userChipAvatar").textContent = av);
  document.querySelector("#userChipName") && (document.querySelector("#userChipName").textContent = current.name || "—");
  document.querySelector("#userChipRole") && (document.querySelector("#userChipRole").textContent = roleLabel(current.role) || current.role || "—");

  // 已登录：展示只读身份卡片，隐藏演示用「切换成员」下拉（避免 disabled 看起来像点不动）
  const authenticated = Boolean(state.authStatus?.enabled && state.session?.user);
  selectWrap?.classList.toggle("is-hidden", authenticated);
  userChip?.classList.toggle("is-hidden", !authenticated);
  if (select) {
    select.disabled = authenticated;
    select.title = authenticated ? "已登录企业账号，身份不可切换" : "演示模式可切换成员";
  }
  document.querySelector("#logoutButton")?.classList.toggle("is-hidden", !authenticated);
  updateNotificationBadge();

  const workspaceName = document.querySelector("#workspaceName");
  const workspaceMode = document.querySelector("#workspaceMode");
  if (workspaceName) workspaceName.textContent = state.organization?.name || current.organization_name || "当前企业";
  if (workspaceMode) workspaceMode.textContent = authenticated ? "已登录" : "演示模式";
  const appSelect = document.querySelector("#applicationSelect");
  if (appSelect) {
    appSelect.innerHTML = (state.apps || []).map(app =>
      `<option value="${app.id}">${escapeHTML(app.name)} · ${escapeHTML(app.runtime || app.database || app.kind || "服务")}</option>`
    ).join("");
  }
}

function pendingNotifyItems() {
  const changes = state.changes || [];
  const me = actor()?.id;
  const waiting = changes.filter(item => item.status === "WAITING_APPROVAL");
  const failed = changes.filter(item => item.status === "CHECK_FAILED").slice(0, 5);
  const ready = changes.filter(item => item.status === "READY_FOR_EXPERIMENT" && item.submitter_id === me).slice(0, 5);
  const openFindings = changes.flatMap(change =>
    (change.findings || [])
      .filter(f => f.status !== "VERIFIED")
      .map(f => ({ ...f, change_id: change.id, change_title: change.title }))
  ).slice(0, 8);
  return { waiting, failed, ready, openFindings };
}

function updateNotificationBadge() {
  const { waiting, failed, ready, openFindings } = pendingNotifyItems();
  const count = waiting.length + Math.min(failed.length, 3) + Math.min(ready.length, 3) + Math.min(openFindings.length, 3);
  const dot = document.querySelector("#notificationDot");
  if (dot) {
    dot.hidden = count === 0;
    dot.title = count ? `${count} 条待关注` : "";
  }
}

function closeNotifyPanel() {
  const panel = document.querySelector("#notifyPanel");
  const btn = document.querySelector("#notificationButton");
  if (panel) panel.hidden = true;
  if (btn) btn.setAttribute("aria-expanded", "false");
}

function toggleNotifyPanel() {
  const panel = document.querySelector("#notifyPanel");
  const btn = document.querySelector("#notificationButton");
  if (!panel) return;
  const open = panel.hidden;
  panel.hidden = !open;
  btn?.setAttribute("aria-expanded", open ? "true" : "false");
  if (open) renderNotifyPanel();
}

function renderNotifyPanel() {
  const body = document.querySelector("#notifyPanelBody");
  if (!body) return;
  const { waiting, failed, ready, openFindings } = pendingNotifyItems();
  const blocks = [];
  if (waiting.length) {
    blocks.push(`<div class="notify-section"><h4>待审批（${waiting.length}）</h4>
      ${waiting.slice(0, 6).map(item =>
        `<button type="button" class="notify-item" data-open-change="${item.id}">
          <strong>${escapeHTML(item.title)}</strong>
          <small>${escapeHTML(item.application_name || "")} · ${escapeHTML(item.submitter_name || "")}</small>
        </button>`
      ).join("")}
      <button type="button" class="button button-text button-small" data-route="approvals">去审批队列 →</button>
    </div>`);
  }
  if (ready.length) {
    blocks.push(`<div class="notify-section"><h4>待预发布验证（${ready.length}）</h4>
      ${ready.map(item =>
        `<button type="button" class="notify-item" data-open-change="${item.id}">
          <strong>${escapeHTML(item.title)}</strong>
          <small>规则已通过 · 可开始验证</small>
        </button>`
      ).join("")}
    </div>`);
  }
  if (failed.length) {
    blocks.push(`<div class="notify-section"><h4>规则阻断（${failed.length}）</h4>
      ${failed.map(item =>
        `<button type="button" class="notify-item" data-open-change="${item.id}">
          <strong>${escapeHTML(item.title)}</strong>
          <small>CHECK_FAILED · 需修改后重提</small>
        </button>`
      ).join("")}
    </div>`);
  }
  if (openFindings.length) {
    blocks.push(`<div class="notify-section"><h4>未闭环风险</h4>
      ${openFindings.map(item =>
        `<button type="button" class="notify-item" data-open-change="${item.change_id}">
          <strong>${escapeHTML(item.title)}</strong>
          <small>${escapeHTML(item.change_title || item.change_id)}</small>
        </button>`
      ).join("")}
      <button type="button" class="button button-text button-small" data-route="risks">风险事项 →</button>
    </div>`);
  }
  if (!blocks.length) {
    body.innerHTML = `<div class="notify-empty">暂无待办。规则阻断、待验证、待审批和未闭环风险会出现在这里。</div>`;
    return;
  }
  body.innerHTML = blocks.join("");
}
function currentRoute() { return (location.hash.replace(/^#\/?/, "") || "dashboard").split("/"); }
function renderNav() {
  const route = currentRoute()[0];
  document.querySelector("#navList").innerHTML = navItems.map(group => `
    <div class="nav-group-label">${group.group}</div>
    ${group.items.map(item => `<button class="nav-item ${route === item.route ? "active" : ""}" data-route="${item.route}">
      ${svg(item.icon)}<span>${item.label}</span>${item.count && item.count() ? `<b class="nav-count">${item.count()}</b>` : ""}
    </button>`).join("")}
  `).join("");
}
function setHeader(title, breadcrumb = title) {
  document.querySelector("#pageTitle").textContent = title;
  document.querySelector("#breadcrumb").textContent = breadcrumb;
  document.title = title + " · ChangeGuard";
}
function pageHeading(title, description, actions = "") {
  return `<div class="page-heading"><div><h2>${title}</h2><p>${description}</p></div><div class="page-heading-actions">${actions}</div></div>`;
}

async function renderPage() {
  const [route, id] = currentRoute();
  document.body.classList.toggle("panorama-mode", route === "panorama");
  renderNav();
  const main = document.querySelector("#mainContent");
  window.scrollTo({top:0,behavior:"instant"});
  if (route === "panorama") return renderPanorama(main);
  if (route === "dashboard") return renderDashboard(main);
  if (route === "assets" && id) return renderAssetDetail(main, id);
  if (route === "assets") return renderAssets(main);
  if (route === "risks") return renderRiskCenter(main);
  if (route === "changes" && id) return renderChangeDetail(main, id);
  if (route === "changes") return renderChanges(main);
  if (route === "calendar") return renderCalendar(main);
  if (route === "experiments") return renderExperiments(main);
  if (route === "approvals") return renderApprovals(main);
  if (route === "observability") return renderObservability(main);
  if (route === "incidents") return renderIncidentBacktrace(main);
  if (route === "policies") return renderPolicies(main);
  if (route === "enterprise") return renderEnterprise(main);
  if (route === "apps") return renderApps(main);
  if (route === "audits") return renderAudits(main);
  if (route === "settings") return renderSettings(main);
  location.hash = "#/dashboard";
}

function panoramaPanel(title, className, content) {
  return `<article class="panorama-panel ${className}"><span class="panorama-corner c-tl" aria-hidden="true"></span><span class="panorama-corner c-tr" aria-hidden="true"></span><span class="panorama-corner c-bl" aria-hidden="true"></span><span class="panorama-corner c-br" aria-hidden="true"></span><span class="panorama-panel-scan" aria-hidden="true"></span><header><i></i><h3>${title}</h3><span class="panorama-panel-signal" aria-hidden="true"></span></header><div class="panorama-panel-body">${content}</div></article>`;
}

function panoramaRing(value, total, label, tone = "cyan") {
  const safeTotal = Math.max(1, Number(total) || 0);
  const ratio = Math.min(100, Math.max(0, (Number(value) || 0) / safeTotal * 100));
  return `<div class="panorama-ring panorama-ring-${tone}" style="--ring-value:${ratio.toFixed(1)}%">
    <div><strong>${Number(value) || 0}</strong><span>${label}</span></div>
  </div>`;
}

function panoramaProgress(label, value, max, tone = "cyan") {
  const width = Math.max(4, Math.min(100, max ? value / max * 100 : 0));
  return `<div class="panorama-progress">
    <div><span>${escapeHTML(label)}</span><strong>${value}</strong></div>
    <div class="panorama-progress-track"><i class="tone-${tone}" style="width:${width.toFixed(1)}%"></i></div>
  </div>`;
}

function panoramaNode(key, label, value, route, iconName) {
  return `<button class="panorama-node panorama-node-${key}" data-route="${route}" aria-label="查看${escapeHTML(label)}">
    <span class="panorama-node-value">${value}</span>
    <span class="panorama-node-icon">${svg(iconName)}</span>
    <span class="panorama-node-label">${escapeHTML(label)}</span>
  </button>`;
}

function panoramaSnapshot() {
  const changes = Array.isArray(state.changes) ? state.changes : [];
  const apps = Array.isArray(state.apps) ? state.apps : [];
  const users = Array.isArray(state.users) ? state.users : [];
  const policies = Array.isArray(state.policies) ? state.policies : [];
  const audits = Array.isArray(state.audits) ? state.audits : [];
  const findings = changes.flatMap(change => Array.isArray(change.findings) ? change.findings : []);
  const risks = {LOW:0, MEDIUM:0, HIGH:0, UNKNOWN:0};
  changes.forEach(change => { risks[change.risk] = (risks[change.risk] || 0) + 1; });

  const applicationCounts = new Map();
  changes.forEach(change => {
    const key = change.application_id || change.application_name || "未归属应用";
    applicationCounts.set(key, (applicationCounts.get(key) || 0) + 1);
  });
  const appRanking = apps.map(app => ({
    name: app.name || app.code || "未命名应用",
    count: applicationCounts.get(app.id) || applicationCounts.get(app.name) || 0
  })).sort((left, right) => right.count - left.count || left.name.localeCompare(right.name, "zh-CN")).slice(0, 5);
  if (!appRanking.length && applicationCounts.size) {
    applicationCounts.forEach((count, name) => appRanking.push({name, count}));
    appRanking.sort((left, right) => right.count - left.count).splice(5);
  }

  const ruleMap = new Map();
  findings.forEach(finding => {
    const code = finding.code || "UNCLASSIFIED";
    const current = ruleMap.get(code) || {code, title:finding.title || code, count:0, severity:finding.severity || "UNKNOWN"};
    current.count += 1;
    ruleMap.set(code, current);
  });
  const topRules = [...ruleMap.values()].sort((left, right) => right.count - left.count).slice(0, 6);

  const flow = [
    {label:"草稿", statuses:["DRAFT"]},
    {label:"检查与整改", statuses:["CHECKING","CHECK_FAILED","READY_FOR_EXPERIMENT"]},
    {label:"预发布验证", statuses:["EXPERIMENT_QUEUED","EXPERIMENT_RUNNING"]},
    {label:"等待审批", statuses:["WAITING_APPROVAL"]},
    {label:"已闭环", statuses:["APPROVED","COMPLETED","REJECTED"]}
  ].map(item => ({...item, count:changes.filter(change => item.statuses.includes(change.status)).length}));

  return {
    changes, apps, users, policies, audits, findings, risks, appRanking, topRules, flow,
    highRisk: changes.filter(change => change.risk === "HIGH").length,
    experiments: changes.filter(change => change.experiment).length,
    pending: changes.filter(change => change.status === "WAITING_APPROVAL").length,
    closed: changes.filter(change => ["APPROVED","COMPLETED"].includes(change.status)).length,
    enabledPolicies: policies.filter(policy => policy.enabled !== false).length
  };
}

function renderPanorama(main) {
  setHeader("治理全景");
  const snapshot = panoramaSnapshot();
  const totalChanges = snapshot.changes.length;
  const riskTotal = Object.values(snapshot.risks).reduce((sum, value) => sum + value, 0);
  const maxApplication = Math.max(1, ...snapshot.appRanking.map(item => item.count));
  const maxFlow = Math.max(1, ...snapshot.flow.map(item => item.count));
  const now = new Intl.DateTimeFormat("zh-CN", {year:"numeric",month:"2-digit",day:"2-digit",hour:"2-digit",minute:"2-digit",second:"2-digit",hour12:false}).format(new Date()).replaceAll("/", "-");
  const topRuleMax = Math.max(1, snapshot.topRules[0]?.count || 1);
  const highRatio = totalChanges ? snapshot.highRisk / totalChanges : 0;
  const threatLevel = highRatio >= 0.35 || snapshot.highRisk >= 5 ? "CRITICAL" : highRatio >= 0.15 || snapshot.highRisk >= 2 ? "ELEVATED" : snapshot.pending >= 3 ? "WATCH" : "NOMINAL";
  const threatLabel = {CRITICAL:"危急", ELEVATED:"升高", WATCH:"关注", NOMINAL:"平稳"}[threatLevel];
  const closureRate = totalChanges ? Math.round(snapshot.closed / totalChanges * 100) : 0;
  const ruleCloud = snapshot.topRules.length ? snapshot.topRules.map((rule, index) => {
    const severity = String(rule.severity || "unknown").toLowerCase();
    const width = Math.max(8, Math.min(100, rule.count / topRuleMax * 100));
    return `<button class="panorama-rule-row risk-${severity}" data-route="risks" title="${escapeHTML(rule.title)}">
      <em>${String(index + 1).padStart(2, "0")}</em>
      <div class="panorama-rule-meta">
        <span>${escapeHTML(rule.title)}</span>
        <i><b style="width:${width.toFixed(1)}%"></b></i>
      </div>
      <strong>${rule.count}</strong>
    </button>`;
  }).join("") : `<div class="panorama-empty"><strong>暂无规则命中</strong><span>提交变更后会出现在这里</span></div>`;

  // 星尘 + 远景光点
  const stars = Array.from({length: 56}, (_, i) => {
    const left = ((i * 37) % 97) + 1.5;
    const top = ((i * 53) % 91) + 2;
    const delay = ((i * 0.37) % 6).toFixed(2);
    const size = 1 + (i % 3);
    return `<i class="panorama-star" style="left:${left}%;top:${top}%;width:${size}px;height:${size}px;animation-delay:-${delay}s"></i>`;
  }).join("");
  const rain = Array.from({length: 18}, (_, i) => {
    const left = ((i * 17) % 96) + 2;
    const delay = ((i * 0.7) % 8).toFixed(2);
    const dur = (6 + (i % 5)).toFixed(1);
    return `<span class="panorama-rain-col" style="left:${left}%;animation-duration:${dur}s;animation-delay:-${delay}s"></span>`;
  }).join("");
  const highPct = Math.round(highRatio * 100);
  const midPct = totalChanges ? Math.round((snapshot.risks.MEDIUM || 0) / totalChanges * 100) : 0;
  const lowPct = totalChanges ? Math.round((snapshot.risks.LOW || 0) / totalChanges * 100) : 0;

  main.innerHTML = `<section class="panorama-screen panorama-screen-v2 panorama-screen-v3">
    <div class="panorama-aurora" aria-hidden="true"></div>
    <div class="panorama-nebula" aria-hidden="true"></div>
    <div class="panorama-stars" aria-hidden="true">${stars}</div>
    <div class="panorama-rain" aria-hidden="true">${rain}</div>
    <div class="panorama-scanline" aria-hidden="true"></div>
    <div class="panorama-scanline panorama-scanline-2" aria-hidden="true"></div>
    <div class="panorama-scanline panorama-scanline-3" aria-hidden="true"></div>
    <div class="panorama-vignette" aria-hidden="true"></div>
    <div class="panorama-noise" aria-hidden="true"></div>
    <div class="panorama-frame-edge pe-t" aria-hidden="true"></div>
    <div class="panorama-frame-edge pe-b" aria-hidden="true"></div>
    <div class="panorama-frame-edge pe-l" aria-hidden="true"></div>
    <div class="panorama-frame-edge pe-r" aria-hidden="true"></div>
    <div class="panorama-hud-bracket hb-tl" aria-hidden="true"></div>
    <div class="panorama-hud-bracket hb-tr" aria-hidden="true"></div>
    <div class="panorama-hud-bracket hb-bl" aria-hidden="true"></div>
    <div class="panorama-hud-bracket hb-br" aria-hidden="true"></div>
    <div class="panorama-hud-tag hud-tl" aria-hidden="true">SEC·GRID // ONLINE</div>
    <div class="panorama-hud-tag hud-tr" aria-hidden="true">CHN·NODE // CG-01</div>
    <div class="panorama-hud-tag hud-bl" aria-hidden="true">LAT ${Math.max(12, 28 - snapshot.pending)}ms</div>
    <div class="panorama-hud-tag hud-br" aria-hidden="true">THR ${threatLevel}</div>
    <header class="panorama-header">
      <div class="panorama-header-left">
        <time id="panoramaLiveClock" datetime="${escapeHTML(now)}">${escapeHTML(now)}</time>
        <span class="panorama-live-pill"><i></i>LIVE · 实时态势</span>
        <span class="panorama-sys-chip">SYS·OK</span>
      </div>
      <div class="panorama-title">
        <span></span>
        <div class="panorama-title-stack">
          <small>CHANGEGUARD · GOVERNANCE COMMAND DECK</small>
          <h1>治理全景</h1>
          <em class="panorama-title-sub">风险链路 · 审批中枢 · 闭环遥测</em>
        </div>
        <span></span>
      </div>
      <div class="panorama-header-right">
        <span class="panorama-threat threat-${threatLevel.toLowerCase()}" title="综合高风险占比与待审量"><em>威胁等级</em><b>${threatLabel}</b></span>
        <button data-route="dashboard">返回工作台</button>
      </div>
    </header>
    <div class="panorama-ticker" aria-hidden="true">
      <div class="panorama-ticker-track">
        <span>高风险变更 ${snapshot.highRisk}</span><i></i>
        <span>待审批 ${snapshot.pending}</span><i></i>
        <span>验证记录 ${snapshot.experiments}</span><i></i>
        <span>闭环率 ${closureRate}%</span><i></i>
        <span>启用规则 ${snapshot.enabledPolicies}/${snapshot.policies.length}</span><i></i>
        <span>风险项 ${snapshot.findings.length}</span><i></i>
        <span>纳管服务 ${snapshot.apps.length}</span><i></i>
        <span>高风险变更 ${snapshot.highRisk}</span><i></i>
        <span>待审批 ${snapshot.pending}</span><i></i>
        <span>验证记录 ${snapshot.experiments}</span><i></i>
        <span>闭环率 ${closureRate}%</span><i></i>
        <span>启用规则 ${snapshot.enabledPolicies}/${snapshot.policies.length}</span><i></i>
      </div>
    </div>
    <div class="panorama-telemetry" aria-hidden="true">
      <div class="panorama-tele-item"><span>HIGH</span><b style="--v:${highPct}%">${snapshot.risks.HIGH || 0}</b><i></i></div>
      <div class="panorama-tele-item"><span>MED</span><b style="--v:${midPct}%">${snapshot.risks.MEDIUM || 0}</b><i></i></div>
      <div class="panorama-tele-item"><span>LOW</span><b style="--v:${lowPct}%">${snapshot.risks.LOW || 0}</b><i></i></div>
      <div class="panorama-tele-item"><span>PEND</span><b style="--v:${Math.min(100, snapshot.pending * 12)}%">${snapshot.pending}</b><i></i></div>
      <div class="panorama-tele-item"><span>CLOSE</span><b style="--v:${closureRate}%">${closureRate}%</b><i></i></div>
    </div>
    <div class="panorama-layout">
      <aside class="panorama-column panorama-column-left">
        ${panoramaPanel("资产态势", "panel-assets", `<div class="panorama-assets">
          ${panoramaRing(snapshot.highRisk, Math.max(1, totalChanges), "高风险", "danger")}
          <div class="panorama-asset-list">
            <button data-route="assets"><i>${svg("apps")}</i><span><b>${snapshot.apps.length}</b>纳管服务</span></button>
            <button data-route="changes"><i>${svg("code")}</i><span><b>${totalChanges}</b>变更单</span></button>
            <button data-route="policies"><i>${svg("shield")}</i><span><b>${snapshot.policies.length}</b>治理规则</span></button>
          </div></div>`)}
        ${panoramaPanel("服务热力", "panel-ranking", snapshot.appRanking.length ? `<div class="panorama-ranking">${snapshot.appRanking.map(item => panoramaProgress(item.name, item.count, maxApplication)).join("")}</div>` : `<div class="panorama-empty"><strong>暂无数据</strong><span>登记服务与变更后显示</span></div>`)}
        ${panoramaPanel("风险光谱", "panel-risk", `<div class="panorama-risk-summary">
          ${panoramaRing(riskTotal, riskTotal, "变更总量")}
          <div class="panorama-risk-legend">
            <span class="high"><i></i>高危通道<b>${snapshot.risks.HIGH || 0}</b></span>
            <span class="medium"><i></i>中危通道<b>${snapshot.risks.MEDIUM || 0}</b></span>
            <span class="low"><i></i>低危通道<b>${snapshot.risks.LOW || 0}</b></span>
            <span class="unknown"><i></i>待标定<b>${snapshot.risks.UNKNOWN || 0}</b></span>
          </div>
          <div class="panorama-spectrum" aria-hidden="true">
            <i class="sp-h" style="flex:${Math.max(1, snapshot.risks.HIGH || 0)}"></i>
            <i class="sp-m" style="flex:${Math.max(1, snapshot.risks.MEDIUM || 0)}"></i>
            <i class="sp-l" style="flex:${Math.max(1, snapshot.risks.LOW || 0)}"></i>
            <i class="sp-u" style="flex:${Math.max(1, snapshot.risks.UNKNOWN || 0)}"></i>
          </div>
          </div>`)}
      </aside>

      <section class="panorama-topology" aria-label="治理中枢拓扑">
        <div class="panorama-grid-glow"></div>
        <div class="panorama-floor" aria-hidden="true"></div>
        <div class="panorama-radar" aria-hidden="true"><span></span><b></b></div>
        <div class="panorama-orbit panorama-orbit-a" aria-hidden="true"></div>
        <div class="panorama-orbit panorama-orbit-b" aria-hidden="true"></div>
        <div class="panorama-orbit panorama-orbit-c" aria-hidden="true"></div>
        <div class="panorama-orbit panorama-orbit-d" aria-hidden="true"></div>
        <div class="panorama-link-pulses" aria-hidden="true"></div>
        <div class="panorama-beam panorama-beam-h" aria-hidden="true"></div>
        <div class="panorama-beam panorama-beam-v" aria-hidden="true"></div>
        <div class="panorama-beam panorama-beam-d1" aria-hidden="true"></div>
        <div class="panorama-beam panorama-beam-d2" aria-hidden="true"></div>
        <svg class="panorama-links" viewBox="0 0 900 650" preserveAspectRatio="none" aria-hidden="true">
          <defs>
            <linearGradient id="lineGlow" x1="0" x2="1"><stop stop-color="#1a9dff"/><stop offset=".5" stop-color="#7cf6ff"/><stop offset="1" stop-color="#2ce7ff"/></linearGradient>
            <linearGradient id="lineHot" x1="0" y1="0" x2="1" y2="1"><stop stop-color="#ff5b6e"/><stop offset="1" stop-color="#2ce7ff"/></linearGradient>
            <filter id="lineSoft"><feGaussianBlur stdDeviation="1.6" result="b"/><feMerge><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge></filter>
            <radialGradient id="coreGlow" cx="50%" cy="50%" r="50%"><stop stop-color="rgba(44,231,255,.65)"/><stop offset="1" stop-color="rgba(44,231,255,0)"/></radialGradient>
            <filter id="glowSoft"><feGaussianBlur stdDeviation="3" result="coloredBlur"/><feMerge><feMergeNode in="coloredBlur"/><feMergeNode in="SourceGraphic"/></feMerge></filter>
          </defs>
          <circle cx="450" cy="325" r="118" fill="url(#coreGlow)" opacity=".55">
            <animate attributeName="r" values="100;135;100" dur="4.2s" repeatCount="indefinite"/>
            <animate attributeName="opacity" values=".3;.7;.3" dur="4.2s" repeatCount="indefinite"/>
          </circle>
          <circle cx="450" cy="325" r="160" fill="none" stroke="rgba(44,231,255,.12)" stroke-dasharray="4 10">
            <animateTransform attributeName="transform" type="rotate" from="0 450 325" to="360 450 325" dur="40s" repeatCount="indefinite"/>
          </circle>
          <path class="panorama-link-path" filter="url(#lineSoft)" d="M450 325C450 220 450 140 450 82"/>
          <path class="panorama-link-path" filter="url(#lineSoft)" d="M450 325C360 250 250 190 185 155"/>
          <path class="panorama-link-path" filter="url(#lineSoft)" d="M450 325C540 250 650 190 715 155"/>
          <path class="panorama-link-path" filter="url(#lineSoft)" d="M450 325C320 325 200 325 120 325"/>
          <path class="panorama-link-path" filter="url(#lineSoft)" d="M450 325C580 325 700 325 780 325"/>
          <path class="panorama-link-path" filter="url(#lineSoft)" d="M450 325C360 410 260 490 205 535"/>
          <path class="panorama-link-path" filter="url(#lineSoft)" d="M450 325C540 410 640 490 695 535"/>
          <path class="panorama-link-path" filter="url(#lineSoft)" d="M450 325C450 430 450 520 450 590"/>
          <circle r="3.6" fill="#7cf6ff" filter="url(#glowSoft)"><animateMotion dur="2.6s" repeatCount="indefinite" path="M450 325C450 220 450 140 450 82"/></circle>
          <circle r="3.6" fill="#2ce7ff" filter="url(#glowSoft)"><animateMotion dur="3.0s" repeatCount="indefinite" path="M450 325C360 250 250 190 185 155" begin="-.4s"/></circle>
          <circle r="3.6" fill="#9b7bff" filter="url(#glowSoft)"><animateMotion dur="2.9s" repeatCount="indefinite" path="M450 325C540 250 650 190 715 155" begin="-.8s"/></circle>
          <circle r="3.6" fill="#2ce7ff" filter="url(#glowSoft)"><animateMotion dur="2.4s" repeatCount="indefinite" path="M450 325C320 325 200 325 120 325" begin="-1.1s"/></circle>
          <circle r="3.6" fill="#ffad42" filter="url(#glowSoft)"><animateMotion dur="2.5s" repeatCount="indefinite" path="M450 325C580 325 700 325 780 325" begin="-1.5s"/></circle>
          <circle r="3.6" fill="#ff5b6e" filter="url(#glowSoft)"><animateMotion dur="3.2s" repeatCount="indefinite" path="M450 325C360 410 260 490 205 535" begin="-.2s"/></circle>
          <circle r="3.6" fill="#22e4a7" filter="url(#glowSoft)"><animateMotion dur="3.1s" repeatCount="indefinite" path="M450 325C540 410 640 490 695 535" begin="-1.9s"/></circle>
          <circle r="3.6" fill="#7cf6ff" filter="url(#glowSoft)"><animateMotion dur="2.7s" repeatCount="indefinite" path="M450 325C450 430 450 520 450 590" begin="-1.3s"/></circle>
        </svg>
        ${panoramaNode("apps", "服务", snapshot.apps.length, "assets", "apps")}
        ${panoramaNode("users", "成员", snapshot.users.length, "enterprise", "users")}
        ${panoramaNode("policies", "规则", snapshot.policies.length, "policies", "shield")}
        ${panoramaNode("changes", "变更单", totalChanges, "changes", "code")}
        ${panoramaNode("approvals", "待审批", snapshot.pending, "approvals", "approval")}
        ${panoramaNode("findings", "风险项", snapshot.findings.length, "risks", "alert")}
        ${panoramaNode("experiments", "验证", snapshot.experiments, "experiments", "flask")}
        ${panoramaNode("reports", "已闭环", snapshot.closed, "audits", "file")}
        <button class="panorama-core" data-route="dashboard" aria-label="返回工作台">
          <span class="panorama-core-ring-spin" aria-hidden="true"></span>
          <span class="panorama-core-ring-spin panorama-core-ring-spin-2" aria-hidden="true"></span>
          <span class="panorama-core-halo"></span>
          <span class="panorama-core-halo panorama-core-halo-2"></span>
          <span class="panorama-core-halo panorama-core-halo-3"></span>
          <span class="panorama-core-disc"></span>
          <span class="panorama-core-energy" aria-hidden="true"></span>
          <span class="panorama-core-emblem">${svg("shield")}<b>CG</b></span>
          <strong>治理中枢</strong>
          <small>检查 · 验证 · 审批 · 审计</small>
          <em class="panorama-core-status"><i></i>链路在线 · SYNC</em>
        </button>
      </section>

      <aside class="panorama-column panorama-column-right">
        ${panoramaPanel("规则命中雷达", "panel-rules", `<div class="panorama-rule-content"><div class="panorama-rule-cloud">${ruleCloud}</div><button class="panorama-panel-entry" data-route="policies"><span>进入规则中心</span>${svg("arrow")}</button></div>`)}
        ${panoramaPanel("治理流水线", "panel-flow", `<div class="panorama-flow">${snapshot.flow.map((item, index) => `
          <button data-route="${index === 2 ? "experiments" : index === 3 ? "approvals" : "changes"}">
            <em>${String(index + 1).padStart(2, "0")}</em><span>${item.label}</span>
            <i><b style="width:${Math.max(5, item.count / maxFlow * 100).toFixed(1)}%"></b></i><strong>${item.count}</strong>
          </button>`).join("")}</div>`)}
        ${panoramaPanel("闭环仪表", "panel-closure", `<div class="panorama-closure">
          ${panoramaRing(snapshot.closed, Math.max(1, totalChanges), "已闭环", "success")}
          <div class="panorama-closure-list">
            <button data-route="policies"><span>启用规则</span><b>${snapshot.enabledPolicies}</b></button>
            <button data-route="experiments"><span>验证记录</span><b>${snapshot.experiments}</b></button>
            <button data-route="audits"><span>审计事件</span><b>${snapshot.audits.length}</b></button>
          </div></div>`)}
      </aside>
    </div>
    <footer class="panorama-footer">
      <span class="panorama-footer-brand">ChangeGuard</span>
      <i></i>
      <span>企业研发变更风险治理 · COMMAND DECK</span>
      <i></i>
      <span>闭环率 ${closureRate}% · 威胁 ${threatLabel} · 节点 ${8}</span>
    </footer>
  </section>`;

  // 实时时钟（不重绘整页）
  if (window.__panoramaClockTimer) clearInterval(window.__panoramaClockTimer);
  window.__panoramaClockTimer = setInterval(() => {
    const el = document.querySelector("#panoramaLiveClock");
    if (!el || !document.body.classList.contains("panorama-mode")) {
      clearInterval(window.__panoramaClockTimer);
      window.__panoramaClockTimer = null;
      return;
    }
    const t = new Intl.DateTimeFormat("zh-CN", {year:"numeric",month:"2-digit",day:"2-digit",hour:"2-digit",minute:"2-digit",second:"2-digit",hour12:false}).format(new Date()).replaceAll("/", "-");
    el.textContent = t;
    el.setAttribute("datetime", t);
  }, 1000);
}

const releaseTerminalStatuses = new Set(["REJECTED", "COMPLETED"]);
const releaseWindowMilliseconds = 90 * 60 * 1000;

function releaseDateKey(value) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return [date.getFullYear(), String(date.getMonth() + 1).padStart(2, "0"), String(date.getDate()).padStart(2, "0")].join("-");
}

function releaseArtifactLabels(change) {
  const labels = {CODE:"代码", CONFIG:"配置", KUBERNETES:"Kubernetes", API:"API", DATABASE:"数据库"};
  const kinds = [...new Set((change.artifacts || []).map(item => item.kind).filter(Boolean))];
  if (!kinds.length && change.sql) kinds.push("DATABASE");
  return kinds.map(kind => labels[kind] || kind);
}

function releaseWindowAnalysis(change) {
  const currentTime = new Date(change.planned_at).getTime();
  const currentApp = state.apps.find(app => app.id === change.application_id) || null;
  const dependencies = (currentApp?.dependencies || []).map(id => state.apps.find(app => app.id === id)).filter(Boolean);
  const downstream = state.apps.filter(app => (app.dependencies || []).includes(change.application_id));
  const sameService = [];
  const dependency = [];
  if (!currentApp || !Number.isFinite(currentTime) || releaseTerminalStatuses.has(change.status)) return {currentApp, dependencies, downstream, sameService, dependency, nearby:[]};
  const upstreamIds = new Set(currentApp.dependencies || []);
  const downstreamIds = new Set(downstream.map(app => app.id));
  for (const other of state.changes) {
    if (other.id === change.id || releaseTerminalStatuses.has(other.status)) continue;
    const otherTime = new Date(other.planned_at).getTime();
    if (!Number.isFinite(otherTime) || Math.abs(otherTime - currentTime) > releaseWindowMilliseconds) continue;
    if (other.application_id === change.application_id) sameService.push(other);
    else if (upstreamIds.has(other.application_id) || downstreamIds.has(other.application_id)) dependency.push(other);
  }
  const nearby = [...sameService.map(item => ({...item, relation:"同服务", level:"hard"})), ...dependency.map(item => ({...item, relation:upstreamIds.has(item.application_id) ? "上游" : "下游", level:"warning"}))].sort((left, right) => new Date(left.planned_at) - new Date(right.planned_at));
  return {currentApp, dependencies, downstream, sameService, dependency, nearby};
}

function buildReleaseSchedule() {
  return state.changes.filter(change => change.planned_at).map(change => {
    const analysis = releaseWindowAnalysis(change);
    const conflictLevel = analysis.sameService.length ? "hard" : analysis.dependency.length ? "warning" : "normal";
    return {change, analysis, conflictLevel, timestamp:new Date(change.planned_at).getTime(), dateKey:releaseDateKey(change.planned_at)};
  }).filter(item => Number.isFinite(item.timestamp)).sort((left, right) => left.timestamp - right.timestamp);
}

function releasePairCount(items, field) {
  const pairs = new Set();
  items.forEach(item => item.analysis[field].forEach(other => pairs.add([item.change.id, other.id].sort().join("|"))));
  return pairs.size;
}

function releaseDayLabel(dateKey) {
  const date = new Date(dateKey + "T00:00:00");
  const today = releaseDateKey(new Date());
  const tomorrow = releaseDateKey(new Date(Date.now() + 86400000));
  const prefix = dateKey === today ? "今天" : dateKey === tomorrow ? "明天" : new Intl.DateTimeFormat("zh-CN", {weekday:"short"}).format(date);
  return `${prefix} · ${dateKey.slice(5).replace("-", "月")}日`;
}

function renderReleaseSchedule(items) {
  if (!items.length) return `<div class="release-empty">${svg("file")}<strong>当前条件下没有发布安排</strong><span>调整筛选条件，或在统一变更中补充计划窗口。</span><button class="button button-secondary" data-route="changes">查看统一变更</button></div>`;
  const groups = new Map();
  items.forEach(item => { if (!groups.has(item.dateKey)) groups.set(item.dateKey, []); groups.get(item.dateKey).push(item); });
  return [...groups.entries()].map(([dateKey, dayItems]) => `<section class="release-day">
    <header class="release-day-head"><div><strong>${releaseDayLabel(dateKey)}</strong><span>${dayItems.length} 个发布安排</span></div><i></i></header>
    <div class="release-rail">${dayItems.map(item => { const change = item.change; const artifacts = releaseArtifactLabels(change); const time = new Intl.DateTimeFormat("zh-CN", {hour:"2-digit",minute:"2-digit",hour12:false}).format(new Date(change.planned_at)); const conflictText = item.conflictLevel === "hard" ? `同服务冲突 ${item.analysis.sameService.length} 项` : item.conflictLevel === "warning" ? `上下游重叠 ${item.analysis.dependency.length} 项` : "窗口正常"; return `<button class="release-card release-card-${item.conflictLevel}" data-open-change="${change.id}"><span class="release-card-time">${time}</span><span class="release-card-marker"></span><div class="release-card-body"><div class="release-card-top"><span>${escapeHTML(change.application_name)}</span><b class="release-conflict-label">${conflictText}</b></div><strong>${escapeHTML(change.title)}</strong><div class="release-card-meta">${statusBadge(change.status)}${riskBadge(change.risk)}</div><div class="release-card-foot"><span>${escapeHTML(change.submitter_name || "待分配")}</span><span>${escapeHTML(change.environment || "未配置")}</span></div>${artifacts.length ? `<div class="release-artifacts">${artifacts.map(label => `<i>${escapeHTML(label)}</i>`).join("")}</div>` : ""}</div></button>`; }).join("")}</div>
  </section>`).join("");
}

function renderCalendar(main) {
  setHeader("发布日历");
  const schedule = buildReleaseSchedule();
  const start = new Date(); start.setHours(0, 0, 0, 0);
  const end = new Date(start); end.setDate(end.getDate() + 7);
  const baseItems = schedule.filter(item => !releaseTerminalStatuses.has(item.change.status) && item.timestamp >= start.getTime() && item.timestamp < end.getTime());
  const serviceOptions = [...new Set(baseItems.map(item => item.change.application_id).filter(Boolean))].map(id => state.apps.find(app => app.id === id)).filter(Boolean);
  const environments = [...new Set(baseItems.map(item => item.change.environment).filter(Boolean))];
  const statuses = [...new Set(baseItems.map(item => item.change.status).filter(Boolean))];
  const hardConflicts = releasePairCount(baseItems, "sameService");
  const dependencyOverlaps = releasePairCount(baseItems, "dependency");
  const production = baseItems.filter(item => /生产|prod/i.test(item.change.environment || "")).length;
  main.innerHTML = pageHeading("发布日历", "把未来 7 天的服务发布、同服务撞车和上下游联动窗口放在一张计划表中。", `<button class="button button-secondary" data-route="changes">${svg("code")}查看统一变更</button><button class="button button-primary" data-create>${svg("plus")}登记发布计划</button>`) + `
    <section class="registry-stat-grid release-calendar-stats">
      <article><span>近 7 天发布</span><strong>${baseItems.length}</strong><small>包含草稿、验证、审批与待执行计划</small></article>
      <article class="success"><span>生产环境计划</span><strong>${production}</strong><small>需要发布策略、回滚和观察指标</small></article>
      <article class="danger"><span>同服务硬冲突</span><strong>${hardConflicts}</strong><small>90 分钟内重复安排会阻断提交</small></article>
      <article class="warning"><span>上下游窗口重叠</span><strong>${dependencyOverlaps}</strong><small>需确认兼容顺序和联合回滚</small></article>
    </section>
    <article class="panel release-calendar-panel">
      <div class="release-calendar-toolbar"><div class="filter-group"><select class="filter-select" id="calendarService"><option value="">全部服务</option>${serviceOptions.map(app => `<option value="${app.id}">${escapeHTML(app.name)}</option>`).join("")}</select><select class="filter-select" id="calendarEnvironment"><option value="">全部环境</option>${environments.map(value => `<option value="${escapeHTML(value)}">${escapeHTML(value)}</option>`).join("")}</select><select class="filter-select" id="calendarStatus"><option value="">全部状态</option>${statuses.map(value => `<option value="${value}">${statusInfo(value)[0]}</option>`).join("")}</select><select class="filter-select" id="calendarConflict"><option value="">全部窗口</option><option value="hard">仅同服务冲突</option><option value="warning">仅上下游重叠</option><option value="normal">仅正常窗口</option></select></div><div class="release-legend"><span><i class="normal"></i>正常</span><span><i class="warning"></i>上下游重叠</span><span><i class="hard"></i>同服务冲突</span></div></div>
      <div class="release-schedule" id="releaseSchedule">${renderReleaseSchedule(baseItems)}</div>
    </article>`;
  const paint = () => {
    const service = document.querySelector("#calendarService").value;
    const environment = document.querySelector("#calendarEnvironment").value;
    const status = document.querySelector("#calendarStatus").value;
    const conflict = document.querySelector("#calendarConflict").value;
    const filtered = baseItems.filter(item => (!service || item.change.application_id === service) && (!environment || item.change.environment === environment) && (!status || item.change.status === status) && (!conflict || item.conflictLevel === conflict));
    document.querySelector("#releaseSchedule").innerHTML = renderReleaseSchedule(filtered);
  };
  ["calendarService", "calendarEnvironment", "calendarStatus", "calendarConflict"].forEach(id => document.querySelector("#" + id)?.addEventListener("change", paint));
}

function renderImpactPanel(change) {
  const analysis = releaseWindowAnalysis(change);
  const nearby = analysis.nearby;
  const tone = analysis.sameService.length ? "hard" : analysis.dependency.length ? "warning" : "normal";
  const headline = tone === "hard" ? "存在同服务窗口冲突" : tone === "warning" ? "存在上下游联动窗口" : "当前窗口未发现直接冲突";
  const serviceNames = items => items.length ? items.map(item => `<span>${escapeHTML(item.name)}</span>`).join("") : `<em>暂无</em>`;
  return `<article class="panel impact-panel impact-${tone}"><header class="panel-header"><div><h3>影响范围与发布窗口</h3><p>依据服务依赖和 90 分钟发布保护窗口计算</p></div><button class="panel-link" data-route="calendar">查看日历 →</button></header><div class="panel-body"><div class="impact-headline"><span>${svg(tone === "hard" ? "alert" : tone === "warning" ? "activity" : "check")}</span><div><strong>${headline}</strong><p>${escapeHTML(analysis.currentApp?.name || change.application_name || "未关联服务")} · ${formatDate(change.planned_at)}</p></div></div><div class="impact-service-map"><div><b>直接上游</b><span>${serviceNames(analysis.dependencies)}</span></div><div><b>直接下游</b><span>${serviceNames(analysis.downstream)}</span></div></div><div class="impact-window-list">${nearby.length ? nearby.slice(0, 5).map(item => `<button data-open-change="${item.id}" class="impact-window-item impact-window-${item.level}"><span>${formatDate(item.planned_at)}</span><strong>${escapeHTML(item.application_name)} · ${escapeHTML(item.title)}</strong><small>${item.relation} · ${statusInfo(item.status)[0]}</small></button>`).join("") : `<div class="impact-window-empty">相邻 90 分钟内没有同服务或直接上下游发布。</div>`}</div></div></article>
  <article class="panel blast-panel" id="blastRadiusPanel" data-change-id="${escapeHTML(change.id)}">
    <header class="panel-header"><div><h3>爆炸半径分析</h3><p>震中服务 → 下游波及链 · 核心链路高亮</p></div><button type="button" class="panel-link" data-load-blast>计算半径</button></header>
    <div class="panel-body" id="blastRadiusBody"><div class="blast-placeholder">点击「计算半径」，基于服务依赖表 BFS 推演影响面（含负责人 / 历史事故次数）。</div></div>
  </article>
  <article class="panel efficacy-panel" id="efficacyPanel" data-change-id="${escapeHTML(change.id)}">
    <header class="panel-header"><div><h3>变更效果验证</h3><p>发布前后错误率 / P95（当前为可复现 mock，可接 Prometheus）</p></div><button type="button" class="panel-link" data-load-efficacy>拉取效果</button></header>
    <div class="panel-body" id="efficacyBody"><div class="blast-placeholder">发布完成后自动对比前后窗口指标，回答「这次到底好不好」。</div></div>
  </article>`;
}

function renderDashboardReleasePreview() {
  const now = Date.now();
  const upcoming = buildReleaseSchedule().filter(item => item.timestamp >= now - 60 * 60 * 1000 && !releaseTerminalStatuses.has(item.change.status)).slice(0, 5);
  const conflictCount = upcoming.filter(item => item.conflictLevel !== "normal").length;
  return `<article class="panel dashboard-release-preview"><header class="panel-header"><div><h3>近期发布窗口</h3><p>优先处理同服务撞车和上下游联动计划</p></div><button class="panel-link" data-route="calendar">打开发布日历 →</button></header><div class="dashboard-release-body"><div class="dashboard-release-summary"><strong>${upcoming.length}</strong><span>近期安排</span><b>${conflictCount} 项需协调</b></div><div class="dashboard-release-list">${upcoming.length ? upcoming.map(item => `<button data-open-change="${item.change.id}" class="dashboard-release-item dashboard-release-${item.conflictLevel}"><time>${formatDate(item.change.planned_at)}</time><span><strong>${escapeHTML(item.change.application_name)}</strong><small>${escapeHTML(item.change.title)}</small></span><i>${item.conflictLevel === "hard" ? "同服务冲突" : item.conflictLevel === "warning" ? "上下游重叠" : statusInfo(item.change.status)[0]}</i></button>`).join("") : `<div class="impact-window-empty">近期没有待执行发布计划。</div>`}</div></div></article>`;
}
function statCard(label, value, foot, iconName, accent, soft) {
  return `<article class="stat-card" style="--accent:${accent};--soft:${soft}">
    <div class="stat-top"><span class="stat-label">${label}</span><span class="stat-icon">${svg(iconName)}</span></div>
    <div class="stat-value">${value}</div><div class="stat-foot">${foot}</div>
  </article>`;
}
function renderDashboard(main) {
  setHeader("概览");
  const d = state.dashboard || {};
  const dist = d.risk_distribution || {};
  const total = Object.values(dist).reduce((sum, value) => sum + value, 0);
  const recent = d.recent_changes || [];
  const allChanges = state.changes || [];
  const totalChanges = allChanges.length;
  const ruleCoverage = totalChanges ? Math.round(allChanges.filter(item => (item.findings || []).length > 0).length / totalChanges * 100) : 0;
  const rollbackCoverage = totalChanges ? Math.round(allChanges.filter(item => item.rollback_plan || item.rollback_sql).length / totalChanges * 100) : 0;
  const approvalScope = allChanges.filter(item => ["WAITING_APPROVAL","APPROVED","REJECTED","COMPLETED"].includes(item.status));
  const approvalClosed = approvalScope.filter(item => ["APPROVED","REJECTED","COMPLETED"].includes(item.status)).length;
  const approvalClosure = approvalScope.length ? Math.round(approvalClosed / approvalScope.length * 100) : 0;
  const cfg = state.config || {};
  const llmReady = !!cfg.llm_configured;
  const canEditIntegrations = !!(actor()?.enterprise_admin || actor()?.role === "技术负责人");
  const setupBanner = !llmReady ? `
    <article class="setup-banner">
      <div class="setup-banner-main">
        <span class="setup-banner-icon">${svg("activity")}</span>
        <div>
          <strong>尚未接入 AI 分析</strong>
          <p>规则检查与审批流程可独立运行；接入 DeepSeek / OpenAI 兼容接口后，提交时会多一层只读辅助分析（不可替代审批）。</p>
        </div>
      </div>
      <div class="setup-banner-actions">
        ${canEditIntegrations
          ? `<button type="button" class="button button-primary" data-route="settings">去接入 AI</button>`
          : `<button type="button" class="button button-secondary" data-route="settings">查看集成说明</button>`}
        <button type="button" class="button button-secondary" data-create>先建变更单</button>
      </div>
    </article>` : "";
  main.innerHTML = pageHeading("概览", "当前企业的变更、验证与待审批情况。", `<button class="button button-secondary" data-refresh>${svg("refresh")}刷新</button>`) + `
    ${setupBanner}
    <section class="stats-grid">
      ${statCard("处理中变更", d.pending_count || 0, "<b>实时</b> 汇总全部待处理节点", "database", "#2458e6", "#eaf0ff")}
      ${statCard("验证通过率", pct(d.experiment_pass_rate), "基于已完成的预发布验证", "flask", "#16845b", "#eaf8f2")}
      ${statCard("高风险变更", d.high_risk_count || 0, "需负责人复核后才能批准", "alert", "#c43645", "#fff0f1")}
      ${statCard("平均验证耗时", (d.average_experiment_sec || 0).toFixed(1) + "s", "包含制品检查、回滚与证据采集", "activity", "#7357c9", "#f2efff")}
    </section>
    <section class="content-grid">
      <article class="panel dashboard-changes-panel">
      <header class="panel-header"><div><h3>近期变更</h3><p>最近登记和推进的统一研发变更</p></div><button class="panel-link" data-route="changes">查看全部 →</button></header>
        <div class="table-wrap">${changeTable(recent, true)}</div>
      </article>
      <article class="panel">
        <header class="panel-header"><div><h3>风险分布</h3><p>按当前证据结论统计</p></div></header>
        <div class="panel-body">
          <div class="risk-summary"><div class="risk-ring"><div class="risk-ring-center"><strong>${total}</strong><span>变更总数</span></div></div>
            <div class="risk-legend">
              <div class="risk-legend-row"><span><i style="background:#2458e6"></i>低风险</span><b>${dist.LOW || 0}</b></div>
              <div class="risk-legend-row"><span><i style="background:#e4aa54"></i>中风险</span><b>${dist.MEDIUM || 0}</b></div>
              <div class="risk-legend-row"><span><i style="background:#df5b68"></i>高风险</span><b>${dist.HIGH || 0}</b></div>
            </div>
          </div>
          <div class="metric-bars">
            ${[["规则检查覆盖",ruleCoverage,"#2458e6"],["回滚方案完整",rollbackCoverage,"#16845b"],["审批闭环率",approvalClosure,"#7357c9"]].map(x => `<div><div class="metric-bar-head"><span>${x[0]}</span><b>${x[1]}%</b></div><div class="metric-bar-track"><div class="metric-bar-fill" style="width:${x[1]}%;--bar:${x[2]}"></div></div></div>`).join("")}
          </div>
        </div>
      </article>
    </section>
    ${renderDashboardReleasePreview()}
    <article class="panel flow-panel">
      <header class="panel-header"><div><h3>标准变更链路</h3><p>每一步都有结构化证据和责任人记录</p></div><span class="status ${totalChanges ? "status-approved" : "status-draft"}"><i></i>${totalChanges ? `已形成 ${totalChanges} 条记录` : "等待首条变更"}</span></header>
      <div class="panel-body"><div class="flow-line">
        ${[["file","登记变更","代码 / 配置 / K8s / API / SQL"],["shield","规则检查","确定性多制品规则"],["flask","预发布验证","回滚与专项验证"],["activity","证据分析","只读工具调用"],["approval","发布审批","四眼原则"]].map((item,index) => `<div class="flow-item ${index < 4 ? "done" : "current"}"><div class="flow-icon">${index < 4 ? svg("check") : svg(item[0])}</div><strong>${item[1]}</strong><span>${item[2]}</span></div>`).join("")}
      </div></div>
    </article>
  `;
}

function changeTable(items, compact = false) {
  if (!items.length) return emptyState("暂无变更记录", "新建变更单后，系统会在这里展示完整治理进度。");
  return `<table class="data-table"><thead><tr><th>变更内容</th><th>服务</th><th>风险</th><th>当前状态</th><th>提交人</th><th>更新时间</th><th></th></tr></thead>
    <tbody>${items.map(item => `<tr data-open-change="${item.id}">
      <td><div class="change-title"><span class="type-icon">${svg(item.change_type === "DML" ? "code" : "database")}</span><div><strong>${escapeHTML(item.title)}</strong><span>${escapeHTML(item.id)}</span></div></div></td>
      <td>${escapeHTML(item.application_name)}</td><td>${riskBadge(item.risk)}</td><td>${statusBadge(item.status)}</td>
      <td><div class="person-cell"><span class="avatar">${escapeHTML(initials(item.submitter_name))}</span><span>${escapeHTML(item.submitter_name)}</span></div></td>
      <td>${formatDate(item.updated_at)}</td><td><button class="icon-button">${svg("arrow")}</button></td>
    </tr>`).join("")}</tbody></table>`;
}
function emptyState(title, description) {
  return `<div class="empty-state"><div class="empty-state-icon">${svg("file")}</div><h3>${title}</h3><p>${description}</p><button class="button button-primary" data-create>${svg("plus")}新建变更</button></div>`;
}
async function renderChanges(main) {
  setHeader("变更单");
  if (!state.changeListPage) state.changeListPage = 1;
  const pageSize = 15;
  const seedKeyword = state.pendingChangeFilter || "";
  if (state.pendingChangeFilter) state.pendingChangeFilter = "";
  main.innerHTML = pageHeading("变更单", "提交、检查、验证到审批的记录列表。", `<button class="button button-primary" type="button" data-create>${svg("plus")}新建变更</button>`) + `
    <article class="panel">
      <div class="toolbar">
      <div class="filter-group"><input class="filter-input" id="changeFilter" placeholder="标题 / 编号 / 服务" value="${escapeHTML(seedKeyword)}"><select class="filter-select" id="statusFilter"><option value="">全部状态</option>${[...new Set((state.changes || []).map(x=>x.status))].map(status => `<option value="${status}">${statusInfo(status)[0]}</option>`).join("")}</select><select class="filter-select" id="riskFilter"><option value="">全部风险</option><option value="LOW">低</option><option value="MEDIUM">中</option><option value="HIGH">高</option></select></div>
        <span class="result-count" id="resultCount">加载中…</span>
      </div>
      <div class="table-wrap" id="changeTable"><div class="page-loading"><div class="skeleton skeleton-panel"></div></div></div>
      <div class="pager" id="changePager"></div>
    </article>`;

  const loadPage = async () => {
    const keyword = document.querySelector("#changeFilter")?.value.trim() || "";
    const status = document.querySelector("#statusFilter")?.value || "";
    const risk = document.querySelector("#riskFilter")?.value || "";
    const q = new URLSearchParams({
      page: String(state.changeListPage || 1),
      page_size: String(pageSize),
    });
    if (keyword) q.set("q", keyword);
    if (status) q.set("status", status);
    if (risk) q.set("risk", risk);
    try {
      const result = await api("/api/changes?" + q.toString());
      const items = Array.isArray(result) ? result : (result.items || []);
      const total = Array.isArray(result) ? result.length : Number(result.total || 0);
      const page = Array.isArray(result) ? 1 : Number(result.page || 1);
      const size = Array.isArray(result) ? Math.max(total, 1) : Number(result.page_size || pageSize);
      const table = document.querySelector("#changeTable");
      const countEl = document.querySelector("#resultCount");
      if (table) table.innerHTML = changeTable(items);
      if (countEl) countEl.textContent = `共 ${total} 条 · 第 ${page} 页`;
      const pages = Math.max(1, Math.ceil(total / Math.max(1, size)));
      const pager = document.querySelector("#changePager");
      if (pager) {
        pager.innerHTML = `<button type="button" class="button button-secondary button-small" data-page-prev ${page <= 1 ? "disabled" : ""}>上一页</button>
          <span class="muted"> ${page} / ${pages} </span>
          <button type="button" class="button button-secondary button-small" data-page-next ${page >= pages ? "disabled" : ""}>下一页</button>`;
        pager.querySelector("[data-page-prev]")?.addEventListener("click", () => {
          if (state.changeListPage > 1) { state.changeListPage -= 1; loadPage(); }
        });
        pager.querySelector("[data-page-next]")?.addEventListener("click", () => {
          if (state.changeListPage < pages) { state.changeListPage += 1; loadPage(); }
        });
      }
      updateNotificationBadge();
    } catch (error) {
      const table = document.querySelector("#changeTable");
      if (table) table.innerHTML = emptyState("加载失败", error.message || "请稍后重试");
    }
  };

  const resetAndLoad = () => { state.changeListPage = 1; loadPage(); };
  ["#changeFilter","#statusFilter","#riskFilter"].forEach(selector => {
    document.querySelector(selector)?.addEventListener("change", resetAndLoad);
    document.querySelector(selector)?.addEventListener("input", () => {
      clearTimeout(state._changeFilterTimer);
      state._changeFilterTimer = setTimeout(resetAndLoad, 280);
    });
  });
  await loadPage();
}

function assetRecord(app) {
  const changes = state.changes.filter(change => change.application_id === app.id);
  const findings = changes.flatMap(change => (change.findings || []).map(finding => ({...finding, change})));
  const activeFindings = findings.filter(item => item.status !== "VERIFIED");
  const latest = [...changes].sort((left, right) => new Date(right.updated_at) - new Date(left.updated_at))[0] || null;
  const risk = activeFindings.some(item => item.severity === "HIGH") ? "HIGH" : activeFindings.length ? "MEDIUM" : changes.length ? "LOW" : "UNKNOWN";
  return {app, changes, findings, activeFindings, latest, risk, highCount:activeFindings.filter(item => item.severity === "HIGH").length};
}

function assetTable(records) {
  if (!records.length) return `<div class="registry-empty">${svg("server")}<strong>没有符合条件的服务</strong><span>调整搜索条件，或前往服务配置纳管业务服务。</span><button class="button button-secondary" data-route="apps">前往服务配置</button></div>`;
  return `<table class="data-table asset-registry-table"><thead><tr><th>服务名称</th><th>类型 / 运行时</th><th>环境</th><th>负责人</th><th>变更记录</th><th>未闭环风险</th><th>最近变更</th><th></th></tr></thead><tbody>${records.map(record => `<tr data-route="assets/${record.app.id}">
    <td><div class="change-title"><span class="type-icon">${svg("server")}</span><div><strong>${escapeHTML(record.app.name)}</strong><span>${escapeHTML(record.app.tier || "T2")} · ${escapeHTML(record.app.lifecycle || "生产运行")}</span></div></div></td>
    <td><div class="asset-database"><strong>${escapeHTML(record.app.kind || "后端服务")}</strong><span>${escapeHTML(record.app.runtime || [record.app.database,record.app.schema].filter(Boolean).join(" / ") || "待补充技术栈")}</span></div></td>
    <td><span class="asset-environment">${escapeHTML(record.app.environment || "未配置")}</span></td>
    <td><div class="person-cell"><span class="avatar">${escapeHTML(initials(record.app.owner))}</span><span>${escapeHTML(record.app.owner || "待分配")}</span></div></td>
    <td><strong class="registry-number">${record.changes.length}</strong></td>
    <td><div class="asset-risk-count">${riskBadge(record.risk)}<span>${record.activeFindings.length} 项</span></div></td>
    <td>${record.latest ? `<div class="asset-latest"><strong>${escapeHTML(record.latest.title)}</strong><span>${formatDate(record.latest.updated_at)}</span></div>` : "—"}</td>
    <td><button class="icon-button" aria-label="查看服务详情">${svg("arrow")}</button></td>
  </tr>`).join("")}</tbody></table>`;
}

function renderAssets(main) {
  setHeader("服务目录");
  const canManage = Boolean(actor().enterprise_admin || actor().role === "技术负责人");
  const records = state.apps.map(assetRecord);
  const productionCount = records.filter(record => /生产|prod/i.test(record.app.environment || "")).length;
  const activeRiskCount = records.reduce((sum, record) => sum + record.activeFindings.length, 0);
  const changedCount = records.filter(record => record.changes.length).length;
  const environments = [...new Set(state.apps.map(app => app.environment).filter(Boolean))];
  const actions = `<button class="button button-secondary" data-route="panorama">${svg("activity")}打开治理全景</button>${canManage ? `<button class="button button-primary" data-app-create>${svg("plus")}纳管业务服务</button>` : ""}`;
  main.innerHTML = pageHeading("服务目录", "统一维护服务归属、代码仓库、运行时、上下游依赖、资源适配器和变更风险。", actions) + `
    <section class="registry-stat-grid">
      <article><span>纳管服务</span><strong>${records.length}</strong><small>按业务服务划分治理边界</small></article>
      <article><span>生产服务</span><strong>${productionCount}</strong><small>执行审批、回滚与观察校验</small></article>
      <article><span>发生过变更</span><strong>${changedCount}</strong><small>已形成可追溯发布记录</small></article>
      <article class="danger"><span>未闭环风险</span><strong>${activeRiskCount}</strong><small>跨服务汇总整改事项</small></article>
    </section>
    <article class="panel registry-panel">
      <div class="toolbar"><div class="filter-group"><input class="filter-input" id="assetSearch" value="${escapeHTML(state.assetFilters.keyword)}" placeholder="搜索服务、运行时、仓库、资源或负责人"><select class="filter-select" id="assetEnvironment"><option value="">全部环境</option>${environments.map(environment => `<option value="${escapeHTML(environment)}" ${state.assetFilters.environment === environment ? "selected" : ""}>${escapeHTML(environment)}</option>`).join("")}</select></div><span class="result-count" id="assetResultCount">共 ${records.length} 项服务</span></div>
      <div class="table-wrap" id="assetTable">${assetTable(records)}</div>
    </article>`;
  const apply = () => {
    state.assetFilters.keyword = document.querySelector("#assetSearch").value.trim();
    state.assetFilters.environment = document.querySelector("#assetEnvironment").value;
    const keyword = state.assetFilters.keyword.toLowerCase();
    const filtered = records.filter(record => (!keyword || [record.app.name,record.app.kind,record.app.runtime,record.app.repository_url,record.app.database,record.app.schema,record.app.owner,record.app.description,...(record.app.tags || [])].join(" ").toLowerCase().includes(keyword)) && (!state.assetFilters.environment || record.app.environment === state.assetFilters.environment));
    document.querySelector("#assetTable").innerHTML = assetTable(filtered);
    document.querySelector("#assetResultCount").textContent = `共 ${filtered.length} 项服务`;
  };
  document.querySelector("#assetSearch").addEventListener("input", apply);
  document.querySelector("#assetEnvironment").addEventListener("change", apply);
}

function renderAssetDetail(main, id) {
  const app = state.apps.find(item => item.id === id);
  if (!app) {
    setHeader("服务不存在");
    main.innerHTML = `<div class="registry-empty">${svg("alert")}<strong>未找到该服务</strong><span>服务可能已被移出当前企业工作空间。</span><button class="button button-secondary" data-route="assets">返回服务目录</button></div>`;
    return;
  }
  const record = assetRecord(app);
  const canManage = Boolean(actor().enterprise_admin || actor().role === "技术负责人");
  const severity = {HIGH:0,MEDIUM:0,LOW:0,UNKNOWN:0};
  record.activeFindings.forEach(item => { severity[item.severity || "UNKNOWN"] = (severity[item.severity || "UNKNOWN"] || 0) + 1; });
  const completed = record.changes.filter(change => ["APPROVED","COMPLETED"].includes(change.status)).length;
  const dependencies = (app.dependencies || []).map(dependency => state.apps.find(item => item.id === dependency)?.name || dependency).filter(Boolean);
  const resources = [app.database && `数据库 ${app.database}${app.schema ? ` / ${app.schema}` : ""}`, app.runtime, ...(app.tags || [])].filter(Boolean);
  setHeader(app.name, `服务目录 / ${app.name}`);
  const actions = `${canManage ? `<button class="button button-secondary" data-app-edit="${app.id}">维护服务信息</button>` : ""}<button class="button button-primary" data-create>${svg("plus")}发起统一变更</button>`;
  main.innerHTML = `<button class="back-link" data-route="assets">${svg("back")}返回服务目录</button>` + pageHeading(escapeHTML(app.name), `${escapeHTML(app.kind || "后端服务")} · ${escapeHTML(app.runtime || "待补充技术栈")} · ${escapeHTML(app.environment || "未配置环境")}`, actions) + `
    <section class="asset-detail-hero">
      <article class="asset-profile"><span class="asset-profile-icon">${svg("server")}</span><div><span>${escapeHTML(app.tier || "T2")} · ${escapeHTML(app.lifecycle || "生产运行")}</span><h3>${escapeHTML(app.kind || "业务服务")}</h3><p>${escapeHTML(app.description || "尚未填写业务说明")}</p></div><dl><div><dt>服务负责人</dt><dd>${escapeHTML(app.owner || "待分配")}</dd></div><div><dt>治理环境</dt><dd>${escapeHTML(app.environment || "未配置")}</dd></div><div><dt>代码仓库</dt><dd>${escapeHTML(app.repository_url || "待登记")}</dd></div></dl></article>
      <article class="asset-health-card"><div><span>变更治理状态</span>${riskBadge(record.risk)}</div><strong>${record.risk === "HIGH" ? "存在高风险待整改" : record.activeFindings.length ? "存在未闭环风险" : record.changes.length ? "当前治理状态良好" : "等待首次变更登记"}</strong><p>状态由多制品规则、预发布验证、审批进度和整改证据动态计算。</p></article>
    </section>
    <section class="registry-stat-grid asset-detail-stats">
      <article><span>累计变更</span><strong>${record.changes.length}</strong><small>该服务全部发布申请</small></article>
      <article><span>已完成闭环</span><strong>${completed}</strong><small>审批通过或发布执行完成</small></article>
      <article><span>未闭环风险</span><strong>${record.activeFindings.length}</strong><small>待处理、整改中或待复核</small></article>
      <article class="danger"><span>高风险事项</span><strong>${record.highCount}</strong><small>需要优先完成整改复核</small></article>
    </section>
    <section class="service-context-grid">
      <article class="panel service-context-card"><header class="panel-header"><div><h3>上下游依赖</h3><p>用于评估变更传播范围和联动验证</p></div></header><div class="panel-body"><div class="service-chip-list">${dependencies.length ? dependencies.map(item => `<span>${svg("arrow")}${escapeHTML(item)}</span>`).join("") : '<span class="muted">暂未登记依赖服务</span>'}</div></div></article>
      <article class="panel service-context-card"><header class="panel-header"><div><h3>技术与资源</h3><p>数据库只是服务资源适配器之一</p></div></header><div class="panel-body"><div class="service-chip-list">${resources.length ? resources.map(item => `<span>${svg("server")}${escapeHTML(item)}</span>`).join("") : '<span class="muted">暂未登记技术与资源信息</span>'}</div></div></article>
    </section>
    <section class="asset-detail-grid">
      <article class="panel"><header class="panel-header"><div><h3>近期变更轨迹</h3><p>查看该服务最近的申请、验证和发布审批进度</p></div><button class="panel-link" data-route="changes">全部变更 →</button></header><div class="table-wrap">${changeTable(record.changes.slice(0, 8))}</div></article>
      <article class="panel asset-risk-panel"><header class="panel-header"><div><h3>风险画像</h3><p>按未闭环风险等级统计</p></div><button class="panel-link" data-route="risks">进入风险中心 →</button></header><div class="asset-risk-bars">${[["高风险",severity.HIGH,"high"],["中风险",severity.MEDIUM,"medium"],["低风险",severity.LOW,"low"]].map(item => `<div><span>${item[0]}</span><i><b class="${item[2]}" style="width:${record.activeFindings.length ? Math.max(5,item[1] / record.activeFindings.length * 100) : 0}%"></b></i><strong>${item[1]}</strong></div>`).join("")}</div><div class="asset-risk-footer"><span>最后变更</span><strong>${record.latest ? formatDate(record.latest.updated_at) : "暂无记录"}</strong></div></article>
    </section>`;
}

function flattenFindings() {
  return state.changes.flatMap(change => (change.findings || []).map(finding => ({...finding, change_id:change.id, change_title:change.title, application_id:change.application_id, application_name:change.application_name, change_status:change.status, submitter_name:change.submitter_name})));
}

function riskRegisterTable(items) {
  if (!items.length) return `<div class="registry-empty">${svg("shield")}<strong>没有符合条件的风险事项</strong><span>当前筛选范围内没有规则命中，或相关风险已经完成复核闭环。</span><button class="button button-secondary" data-route="changes">查看统一变更</button></div>`;
  const now = Date.now();
  return `<table class="data-table risk-register-table"><thead><tr><th>风险事项</th><th>等级</th><th>处置状态</th><th>所属服务</th><th>关联变更</th><th>负责人 / SLA</th><th>更新时间</th><th></th></tr></thead><tbody>${items.map(item => {
    const meta = findingStatusInfo(item.status);
    const due = item.due_at ? new Date(item.due_at).getTime() : 0;
    const overdue = due && due < now && item.status !== "VERIFIED";
    return `<tr data-open-change="${item.change_id}"><td><div class="risk-register-title"><span class="finding-level ${String(item.severity || "LOW").toLowerCase()}">${item.severity === "HIGH" ? "高" : item.severity === "MEDIUM" ? "中" : "低"}</span><div><strong>${escapeHTML(item.title)}</strong><span>${escapeHTML(item.code)} · ${escapeHTML(item.id)}</span></div></div></td><td>${riskBadge(item.severity)}</td><td><span class="finding-state finding-state-${meta[1]}">${meta[0]}</span></td><td><div class="asset-database"><strong>${escapeHTML(item.application_name)}</strong><span>${escapeHTML(item.application_id)}</span></div></td><td><div class="asset-latest"><strong>${escapeHTML(item.change_title)}</strong><span>${escapeHTML(item.change_id)}</span></div></td><td><div class="risk-owner"><strong>${escapeHTML(item.owner_name || "待分配")}</strong><span class="${overdue ? "overdue" : ""}">${overdue ? "已逾期 · " : ""}${item.due_at ? formatDate(item.due_at) : "未设置期限"}</span></div></td><td>${formatDate(item.updated_at)}</td><td><button class="icon-button" aria-label="查看关联变更">${svg("arrow")}</button></td></tr>`;
  }).join("")}</tbody></table>`;
}

function renderRiskCenter(main) {
  setHeader("风险事项中心");
  const items = flattenFindings();
  const active = items.filter(item => item.status !== "VERIFIED");
  const now = Date.now();
  const overdue = active.filter(item => item.due_at && new Date(item.due_at).getTime() < now).length;
  const pendingVerify = active.filter(item => item.status === "RESOLVED").length;
  const verified = items.filter(item => item.status === "VERIFIED").length;
  main.innerHTML = pageHeading("风险事项中心", "跨服务汇总代码、配置、Kubernetes、API 和 SQL 风险，形成发现、派单、整改、复核的闭环。", `<button class="button button-secondary" data-route="policies">${svg("shield")}管理策略规则</button><button class="button button-primary" data-route="changes">查看统一变更</button>`) + `
    <section class="registry-stat-grid risk-center-stats">
      <article><span>风险事项总数</span><strong>${items.length}</strong><small>全部历史规则命中记录</small></article>
      <article><span>待处置</span><strong>${active.length}</strong><small>待派单、整改中或待复核</small></article>
      <article><span>等待独立复核</span><strong>${pendingVerify}</strong><small>整改人与复核人职责分离</small></article>
      <article class="danger"><span>已逾期</span><strong>${overdue}</strong><small>超过整改 SLA 且尚未闭环</small></article>
      <article class="success"><span>已闭环</span><strong>${verified}</strong><small>整改证据已由审核人确认</small></article>
    </section>
    <article class="panel registry-panel">
      <div class="toolbar risk-toolbar"><div class="filter-group"><input class="filter-input" id="riskSearch" value="${escapeHTML(state.riskFilters.keyword)}" placeholder="搜索风险标题、规则编号、变更单或负责人"><select class="filter-select" id="riskApplication"><option value="">全部资产</option>${state.apps.map(app => `<option value="${app.id}" ${state.riskFilters.application === app.id ? "selected" : ""}>${escapeHTML(app.name)}</option>`).join("")}</select><select class="filter-select" id="riskSeverity"><option value="">全部等级</option><option value="HIGH" ${state.riskFilters.severity === "HIGH" ? "selected" : ""}>高风险</option><option value="MEDIUM" ${state.riskFilters.severity === "MEDIUM" ? "selected" : ""}>中风险</option><option value="LOW" ${state.riskFilters.severity === "LOW" ? "selected" : ""}>低风险</option></select><select class="filter-select" id="riskStatus"><option value="">全部状态</option>${["OPEN","ASSIGNED","RESOLVED","VERIFIED"].map(status => `<option value="${status}" ${state.riskFilters.status === status ? "selected" : ""}>${findingStatusInfo(status)[0]}</option>`).join("")}</select></div><span class="result-count" id="riskResultCount">共 ${items.length} 项风险</span></div>
      <div class="table-wrap" id="riskRegisterTable">${riskRegisterTable(items)}</div>
    </article>`;
  const apply = () => {
    state.riskFilters.keyword = document.querySelector("#riskSearch").value.trim();
    state.riskFilters.application = document.querySelector("#riskApplication").value;
    state.riskFilters.severity = document.querySelector("#riskSeverity").value;
    state.riskFilters.status = document.querySelector("#riskStatus").value;
    const keyword = state.riskFilters.keyword.toLowerCase();
    const filtered = items.filter(item => (!keyword || [item.title,item.code,item.id,item.change_id,item.change_title,item.application_name,item.owner_name,item.submitter_name].join(" ").toLowerCase().includes(keyword)) && (!state.riskFilters.application || item.application_id === state.riskFilters.application) && (!state.riskFilters.severity || item.severity === state.riskFilters.severity) && (!state.riskFilters.status || item.status === state.riskFilters.status));
    document.querySelector("#riskRegisterTable").innerHTML = riskRegisterTable(filtered);
    document.querySelector("#riskResultCount").textContent = `共 ${filtered.length} 项风险`;
  };
  document.querySelector("#riskSearch").addEventListener("input", apply);
  ["#riskApplication","#riskSeverity","#riskStatus"].forEach(selector => document.querySelector(selector).addEventListener("change", apply));
}

function processIndex(status) {
  if (["DRAFT","CHECKING","CHECK_FAILED","READY_FOR_EXPERIMENT"].includes(status)) return 0;
  if (["EXPERIMENT_QUEUED","EXPERIMENT_RUNNING"].includes(status)) return 1;
  if (status === "WAITING_APPROVAL") return 2;
  return 3;
}
function renderProcess(change) {
  const current = processIndex(change.status);
  const steps = [["规则检查","多制品确定性规则"],["预发布验证","回滚与专项检查"],["发布审批","证据复核"],["发布完成","审计闭环"]];
  return `<div class="process-strip">${steps.map((step,index) => `<div class="process-step ${index < current ? "done" : index === current ? "active" : ""}"><span class="process-node">${index < current ? "✓" : index+1}</span><div><strong>${step[0]}</strong><span>${step[1]}</span></div></div>`).join("")}</div>`;
}
function actionButtons(change) {
  const user = actor();
  const own = user.id === change.submitter_id;
  let buttons = [
    '<button type="button" class="button button-secondary" data-export data-format="md" data-id="' + change.id + '">导出 Markdown</button>',
    '<button type="button" class="button button-primary" data-export data-format="xlsx" data-id="' + change.id + '">导出 Excel</button>'
  ];
  if (["DRAFT","CHECK_FAILED","REJECTED"].includes(change.status) && own) {
    buttons.push('<button type="button" class="button button-secondary" data-edit data-id="' + change.id + '">编辑方案</button>');
  }
  if ((change.status === "DRAFT" || change.status === "CHECK_FAILED") && own) {
    buttons.push(`<button type="button" class="button button-primary" data-action="submit" data-id="${change.id}">${svg("shield")}提交规则检查</button>`);
  }
  if (change.status === "READY_FOR_EXPERIMENT" && (own || user.role === "技术负责人")) {
    buttons.push(`<button type="button" class="button button-primary" data-action="experiment" data-id="${change.id}">${svg("flask")}开始预发布验证</button>`);
  }
  if (change.status === "EXPERIMENT_QUEUED" || change.status === "EXPERIMENT_RUNNING") {
    buttons.push(`<button type="button" class="button button-secondary" disabled>验证进行中…</button>`);
  }
  if (change.status === "WAITING_APPROVAL" && !own && ["数据库审核人","技术负责人"].includes(user.role)) {
    buttons.push(`<button type="button" class="button button-danger" data-review="reject" data-id="${change.id}">驳回</button>`);
    buttons.push(`<button type="button" class="button button-primary" data-review="approve" data-id="${change.id}">${svg("check")}审批通过</button>`);
  }
  if (change.status === "WAITING_APPROVAL" && own) {
    buttons.push(`<button type="button" class="button button-secondary" disabled title="提交人不能审批自己的变更">等待他人审批</button>`);
  }
  if (change.status === "CHECK_FAILED" && own) {
    const n = (change.findings || []).length;
    buttons.push(`<button type="button" class="button button-secondary" disabled>有 ${n} 项检查问题待处理</button>`);
  }
  if (change.status === "APPROVED" && (own || user.role === "技术负责人")) {
    buttons.push(`<button type="button" class="button button-primary" data-action="complete" data-id="${change.id}">${svg("check")}标记执行完成</button>`);
  }
  if (buttons.length === 2) {
    buttons.push(`<button type="button" class="button button-secondary" disabled>当前身份暂无可执行操作</button>`);
  }
  return buttons.join("");
}
function artifactKindInfo(kind) {
  return {
    CODE:["代码 Diff","code"], CONFIG:["配置 Diff","settings"], KUBERNETES:["Kubernetes 清单","server"],
    API:["API 契约","activity"], DATABASE:["数据库 SQL","database"]
  }[kind] || [kind || "变更制品","file"];
}

function renderArtifacts(change) {
  const artifacts = [...(change.artifacts || [])];
  if (change.sql && !artifacts.some(item => item.kind === "DATABASE")) artifacts.push({kind:"DATABASE",name:"数据库 SQL",language:"SQL",content:change.sql});
  if (!artifacts.length) return '<div class="registry-empty compact">' + svg("file") + '<strong>未登记变更制品</strong><span>请补充代码、配置、部署清单、API 或 SQL 证据。</span></div>';
  return `<div class="artifact-card-grid">${artifacts.map(artifact => {
    const [label,icon] = artifactKindInfo(artifact.kind);
    return `<article class="artifact-card"><header><span class="artifact-kind">${svg(icon)}${escapeHTML(label)}</span><span>${escapeHTML(artifact.language || artifact.source || "文本证据")}</span></header><div class="artifact-name">${escapeHTML(artifact.name || label)}</div><pre>${escapeHTML(artifact.content || "未提供内容")}</pre></article>`;
  }).join("")}</div>`;
}

function renderReleasePlan(change) {
  const plan = change.release_plan || {};
  const metrics = plan.success_metrics || [];
  return `<div class="release-plan-card"><div class="release-plan-grid">
    <div><span>发布策略</span><strong>${escapeHTML(plan.strategy || "待制定")}</strong></div>
    <div><span>首批流量</span><strong>${Number(plan.canary_percent || 0)}%</strong></div>
    <div><span>观察窗口</span><strong>${Number(plan.observation_minutes || 0)} 分钟</strong></div>
    <div><span>异常处理</span><strong>${plan.auto_rollback ? "允许自动中止" : "人工决策"}</strong></div>
  </div><div class="release-metrics"><span>成功判定指标</span><div>${metrics.length ? metrics.map(item => `<b>${escapeHTML(item)}</b>`).join("") : '<em>尚未登记</em>'}</div></div></div>`;
}

function passportStatusMeta(pp) {
  if (!pp) return ["未签发", "draft"];
  if (pp.status === "REVOKED") return ["已吊销", "failed"];
  if (pp.status === "EXPIRED" || (pp.expires_at && new Date(pp.expires_at) < new Date())) return ["已过期", "failed"];
  if (pp.status === "ACTIVE") return ["有效", "approved"];
  return [pp.status || "未知", "draft"];
}

function renderPassportPanel(change) {
  const pp = change.passport;
  if (!pp && !["APPROVED", "COMPLETED"].includes(change.status)) {
    return `<article class="panel passport-panel">
      <header class="panel-header"><div class="section-title"><span class="section-number">05</span><div><h3>变更通行证 CP</h3><p>审批通过后自动签发，供 CD 门禁验签后才允许部署。</p></div></div>
      <span class="status status-draft"><i></i>待签发</span></header>
      <div class="panel-body"><p class="muted" style="margin:0;font-size:13px;line-height:1.6">完成规则检查、预发布验证与人工审批后，系统用 Ed25519 签发短时通行证，绑定 commit 与制品摘要。平台不直接发生产。</p></div>
    </article>`;
  }
  if (!pp) {
    return `<article class="panel passport-panel">
      <header class="panel-header"><div class="section-title"><span class="section-number">05</span><div><h3>变更通行证 CP</h3><p>当前变更无通行证记录（历史单可能早于该功能）。</p></div></div>
      <span class="status status-draft"><i></i>无</span></header>
      <div class="panel-body"><p class="muted" style="margin:0;font-size:13px">可对后续新审批的变更单使用 CP。</p></div>
    </article>`;
  }
  const [label, cls] = passportStatusMeta(pp);
  const claims = pp.claims || {};
  return `<article class="panel passport-panel">
    <header class="panel-header">
      <div class="section-title"><span class="section-number">05</span><div><h3>变更通行证 CP</h3><p>短时、可验签。CD 校验通过后才应部署对应 commit。</p></div></div>
      <span class="status status-${cls}"><i></i>${label}</span>
    </header>
    <div class="panel-body">
      <div class="description-list">
        <div class="description-item"><span>通行证 ID</span><strong><code>${escapeHTML(pp.passport_id || "")}</code></strong></div>
        <div class="description-item"><span>算法 / Key</span><strong>${escapeHTML(pp.alg || "Ed25519")} · ${escapeHTML(pp.key_id || "")}</strong></div>
        <div class="description-item"><span>绑定 Commit</span><strong><code>${escapeHTML(claims.commit_sha || change.commit_sha || "—")}</code></strong></div>
        <div class="description-item"><span>制品摘要</span><strong><code class="wrap-code">${escapeHTML((claims.artifact_digest || "").slice(0, 24))}…</code></strong></div>
        <div class="description-item"><span>签发时间</span><strong>${formatDate(pp.issued_at)}</strong></div>
        <div class="description-item"><span>有效期至</span><strong>${formatDate(pp.expires_at)}</strong></div>
      </div>
      ${pp.revoked_reason ? `<div class="approval-notice" style="margin-top:12px">${escapeHTML(pp.revoked_reason)}</div>` : ""}
      <div class="passport-actions" style="margin-top:14px;display:flex;flex-wrap:wrap;gap:8px">
        <button type="button" class="button button-primary button-small" data-copy-passport="${escapeHTML(change.id)}">复制 Compact</button>
        <button type="button" class="button button-secondary button-small" data-download-passport="${escapeHTML(change.id)}">下载 JSON</button>
        <button type="button" class="button button-secondary button-small" data-verify-passport="${escapeHTML(change.id)}">在线验签</button>
        <button type="button" class="button button-text button-small" data-route="settings">查看公钥</button>
      </div>
      <pre class="passport-compact" id="passportCompactText" style="margin-top:12px;max-height:96px;overflow:auto;font-size:11px;word-break:break-all">${escapeHTML(pp.compact || "")}</pre>
      <p class="field-hint" style="margin-top:8px">流水线示例：<code>curl -sS -X POST $HOST/api/passport/verify -d '{"compact":"...","expected_commit":"$CI_COMMIT_SHA"}'</code></p>
    </div>
  </article>`;
}

async function renderChangeDetail(main, id) {
  setHeader("变更详情", "统一变更 / 详情");
  main.innerHTML = '<div class="page-loading"><div class="skeleton skeleton-title"></div><div class="skeleton skeleton-panel"></div></div>';
  let change;
  try { change = await api("/api/changes/" + id); } catch (error) { main.innerHTML = emptyState("变更单不存在", error.message); return; }
  state.currentChange = change;
  const exp = change.experiment;
  const analysis = change.analysis;
  main.innerHTML = `
    <div class="detail-head">
      <div class="detail-head-main"><a class="back-link" href="#/changes">${svg("back")}返回变更列表</a><h2>${escapeHTML(change.title)}</h2><div class="detail-meta"><code>${escapeHTML(change.id)}</code>${riskBadge(change.risk)}${statusBadge(change.status)}<span>更新于 ${formatDate(change.updated_at)}</span></div></div>
      <div class="detail-actions">${actionButtons(change)}</div>
    </div>
    ${renderProcess(change)}
    <div class="detail-grid">
      <div class="detail-column">
        <article class="panel">
          <header class="panel-header"><div class="section-title"><span class="section-number">01</span><div><h3>变更内容</h3><p>服务、版本、变更制品与回滚计划</p></div></div></header>
          <div class="panel-body">
            <div class="description-list">
              <div class="description-item"><span>所属服务</span><strong>${escapeHTML(change.application_name)}</strong></div>
              <div class="description-item"><span>目标环境</span><strong>${escapeHTML(change.environment)}</strong></div>
              <div class="description-item"><span>计划窗口</span><strong>${formatDate(change.planned_at)}</strong></div>
            </div>
            <div class="change-source-bar"><span>${svg("code")}<b>${escapeHTML(change.repository_url || "未关联代码仓库")}</b></span><span>分支 ${escapeHTML(change.branch || "—")}</span><code>${escapeHTML(change.commit_sha || "—")}</code></div>
            <p class="change-description">${escapeHTML(change.description || "未填写业务说明")}</p>
            ${renderArtifacts(change)}
            <div class="rollback-panel"><div><span>${svg("refresh")}整体回滚方案</span><strong>${change.rollback_plan ? "已登记" : "待补充"}</strong></div><p>${escapeHTML(change.rollback_plan || "未登记跨制品回滚步骤")}</p>${change.rollback_sql ? `<details><summary>查看数据库回滚 SQL</summary><pre>${escapeHTML(change.rollback_sql)}</pre></details>` : ""}</div>
            ${renderReleasePlan(change)}
          </div>
        </article>
        <article class="panel">
          <header class="panel-header"><div class="section-title"><span class="section-number">02</span><div><h3>确定性规则检查</h3><p>检查代码、配置、Kubernetes、API、SQL 与发布基线</p></div></div><span>${change.findings?.length || 0} 项证据</span></header>
          <div class="panel-body"><div class="finding-list">${renderFindings(change)}</div></div>
        </article>
        <article class="panel">
          <header class="panel-header"><div class="section-title"><span class="section-number">03</span><div><h3>预发布验证</h3><p>汇总制品检查、回滚可用性与数据库专项验证</p></div></div>${exp ? `<span class="status ${exp.status === "PASSED" ? "status-approved" : "status-failed"}"><i></i>${exp.status === "PASSED" ? "验证通过" : "验证失败"}</span>` : ""}</header>
          <div class="panel-body">${renderExperiment(exp)}</div>
        </article>
        <article class="panel">
          <header class="panel-header"><div class="section-title"><span class="section-number">04</span><div><h3>辅助分析</h3><p>基于规则与验证结果生成说明，不能代替审批</p></div></div></header>
          <div class="panel-body">${renderAnalysis(analysis)}</div>
        </article>
        <article class="panel agent-qa-panel">
          <header class="panel-header"><div class="section-title"><span class="section-number">05</span><div><h3>AI 预审问答</h3><p>审批人可现场追问；Agent 调用拓扑 / 历史 / SQL 工具后回答，并写入审批留痕</p></div></div><span class="agent-qa-count">${(change.agent_qa || []).length} 条</span></header>
          <div class="panel-body">
            <div class="agent-qa-stream" id="agentQAStream">${renderAgentQA(change)}</div>
            <form class="agent-qa-form" id="agentQAForm" data-change-id="${escapeHTML(change.id)}">
              <label class="field field-wide"><span>向 Agent 提问</span>
                <textarea name="question" rows="3" maxlength="1000" placeholder="例如：这个变更会影响支付接口吗？上次类似变更出了什么问题？这个 SQL 索引会导致锁表吗？"></textarea>
              </label>
              <div class="agent-qa-suggestions">
                <button type="button" class="chip" data-qa-suggest="这个变更会影响下游哪些服务？依赖边界是什么？">影响面</button>
                <button type="button" class="chip" data-qa-suggest="同服务历史上有没有类似高风险变更？结论是什么？">历史事故</button>
                <button type="button" class="chip" data-qa-suggest="SQL 是否有锁表、全表扫描或无 WHERE 风险？">SQL 风险</button>
                <button type="button" class="chip" data-qa-suggest="当前规则阻断项中，哪些必须整改后才能批准？">阻断项</button>
              </div>
              <div class="comment-form-footer">
                <span>回答会写入时间线与审计（AGENT_QA），不能代替人工审批</span>
                <button class="button button-primary" type="submit" id="agentQASubmit">提问</button>
              </div>
            </form>
          </div>
        </article>
        ${renderPassportPanel(change)}
      </div>
      <aside class="detail-column side-column">
        <article class="panel"><header class="panel-header"><div><h3>责任信息</h3><p>提交、审批与版本记录</p></div></header><div class="panel-body side-info-list">
          <div class="side-info-row"><span>提交人</span><strong>${escapeHTML(change.submitter_name)}<br>${formatDate(change.created_at)}</strong></div>
          <div class="side-info-row"><span>审批人</span><strong>${escapeHTML(change.reviewer_name || "待分配")}</strong></div>
          <div class="side-info-row"><span>审批意见</span><strong>${escapeHTML(change.review_comment || "—")}</strong></div>
          <div class="side-info-row"><span>当前版本</span><strong>V${change.version}</strong></div>
        </div></article>
        ${renderImpactPanel(change)}
        <article class="panel"><header class="panel-header"><div><h3>审批约束</h3><p>生产发布必须满足的治理规则</p></div></header><div class="panel-body"><div class="approval-notice">提交人不能审批自己的变更；高风险发布必须由技术负责人批准；预发布验证失败时禁止批准。</div></div></article>
        <article class="panel"><header class="panel-header"><div><h3>处理时间线</h3><p>状态变化与操作人可追溯</p></div></header><div class="panel-body"><div class="timeline">${[...(change.timeline || [])].reverse().map(item => `<div class="timeline-item"><span class="timeline-dot"></span><div class="timeline-content"><strong>${escapeHTML(item.title)}</strong><p>${escapeHTML(item.detail)}</p><span>${escapeHTML(item.actor)} · ${formatDate(item.created_at)}</span></div></div>`).join("")}</div></div></article>
        <article class="panel collaboration-panel"><header class="panel-header"><div><h3>协作记录</h3><p>审核问题和处理说明集中留痕</p></div><span>${(change.comments || []).length} 条</span></header><div class="panel-body">
          <div class="comment-list">${renderComments(change.comments || [])}</div>
          <form class="comment-form" id="commentForm"><label class="field"><span>添加评论</span><textarea name="content" rows="3" maxlength="1000" placeholder="补充风险说明、修改结果或审批前置条件"></textarea></label><div class="comment-form-footer"><span>评论会写入审计日志</span><button class="button button-secondary" type="submit">发送评论</button></div></form>
        </div></article>

      </aside>
    </div>`;
}

function renderComments(comments) {
  if (!comments.length) return '<div class="comment-empty">暂无协作评论</div>';
  return [...comments].reverse().map(item => `<div class="comment-item"><span class="avatar">${escapeHTML(initials(item.author_name))}</span><div><div class="comment-meta"><strong>${escapeHTML(item.author_name)}</strong><span>${escapeHTML(item.author_role)} · ${formatDate(item.created_at)}</span></div><p>${escapeHTML(item.content)}</p></div></div>`).join("");
}

async function exportReport(id, format = "xlsx") {
  try {
    const response = await fetch("/api/changes/" + id + "/report?format=" + encodeURIComponent(format), {headers:{"X-Actor-ID":state.actorId}});
    if (!response.ok) throw new Error("报告生成失败");
    const blob = await response.blob();
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = id + (format === "xlsx" ? "-evidence.xlsx" : "-report.md");
    link.click();
    URL.revokeObjectURL(url);
    toast(format === "xlsx" ? "Excel 证据报告已导出" : "Markdown 报告已导出");
  } catch (error) {
    toast("导出失败", "error", error.message);
  }
}

function findingStatusInfo(status) {
  const map = {OPEN:["待处理","open"],ASSIGNED:["整改中","assigned"],RESOLVED:["待复核","resolved"],VERIFIED:["已闭环","verified"]};
  return map[status || "OPEN"] || map.OPEN;
}
function renderFindings(change) {
  const findings = change.findings || [];
  if (!findings.length) return '<div class="empty-state"><h3>尚未执行规则检查</h3><p>提交变更后将生成可复现的规则证据。</p></div>';
  const currentUser = actor();
  const canCoordinate = ["数据库审核人","技术负责人"].includes(currentUser.role);
  const blocking = findings.filter(item => item.blocking || item.severity === "HIGH");
  const tip = change.status === "CHECK_FAILED"
    ? `<div class="approval-notice" style="margin-bottom:12px">规则检查未通过${blocking.length ? `（${blocking.length} 项高优先级）` : ""}。修改制品或发布策略后点「编辑方案」，再「提交规则检查」。</div>`
    : "";
  return tip + findings.map(item => {
    const level = item.severity === "HIGH" ? ["高","high"] : item.severity === "MEDIUM" ? ["中","medium"] : ["低","low"];
    const status = item.status || "OPEN";
    const statusMeta = findingStatusInfo(status);
    const evidenceLocked = ["APPROVED","COMPLETED"].includes(change.status);
    const canResolve = !evidenceLocked && status !== "VERIFIED" && (item.owner_id === currentUser.id || change.submitter_id === currentUser.id || currentUser.role === "技术负责人");
    const canVerify = !evidenceLocked && status === "RESOLVED" && canCoordinate && item.owner_id !== currentUser.id;
    const actions = [];
    if (canCoordinate && !evidenceLocked && status !== "VERIFIED") actions.push('<button class="button button-secondary button-small" data-finding-action="assign" data-change-id="' + change.id + '" data-finding-id="' + item.id + '">派单</button>');
    if (canResolve) actions.push('<button class="button button-secondary button-small" data-finding-action="resolve" data-change-id="' + change.id + '" data-finding-id="' + item.id + '">' + (status === "RESOLVED" ? "更新整改" : "提交整改") + '</button>');
    if (canVerify) {
      actions.push('<button class="button button-danger button-small" data-finding-action="verify" data-approved="false" data-change-id="' + change.id + '" data-finding-id="' + item.id + '">退回</button>');
      actions.push('<button class="button button-primary button-small" data-finding-action="verify" data-approved="true" data-change-id="' + change.id + '" data-finding-id="' + item.id + '">复核通过</button>');
    }
    return '<div class="finding-card finding-card-enterprise">' +
      '<span class="finding-level ' + level[1] + '">' + level[0] + '</span>' +
      '<div class="finding-main"><div class="finding-title-row"><h4>' + escapeHTML(item.title) + '</h4><span class="finding-state finding-state-' + statusMeta[1] + '">' + statusMeta[0] + '</span></div>' +
      '<p>' + escapeHTML(item.detail) + '</p>' +
      (item.evidence ? '<div class="finding-evidence">' + escapeHTML(item.evidence) + '</div>' : '') +
      '<div class="finding-suggestion">建议：' + escapeHTML(item.suggestion) + '</div>' +
      '<div class="finding-ownership"><span><b>负责人</b>' + escapeHTML(item.owner_name || "待分配") + '</span><span><b>整改期限</b>' + formatDate(item.due_at) + '</span><span><b>规则编号</b>' + escapeHTML(item.code) + '</span></div>' +
      (item.resolution ? '<div class="finding-resolution"><strong>整改说明</strong><p>' + escapeHTML(item.resolution) + '</p>' + (item.verification_comment ? '<small>复核意见：' + escapeHTML(item.verification_comment) + '</small>' : '') + '</div>' : '') +
      (actions.length ? '<div class="finding-actions">' + actions.join("") + '</div>' : '') +
      '</div></div>';
  }).join("");
}

function renderExperiment(exp) {
  if (!exp) return '<div class="empty-state"><div class="empty-state-icon">' + svg("flask") + '</div><h3>尚未开始预发布验证</h3><p>规则检查通过后，可将制品证据提交给异步验证 Worker。</p></div>';
  const evidence = Array.isArray(exp.evidence) ? exp.evidence : [];
  const checksTotal = Number(exp.checks_total || evidence.length || 1);
  const checksPassed = Number(exp.checks_passed || (exp.status === "PASSED" ? checksTotal : 0));
  const hasDatabaseEvidence = Number(exp.dataset_rows || 0) > 0 || Number(exp.lock_wait_ms || 0) > 0 || Number(exp.p99_after_ms || 0) > 0;
  return `<div class="metrics-grid release-validation-metrics">
    <div class="metric-card"><span>验证项</span><strong>${checksPassed}/${checksTotal}</strong><small>${exp.status === "PASSED" ? "全部通过" : "存在失败项"}</small></div>
    <div class="metric-card"><span>发布策略</span><strong>${escapeHTML(exp.strategy || "未登记")}</strong><small>${Number(exp.canary_percent || 0)}% 首批流量</small></div>
    <div class="metric-card"><span>观察窗口</span><strong>${Number(exp.observation_minutes || 0)} 分钟</strong><small>发布后判定窗口</small></div>
    <div class="metric-card"><span>验证耗时</span><strong>${duration(exp.duration_ms || 0)}</strong><small>${escapeHTML(exp.kind || "多制品验证")}</small></div>
  </div>
  ${exp.execution_error ? `<div class="approval-notice" style="margin-top:12px;background:#fff1f2;border-color:#f6d5d9;color:#a52d3b">${escapeHTML(exp.execution_error)}</div>` : ""}
  <div class="description-list" style="margin-top:12px"><div class="description-item"><span>执行模式</span><strong>${exp.mode === "POSTGRES" ? "多制品检查 + PostgreSQL 影子库" : "多制品确定性验证"}</strong></div><div class="description-item"><span>回滚验证</span><strong>${exp.rollback_verified ? "通过" : "待确认"}</strong></div><div class="description-item"><span>失败检查</span><strong>${Math.max(0, checksTotal - checksPassed)}</strong></div></div>
  ${evidence.length ? `<div class="validation-evidence"><span>验证证据</span>${evidence.map(item => `<div>${svg("check")}<p>${escapeHTML(typeof item === "string" ? item : item.detail || item.title || JSON.stringify(item))}</p></div>`).join("")}</div>` : ""}
  ${hasDatabaseEvidence ? `<div class="database-evidence-block"><div class="subsection-heading"><strong>数据库专项验证</strong><span>仅在变更包含 SQL 时出现</span></div><div class="metrics-grid compact"><div class="metric-card"><span>样本数据量</span><strong>${Number(exp.dataset_rows || 0).toLocaleString()}</strong><small>行</small></div><div class="metric-card"><span>最大锁等待</span><strong>${exp.lock_wait_ms || 0}ms</strong><small>影子环境采样</small></div><div class="metric-card"><span>查询 P99</span><strong>${Number(exp.p99_after_ms || 0).toFixed(1)}ms</strong><small>变更前 ${Number(exp.p99_before_ms || 0).toFixed(1)}ms</small></div><div class="metric-card"><span>失败事务</span><strong>${exp.failed_transactions || 0}</strong><small>演练事务</small></div></div></div>` : ""}`;
}
function renderAnalysis(analysis) {
  const llmReady = !!(state.config && state.config.llm_configured);
  if (!analysis) {
    return `<div class="empty-state"><div class="empty-state-icon">${svg("activity")}</div>
      <h3>尚无辅助分析</h3>
      <p>提交规则检查后会生成说明。分析结论不能代替人工审批。</p>
      ${!llmReady ? `<button type="button" class="button button-secondary button-small" data-route="settings">接入 AI 提升分析质量</button>` : ""}
    </div>`;
  }
  const provider = analysis.provider || "unknown";
  const modelName = analysis.model || (provider === "rules-fallback" ? "本地归纳" : "—");
  const isLive = provider === "openai-compatible" || provider === "openai-compatible-agent";
  const sourceBadge = isLive
    ? `<span class="status status-approved"><i></i>模型 · ${escapeHTML(modelName)}</span>`
    : `<span class="status status-draft"><i></i>本地归纳</span>`;
  const meta = [
    `<span>${escapeHTML(provider)}</span>`,
    analysis.model ? `<span>${escapeHTML(analysis.model)}</span>` : "",
    analysis.trace_id ? `<span>trace ${escapeHTML(analysis.trace_id)}</span>` : "",
    analysis.tool_calls ? `<span>${Number(analysis.tool_calls)} 次工具</span>` : "",
    analysis.tokens ? `<span>${Number(analysis.tokens)} tokens</span>` : "",
    analysis.generated_at ? `<span>${formatDate(analysis.generated_at)}</span>` : "",
  ].filter(Boolean).join("");
  const upgradeHint = !isLive && !llmReady
    ? `<div class="analysis-upgrade"><span>当前为规则结果本地归纳。接入企业模型后可获得更完整的证据解读。</span><button type="button" class="button button-secondary button-small" data-route="settings">接入 AI</button></div>`
    : "";
  const tools = (analysis.tool_call_log || []).length
    ? `<div class="agent-tool-log"><strong>工具轨迹</strong>${(analysis.tool_call_log || []).map(t => `<code>${escapeHTML(t.name || "")}</code>`).join("")}</div>`
    : "";
  return `<div class="analysis-card analysis-panel">
    <div class="analysis-card-head"><h4>辅助分析</h4><div class="analysis-head-meta">${sourceBadge}${riskBadge(analysis.risk)}${analysis.injection_suspected ? `<span class="status status-failed"><i></i>疑似注入</span>` : ""}</div></div>
    <p class="analysis-disclaimer">参考用。规则阻断与审批结论以人工和确定性检查为准。</p>
    ${upgradeHint}
    <p class="analysis-summary">${escapeHTML(analysis.summary)}</p>
    <div class="analysis-columns">
      <div><strong>依据</strong><ul>${(analysis.reasons || []).map(item => `<li>${escapeHTML(item)}</li>`).join("") || "<li class=\"muted\">—</li>"}</ul></div>
      <div><strong>建议</strong><ul>${(analysis.suggestions || []).map(item => `<li>${escapeHTML(item)}</li>`).join("") || "<li class=\"muted\">—</li>"}</ul></div>
    </div>
    ${tools}
    <div class="analysis-meta">${meta}</div>
    <div class="evidence-ids">${(analysis.evidence_ids || []).map(id => `<code>${escapeHTML(id)}</code>`).join("")}</div>
  </div>`;
}

function renderAgentQAItem(item, options = {}) {
  const pending = !!options.pending;
  const tools = (item.tool_call_log || []).map(t => escapeHTML(t.name || "")).filter(Boolean);
  const answerBody = pending
    ? `<div class="agent-qa-thinking"><span class="agent-qa-dots" aria-hidden="true"><i></i><i></i><i></i></span><span>正在调用工具分析…</span></div>`
    : `<p>${escapeHTML(item.answer || "").replaceAll("\n", "<br>")}</p>${tools.length ? `<div class="agent-tool-log">${tools.map(n => `<code>${n}</code>`).join("")}</div>` : ""}`;
  return `<div class="agent-qa-item${pending ? " is-pending" : ""}" data-qa-id="${escapeHTML(item.id || "")}">
    <div class="agent-qa-q"><span class="avatar">${escapeHTML(initials(item.actor_name || actor()?.name || "审"))}</span>
      <div><div class="comment-meta"><strong>${escapeHTML(item.actor_name || actor()?.name || "审批人")}</strong><span>${formatDate(item.created_at || new Date().toISOString())}</span></div>
      <p>${escapeHTML(item.question || "")}</p></div>
    </div>
    <div class="agent-qa-a">
      <div class="agent-qa-a-head"><strong>Agent</strong>
        ${item.model ? `<span>${escapeHTML(item.model)}</span>` : ""}
        ${item.tool_calls ? `<span>${item.tool_calls} 次工具</span>` : (pending ? `<span>分析中</span>` : "")}
        ${item.trace_id ? `<code title="trace">${escapeHTML(item.trace_id)}</code>` : ""}
      </div>
      ${answerBody}
    </div>
  </div>`;
}

function renderAgentQA(change) {
  const items = Array.isArray(change.agent_qa) ? change.agent_qa : [];
  if (!items.length) {
    return `<div class="agent-qa-empty" id="agentQAEmpty">
      <div class="empty-state-icon">${svg("activity")}</div>
      <h4>还没有预审对话</h4>
      <p>审批人可直接提问。Agent 会调用服务拓扑、历史变更、SQL 扫描等工具作答，答案进入时间线与审计。</p>
    </div><div class="agent-qa-list" id="agentQAList" hidden></div>`;
  }
  return `<div class="agent-qa-empty" id="agentQAEmpty" hidden></div><div class="agent-qa-list" id="agentQAList">${[...items].reverse().map(item => renderAgentQAItem(item)).join("")}</div>`;
}

/** 就地更新预审问答区域，避免 renderPage 整页刷新。 */
function patchAgentQAUI(entry, change) {
  if (change) {
    state.currentChange = change;
    const idx = (state.changes || []).findIndex(item => item.id === change.id);
    if (idx >= 0) state.changes[idx] = change;
  }
  const stream = document.querySelector("#agentQAStream");
  if (!stream) return false;
  let list = stream.querySelector("#agentQAList");
  const empty = stream.querySelector("#agentQAEmpty");
  if (!list) {
    stream.innerHTML = `<div class="agent-qa-empty" id="agentQAEmpty" hidden></div><div class="agent-qa-list" id="agentQAList"></div>`;
    list = stream.querySelector("#agentQAList");
  }
  if (empty) empty.hidden = true;
  list.hidden = false;
  // 去掉 pending 占位
  list.querySelectorAll(".agent-qa-item.is-pending").forEach(node => node.remove());
  if (entry) {
    list.insertAdjacentHTML("afterbegin", renderAgentQAItem(entry));
    const first = list.querySelector(".agent-qa-item");
    first?.classList.add("agent-qa-enter");
    first?.scrollIntoView({ behavior: "smooth", block: "nearest" });
  }
  const count = document.querySelector(".agent-qa-count");
  if (count) {
    const n = Array.isArray(state.currentChange?.agent_qa) ? state.currentChange.agent_qa.length : list.children.length;
    count.textContent = n + " 条";
  }
  // 时间线仅追加最新一条，不全量重绘侧栏
  const timeline = document.querySelector(".timeline");
  if (timeline && entry) {
    const actorName = escapeHTML(entry.actor_name || actor()?.name || "审批人");
    const detail = escapeHTML(("问：" + (entry.question || "")).slice(0, 80) + " → 答：" + (entry.answer || "").slice(0, 120));
    timeline.insertAdjacentHTML("afterbegin", `<div class="timeline-item agent-qa-enter"><span class="timeline-dot"></span><div class="timeline-content"><strong>AI 预审问答</strong><p>${detail}</p><span>${actorName} · Agent · ${formatDate(entry.created_at || new Date().toISOString())}</span></div></div>`);
  }
  return true;
}

function showAgentQAPending(question) {
  const stream = document.querySelector("#agentQAStream");
  if (!stream) return;
  let list = stream.querySelector("#agentQAList");
  const empty = stream.querySelector("#agentQAEmpty");
  if (!list) {
    stream.innerHTML = `<div class="agent-qa-empty" id="agentQAEmpty" hidden></div><div class="agent-qa-list" id="agentQAList"></div>`;
    list = stream.querySelector("#agentQAList");
  }
  if (empty) empty.hidden = true;
  list.hidden = false;
  list.querySelectorAll(".agent-qa-item.is-pending").forEach(node => node.remove());
  list.insertAdjacentHTML("afterbegin", renderAgentQAItem({
    id: "pending",
    question,
    actor_name: actor()?.name || "审批人",
    created_at: new Date().toISOString(),
  }, { pending: true }));
  list.querySelector(".agent-qa-item.is-pending")?.scrollIntoView({ behavior: "smooth", block: "nearest" });
}

function renderExperiments(main) {
  setHeader("验证中心");
  const items = state.changes.filter(item => item.experiment || ["READY_FOR_EXPERIMENT","EXPERIMENT_QUEUED","EXPERIMENT_RUNNING"].includes(item.status));
  const queued = state.changes.filter(item => item.status === "EXPERIMENT_QUEUED").length;
  const running = state.changes.filter(item => item.status === "EXPERIMENT_RUNNING").length;
  const passed = state.changes.filter(item => item.experiment?.status === "PASSED").length;
  const mode = state.config?.experiment_mode === "postgres" ? "PostgreSQL 影子验证" : "模拟验证";
  main.innerHTML = pageHeading("预发布验证", "异步验证任务与结果。") + `<section class="registry-stat-grid experiment-real-stats"><article><span>排队</span><strong>${queued}</strong><small>Outbox 待消费</small></article><article><span>执行中</span><strong>${running}</strong><small>Worker 写回状态</small></article><article class="success"><span>通过</span><strong>${passed}</strong><small>有验证证据</small></article><article><span>模式</span><strong class="text-stat">${escapeHTML(mode)}</strong><small>见集成设置</small></article></section><article class="panel"><div class="toolbar"><span class="result-count">共 ${items.length} 条</span></div><div class="table-wrap">${changeTable(items)}</div></article>`;
}

function renderApprovals(main) {
  setHeader("发布审批");
  const items = state.changes.filter(item => item.status === "WAITING_APPROVAL");
  main.innerHTML = pageHeading("待审批", "核对规则结果、验证记录与回滚方案后批准或驳回。") + `<article class="panel"><div class="toolbar"><div class="filter-group"><span class="approval-notice">${escapeHTML(actor().name)} · ${escapeHTML(actor().role)}</span></div><span class="result-count">${items.length} 项</span></div><div class="table-wrap">${changeTable(items)}</div></article>`;
}

function renderBlastRadiusHTML(data) {
  if (!data || !Array.isArray(data.nodes)) return `<div class="blast-placeholder">暂无结果</div>`;
  const nodes = data.nodes || [];
  const down = nodes.filter(n => n.direction === "downstream");
  const up = nodes.filter(n => n.direction === "upstream");
  const epi = nodes.find(n => n.direction === "epicenter");
  const chip = (n) => `<div class="blast-node blast-${escapeHTML(n.direction || "")}${n.is_core ? " is-core" : ""}" title="${escapeHTML(n.owner || "")}">
    <em>${escapeHTML(n.hop_label || "")}</em>
    <strong>${escapeHTML(n.name || n.application_id || "")}</strong>
    <span>${escapeHTML(n.owner || "未登记负责人")}</span>
    <b>事故 ${Number(n.incident_count || 0)} · 高风险 ${Number(n.high_risk_changes || 0)}</b>
  </div>`;
  return `<div class="blast-result">
    <div class="blast-summary"><strong>${escapeHTML(data.summary || "")}</strong><span class="blast-hint">${escapeHTML(data.risk_hint || "")}</span></div>
    <div class="blast-stats">
      <span>波及 <b>${Number(data.affected_count || 0)}</b> 服务</span>
      <span>下游 <b>${Number(data.downstream_hops || 0)}</b> 跳</span>
      <span>核心链路 <b>${Number(data.core_path_count || 0)}</b></span>
    </div>
    <div class="blast-map">
      <div class="blast-col"><h5>上游依赖</h5>${up.length ? up.map(chip).join("") : "<em class='muted'>无</em>"}</div>
      <div class="blast-col blast-center"><h5>震中</h5>${epi ? chip(epi) : "<em class='muted'>—</em>"}</div>
      <div class="blast-col"><h5>下游波及</h5>${down.length ? down.map(chip).join("") : "<em class='muted'>无</em>"}</div>
    </div>
  </div>`;
}

function renderEfficacyHTML(data) {
  if (!data) return `<div class="blast-placeholder">暂无结果</div>`;
  const tone = ({IMPROVED:"ok", REGRESSED:"bad", NEUTRAL:"mid", PENDING:"wait"}[data.status] || "mid");
  return `<div class="efficacy-result efficacy-${tone}">
    <div class="efficacy-verdict"><span class="efficacy-status">${escapeHTML(data.status || "")}</span><strong>${escapeHTML(data.verdict || "")}</strong></div>
    <div class="efficacy-metrics">
      <div><span>错误率</span><b>${Number(data.error_rate_before || 0).toFixed(2)}% → ${Number(data.error_rate_after || 0).toFixed(2)}%</b><small>${escapeHTML(data.window_before || "")} / ${escapeHTML(data.window_after || "")}</small></div>
      <div><span>延迟 P95</span><b>${Number(data.p95_before_ms || 0).toFixed(1)}ms → ${Number(data.p95_after_ms || 0).toFixed(1)}ms</b><small>${escapeHTML(data.source || "mock")}</small></div>
    </div>
    ${data.knowledge_note ? `<p class="efficacy-knowledge">${escapeHTML(data.knowledge_note)}</p>` : ""}
  </div>`;
}

async function loadBlastRadius(changeId) {
  const body = document.querySelector("#blastRadiusBody");
  if (body) body.innerHTML = `<div class="blast-placeholder">正在推演爆炸半径…</div>`;
  try {
    const data = await api("/api/changes/" + changeId + "/blast-radius");
    if (body) body.innerHTML = renderBlastRadiusHTML(data);
  } catch (error) {
    if (body) body.innerHTML = `<div class="blast-placeholder">计算失败：${escapeHTML(error.message)}</div>`;
    toast("爆炸半径计算失败", "error", error.message);
  }
}

async function loadEfficacy(changeId) {
  const body = document.querySelector("#efficacyBody");
  if (body) body.innerHTML = `<div class="blast-placeholder">正在拉取效果指标…</div>`;
  try {
    const data = await api("/api/changes/" + changeId + "/efficacy");
    if (body) body.innerHTML = renderEfficacyHTML(data);
  } catch (error) {
    if (body) body.innerHTML = `<div class="blast-placeholder">拉取失败：${escapeHTML(error.message)}</div>`;
    toast("效果验证失败", "error", error.message);
  }
}

function renderIncidentBacktrace(main) {
  setHeader("事故回溯");
  main.innerHTML = pageHeading("事故反向关联", "输入线上症状，系统检索近期嫌疑变更 TOP3（Agent/规则可继续深挖）。") + `
    <article class="panel">
      <div class="panel-body">
        <form id="incidentForm" class="incident-form">
          <label class="field field-wide"><span>事故症状</span>
            <input name="symptom" maxlength="200" placeholder="例如：支付超时、下单 5xx、库存扣减失败" value="支付超时" />
          </label>
          <div class="comment-form-footer">
            <span>基于变更标题/描述/服务名/状态与时间邻近度打分</span>
            <button class="button button-primary" type="submit">反向定位</button>
          </div>
        </form>
        <div id="incidentResult" class="incident-result"></div>
      </div>
    </article>`;
  document.querySelector("#incidentForm")?.addEventListener("submit", async (event) => {
    event.preventDefault();
    const symptom = new FormData(event.target).get("symptom")?.trim();
    const box = document.querySelector("#incidentResult");
    if (!symptom) { toast("请输入症状", "error"); return; }
    if (box) box.innerHTML = `<div class="blast-placeholder">检索中…</div>`;
    try {
      const data = await api("/api/incidents/backtrace?symptom=" + encodeURIComponent(symptom));
      if (!box) return;
      if (!(data.suspects || []).length) {
        box.innerHTML = `<div class="blast-placeholder">${escapeHTML(data.summary || "无结果")}</div>`;
        return;
      }
      box.innerHTML = `<div class="incident-summary">${escapeHTML(data.summary || "")}</div>
        <div class="incident-list">${data.suspects.map(item => `
          <button type="button" class="incident-card" data-open-change="${escapeHTML(item.change_id)}">
            <div class="incident-rank">#${item.rank}</div>
            <div class="incident-main">
              <strong>${escapeHTML(item.title)}</strong>
              <span>${escapeHTML(item.application_name)} · ${escapeHTML(item.status)} · ${escapeHTML(item.risk)} · 分 ${Number(item.score || 0).toFixed(1)}</span>
              <ul>${(item.reasons || []).map(r => `<li>${escapeHTML(r)}</li>`).join("")}</ul>
            </div>
          </button>`).join("")}</div>`;
    } catch (error) {
      if (box) box.innerHTML = `<div class="blast-placeholder">${escapeHTML(error.message)}</div>`;
      toast("回溯失败", "error", error.message);
    }
  });
}

function renderObservability(main) {
  setHeader("发布观测");
  const changes = [...state.changes].sort((left,right) => new Date(right.updated_at) - new Date(left.updated_at));
  const active = changes.filter(item => ["EXPERIMENT_QUEUED","EXPERIMENT_RUNNING","WAITING_APPROVAL","APPROVED"].includes(item.status));
  const autoRollback = changes.filter(item => item.release_plan?.auto_rollback).length;
  const canary = changes.filter(item => /金丝雀|灰度|分批/.test(item.release_plan?.strategy || "")).length;
  const pending = changes.filter(item => item.status === "WAITING_APPROVAL").length;
  const rows = changes.slice(0,12).map(change => {
    const plan = change.release_plan || {};
    const metrics = plan.success_metrics || [];
    const validation = change.experiment;
    return `<tr data-open-change="${change.id}"><td><div class="change-title"><span class="type-icon">${svg("activity")}</span><div><strong>${escapeHTML(change.title)}</strong><span>${escapeHTML(change.application_name)} · ${escapeHTML(change.environment)}</span></div></div></td><td><strong>${escapeHTML(plan.strategy || "待制定")}</strong><span class="table-subtext">首批 ${Number(plan.canary_percent || 0)}%</span></td><td><strong>${Number(plan.observation_minutes || 0)} 分钟</strong><span class="table-subtext">${plan.auto_rollback ? "允许自动中止" : "人工决策"}</span></td><td><div class="metric-chip-list">${metrics.length ? metrics.slice(0,3).map(item => `<span>${escapeHTML(item)}</span>`).join("") : '<span class="muted">待补充</span>'}</div></td><td>${validation ? `<span class="status ${validation.status === "PASSED" ? "status-approved" : "status-failed"}"><i></i>${validation.status === "PASSED" ? "验证通过" : "验证失败"}</span>` : '<span class="status status-draft"><i></i>未验证</span>'}</td><td>${statusBadge(change.status)}</td></tr>`;
  }).join("");
  main.innerHTML = pageHeading("发布观测", "发布计划与验证结果。监控系统接入后可补真实指标。", `<button class="button button-secondary" data-route="settings">${svg("settings")}集成设置</button>`) + `
    <section class="registry-stat-grid observation-stats">
      <article><span>进行中的治理流程</span><strong>${active.length}</strong><small>验证、审批或待执行</small></article>
      <article><span>允许自动中止</span><strong>${autoRollback}</strong><small>异常时由发布系统执行动作</small></article>
      <article><span>灰度 / 分批策略</span><strong>${canary}</strong><small>避免生产环境一次性全量</small></article>
      <article class="danger"><span>待发布审批</span><strong>${pending}</strong><small>需要人工核对证据</small></article>
    </section>
    <section class="integration-strip"><div><span class="integration-icon">${svg("activity")}</span><div><strong>Prometheus</strong><span>待配置 · 当前不展示虚构实时指标</span></div></div><div><span class="integration-icon">${svg("code")}</span><div><strong>Jenkins / Argo CD</strong><span>待配置 · 当前只管理发布计划与证据</span></div></div><button class="button button-secondary button-small" data-route="settings">前往集成设置</button></section>
    <article class="panel"><header class="panel-header"><div><h3>发布计划与验证证据</h3><p>这些数据来自变更单和预发布验证，不代表已连接生产监控。</p></div><span>${changes.length} 条记录</span></header><div class="table-wrap"><table class="data-table observation-table"><thead><tr><th>变更与服务</th><th>发布策略</th><th>观察窗口</th><th>成功指标</th><th>验证证据</th><th>当前状态</th></tr></thead><tbody>${rows || `<tr><td colspan="6">${emptyState("暂无发布记录","创建变更单并完成规则检查后，此处将形成发布观测记录。")}</td></tr>`}</tbody></table></div></article>`;
}

function policyScopeLabel(policy) {
  const environments = policy.environments?.length ? policy.environments : ["全部环境"];
  const changeTypes = policy.change_types?.length ? policy.change_types : ["全部类型"];
  const artifactKinds = policy.artifact_kinds?.length ? policy.artifact_kinds : ["全部制品"];
  return `<div class="policy-scope"><span>${environments.map(escapeHTML).join(" / ")}</span><small>${changeTypes.map(escapeHTML).join(" / ")}</small><small>${artifactKinds.map(escapeHTML).join(" / ")}</small></div>`;
}

function renderPolicies(main) {
  setHeader("风险规则");
  const canManage = actor().role === "技术负责人";
  const policies = state.policies || [];
  const enabledCount = policies.filter(item => item.enabled).length;
  const blockingCount = policies.filter(item => item.enabled && item.blocking).length;
  const customCount = policies.filter(item => !item.builtin).length;
  const hitCount = policies.reduce((sum, item) => sum + Number(item.hit_count || 0), 0);
  const actions = `<button class="button button-secondary" data-policy-export>${svg("file")}导出规则 JSON</button>${canManage ? `<button class="button button-primary" data-policy-create>${svg("plus")}新建自定义规则</button>` : ""}`;

  main.innerHTML = pageHeading("规则", "代码 / 配置 / K8s / API / SQL 检查项。启停与是否阻断会影响提交结果。", actions) + `
    <section class="policy-stat-grid">
      <article><span>规则总数</span><strong>${policies.length}</strong><small>内置 ${policies.length - customCount} · 自定义 ${customCount}</small></article>
      <article><span>已启用</span><strong>${enabledCount}</strong><small>${policies.length ? Math.round(enabledCount / policies.length * 100) : 0}% 规则参与检查</small></article>
      <article><span>阻断规则</span><strong>${blockingCount}</strong><small>命中后禁止进入预发布验证</small></article>
      <article><span>累计命中</span><strong>${hitCount}</strong><small>仅统计正式提交检查</small></article>
    </section>
    <div class="policy-layout">
      <section class="panel policy-table-panel">
        <div class="panel-header policy-toolbar">
          <div><h3>规则清单</h3><p>内置语义规则与自定义正则规则统一版本化管理。</p></div>
          <div class="policy-filters">
            <input id="policySearch" class="compact-input" placeholder="搜索编码、名称或说明">
            <select id="policySeverity"><option value="">全部等级</option><option value="HIGH">高风险</option><option value="MEDIUM">中风险</option><option value="LOW">低风险</option></select>
            <select id="policyStatus"><option value="">全部状态</option><option value="enabled">已启用</option><option value="disabled">已停用</option><option value="custom">自定义</option><option value="builtin">内置</option></select>
          </div>
        </div>
        <div class="table-wrap"><table class="data-table policy-table">
          <thead><tr><th>规则</th><th>风险与阻断</th><th>作用范围</th><th>命中统计</th><th>状态与版本</th><th>操作</th></tr></thead>
          <tbody id="policyRows"></tbody>
        </table></div>
      </section>
      <aside class="panel policy-test-panel">
        <div class="panel-header"><div><h3>规则试跑</h3><p>不创建变更单、不累计命中次数，可用于策略发布前验证。</p></div><span class="status status-approved"><i></i>只读执行</span></div>
        <form id="policyTestForm" class="policy-test-form">
          <label class="field"><span>目标环境</span><select name="environment"><option>生产环境</option><option>预发布环境</option><option>测试环境</option></select></label>
          <label class="field"><span>变更类型</span><select name="change_type"><option>DDL</option><option>DML</option><option>索引变更</option><option>数据修复</option></select></label>
          <label class="field"><span>待检查 SQL <b>*</b></span><textarea name="sql" rows="8" required>UPDATE orders SET status = 'PAID';</textarea></label>
          <label class="field"><span>回滚 SQL</span><textarea name="rollback_sql" rows="4" placeholder="例如：UPDATE orders SET status = 'PENDING' WHERE id = ..."></textarea></label>
          <button class="button button-primary button-block" type="submit">${svg("shield")}执行规则试跑</button>
        </form>
        <div id="policyTestResult" class="policy-test-result">
          <div class="policy-test-empty">${svg("code")}<strong>等待试跑</strong><span>输入 SQL 后可查看命中规则、风险级别和整改建议。</span></div>
        </div>
      </aside>
    </div>`;

  const paintRows = () => {
    const keyword = (document.querySelector("#policySearch")?.value || "").trim().toLowerCase();
    const severity = document.querySelector("#policySeverity")?.value || "";
    const status = document.querySelector("#policyStatus")?.value || "";
    const filtered = policies.filter(policy => {
      const haystack = [policy.code, policy.name, policy.description, policy.suggestion].join(" ").toLowerCase();
      if (keyword && !haystack.includes(keyword)) return false;
      if (severity && policy.severity !== severity) return false;
      if (status === "enabled" && !policy.enabled) return false;
      if (status === "disabled" && policy.enabled) return false;
      if (status === "custom" && policy.builtin) return false;
      if (status === "builtin" && !policy.builtin) return false;
      return true;
    });
    const rows = document.querySelector("#policyRows");
    if (!filtered.length) {
      rows.innerHTML = `<tr><td colspan="6"><div class="table-empty">没有符合当前筛选条件的规则</div></td></tr>`;
      return;
    }
    rows.innerHTML = filtered.map(policy => `
      <tr class="${policy.enabled ? "" : "policy-row-disabled"}">
        <td><div class="policy-name"><code>${escapeHTML(policy.code)}</code><strong>${escapeHTML(policy.name)}</strong><span>${escapeHTML(policy.description)}</span>${policy.pattern ? `<small>表达式：${escapeHTML(policy.pattern)}</small>` : `<small>语义检查规则</small>`}</div></td>
        <td><div class="policy-risk">${riskBadge(policy.severity)}<span class="policy-blocking ${policy.blocking ? "is-blocking" : ""}">${policy.blocking ? "阻断提交" : "仅告警"}</span></div></td>
        <td>${policyScopeLabel(policy)}</td>
        <td><div class="policy-hit"><strong>${Number(policy.hit_count || 0)}</strong><span>最近：${policy.last_hit_at ? formatDate(policy.last_hit_at) : "尚未命中"}</span></div></td>
        <td><div class="policy-version"><span class="status ${policy.enabled ? "status-approved" : "status-draft"}"><i></i>${policy.enabled ? "已启用" : "已停用"}</span><small>v${Number(policy.version || 1)} · ${policy.builtin ? "内置" : "自定义"}</small><small>${escapeHTML(policy.updated_by || "system")}</small></div></td>
        <td><div class="table-actions">${canManage ? `<button class="button button-small button-secondary" data-policy-edit="${policy.id}">编辑</button><button class="button button-small ${policy.enabled ? "button-danger-soft" : "button-primary-soft"}" data-policy-toggle="${policy.id}">${policy.enabled ? "停用" : "启用"}</button>` : `<span class="muted">只读</span>`}</div></td>
      </tr>`).join("");
  };
  paintRows();
  ["policySearch", "policySeverity", "policyStatus"].forEach(id => {
    document.querySelector("#" + id)?.addEventListener(id === "policySearch" ? "input" : "change", paintRows);
  });
  document.querySelector("#policyTestForm")?.addEventListener("submit", runPolicyTest);
}

async function runPolicyTest(event) {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const payload = Object.fromEntries(form.entries());
  const button = event.currentTarget.querySelector("button[type=submit]");
  const box = document.querySelector("#policyTestResult");
  button.disabled = true;
  button.innerHTML = `${svg("activity")}正在检查…`;
  box.innerHTML = `<div class="policy-test-loading">正在加载当前启用规则并分析 SQL…</div>`;
  try {
    const result = await api("/api/policies/test", {method:"POST", body:JSON.stringify(payload)});
    const findings = result.findings || [];
    box.innerHTML = `<div class="policy-test-summary"><div><span>综合风险</span>${riskBadge(result.risk)}</div><div><span>SQL 语句</span><strong>${(result.statements || []).length}</strong></div><div><span>命中规则</span><strong>${findings.length}</strong></div></div>
      <div class="policy-test-list">${findings.length ? findings.map(finding => `<article><div><code>${escapeHTML(finding.code)}</code>${riskBadge(finding.severity)}${finding.blocking ? `<span class="policy-blocking is-blocking">阻断</span>` : ""}</div><strong>${escapeHTML(finding.title)}</strong><p>${escapeHTML(finding.detail)}</p><small>建议：${escapeHTML(finding.suggestion || "请评估后处理")}</small></article>`).join("") : `<div class="policy-pass">${svg("check")}<div><strong>当前规则检查通过</strong><span>未发现已启用规则对应的风险。</span></div></div>`}</div>`;
  } catch (error) {
    box.innerHTML = `<div class="policy-test-error"><strong>试跑失败</strong><span>${escapeHTML(error.message)}</span></div>`;
  } finally {
    button.disabled = false;
    button.innerHTML = `${svg("shield")}执行规则试跑`;
  }
}

function openPolicy(policy = null) {
  state.editingPolicy = policy;
  const form = document.querySelector("#policyForm");
  form.reset();
  document.querySelector("#policyModalTitle").textContent = policy ? "编辑风险规则" : "新建自定义规则";
  document.querySelector("#policyModalDescription").textContent = policy?.builtin ? "可调整风险等级、阻断策略、制品范围和启停状态；内置语义不可替换。" : "使用 Go 正则表达式定义可复用的制品内容检查策略。";
  document.querySelector("#savePolicyButton").textContent = policy ? "保存并生成新版本" : "创建并启用规则";
  const fields = ["code","name","description","pattern","suggestion","severity"];
  fields.forEach(name => {
    const element = form.elements.namedItem(name);
    if (element) element.value = policy?.[name] || (name === "severity" ? "MEDIUM" : "");
  });
  form.elements.namedItem("environments").value = (policy?.environments || []).join("，");
  form.elements.namedItem("change_types").value = (policy?.change_types || []).join("，");
  form.elements.namedItem("artifact_kinds").value = (policy?.artifact_kinds || []).join("，");
  form.elements.namedItem("blocking").checked = Boolean(policy?.blocking);
  form.elements.namedItem("enabled").checked = policy ? Boolean(policy.enabled) : true;
  form.elements.namedItem("code").disabled = Boolean(policy);
  form.elements.namedItem("pattern").disabled = Boolean(policy?.builtin);
  document.querySelector("#policyPatternHint").textContent = policy?.builtin ? "该规则由语义检查器实现，表达式不可编辑。" : "采用 Go RE2 语法，不支持反向引用和环视。";
  setModalOpen(document.querySelector("#policyModal"), true);
}

function closePolicy() {
  setModalOpen(document.querySelector("#policyModal"), false);
  document.querySelector("#policyForm")?.reset();
  state.editingPolicy = null;
}

async function togglePolicy(id) {
  const policy = state.policies.find(item => item.id === id);
  try {
    const result = await api(`/api/policies/${id}/toggle`, {method:"POST", body:"{}"});
    await refreshData(false);
    if (currentRoute()[0] === "policies") renderPolicies(document.querySelector("#mainContent"));
    toast(result.enabled ? "规则已启用" : "规则已停用", "success", policy?.name || id);
  } catch (error) {
    toast("规则状态更新失败", "error", error.message);
  }
}

async function exportPolicies() {
  try {
    const response = await fetch("/api/policies/export", {headers:{"X-Actor-ID":state.actorId}});
    if (!response.ok) throw new Error("规则导出失败");
    const blob = await response.blob();
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = "dbguard-risk-policies-" + new Date().toISOString().slice(0,10) + ".json";
    link.click();
    URL.revokeObjectURL(url);
    toast("规则配置已导出");
  } catch (error) {
    toast("导出失败", "error", error.message);
  }
}


function renderAuthGate(message = "") {
  const gate = document.querySelector("#authGate");
  const params = new URLSearchParams(location.search);
  const inviteToken = params.get("invite") || "";
  const errorMessage = params.get("auth_error") || message;
  document.body.classList.add("auth-required");
  gate.hidden = false;
  const sso = state.authStatus?.oidc_enabled ? `<button class="auth-sso-button" type="button" data-sso-login><span>${svg("shield")}</span><div><strong>使用企业单点登录（SSO）</strong><small>${escapeHTML(state.authStatus.provider || "企业身份平台")}</small></div>${svg("arrow")}</button>` : "";
  const authVisual = `<aside class="auth-visual" aria-hidden="true">
      <div class="auth-visual-orb auth-visual-orb-a"></div>
      <div class="auth-visual-orb auth-visual-orb-b"></div>
      <div class="auth-visual-grid"></div>
      <span class="auth-orbit auth-orbit-1"></span>
      <span class="auth-orbit auth-orbit-2"></span>
      <span class="auth-orbit auth-orbit-3"></span>
      <span class="auth-orbit auth-orbit-4"></span>
      <i class="auth-scanline"></i>
      <div class="auth-floor" aria-hidden="true"></div>
      <span class="auth-hud hud-tl"><b>SEC-LINK</b>CHG-01</span>
      <span class="auth-hud hud-tr"><b>NODE</b>EAST-2</span>
      <span class="auth-hud hud-bl"><b>ENCRYPT</b>AES-256</span>
      <span class="auth-hud hud-br"><b>SIGNAL</b>STABLE</span>
      <div class="auth-visual-content">
        <div class="auth-visual-badge"><span class="brand-mark">${svg("shield")}</span><div><strong>ChangeGuard</strong><small>Enterprise Change Risk Control</small></div></div>
        <h2>让每一次研发变更<br>都可控、可审计、可回滚</h2>
        <p>规则检查、预发验证、多级审批与全链路审计，构建企业级发布治理闭环。</p>
        <ul class="auth-visual-features">
          <li><i></i><span><b>智能规则引擎</b>拦截高风险变更与冲突窗口</span></li>
          <li><i></i><span><b>预发布验证</b>影子演练后再进入审批</span></li>
          <li><i></i><span><b>全链路审计</b>身份、角色与审批动作可追溯</span></li>
        </ul>
        <div class="auth-visual-metrics">
          <div><strong>01</strong><span>规则检查</span></div>
          <div><strong>02</strong><span>预发验证</span></div>
          <div><strong>03</strong><span>审批闭环</span></div>
        </div>
      </div>
    </aside>`;
  if (inviteToken) {
    gate.innerHTML = `<div class="auth-shell">${authVisual}<section class="auth-card auth-card-invite">
      <div class="auth-brand"><span class="brand-mark">${svg("shield")}</span><div><strong>ChangeGuard</strong><small>企业研发变更风险治理</small></div></div>
      <div class="auth-copy"><span class="eyebrow">企业成员邀请</span><h1>接受邀请并加入工作空间</h1><p>注册后将按照邀请人分配的职责参与提交、整改、审核或批准流程。</p></div>
      ${errorMessage ? `<div class="auth-message error">${escapeHTML(errorMessage)}</div>` : ""}
      <form id="acceptInviteForm" class="auth-form">
        <input type="hidden" name="token" value="${escapeHTML(inviteToken)}">
        <label class="field"><span>姓名</span><input name="name" required maxlength="40" autocomplete="name"></label>
        <label class="field"><span>受邀邮箱</span><input name="email" type="email" required autocomplete="email" placeholder="name@company.com"></label>
        <label class="field"><span>设置密码</span><input name="password" type="password" required minlength="8" autocomplete="new-password" placeholder="至少 8 位，包含字母和数字"></label>
        <button class="button button-primary auth-submit" type="submit">加入企业工作空间</button>
      </form>${sso}<button class="auth-text-button" type="button" data-auth-home>返回普通登录</button>
    </section></div>`;
    ensureAuthCanvas(gate);
    return;
  }
  gate.innerHTML = `<div class="auth-shell">${authVisual}<section class="auth-card">
      <div class="auth-brand"><span class="brand-mark">${svg("shield")}</span><div><strong>ChangeGuard</strong><small>企业研发变更风险治理</small></div></div>
    <div class="auth-tabs ${state.authStatus?.local_enabled === false ? "single" : ""}"><button class="active" data-auth-tab="login">企业登录</button>${state.authStatus?.local_enabled !== false ? `<button data-auth-tab="register">创建企业</button>` : ""}</div>
    ${errorMessage ? `<div class="auth-message error">${escapeHTML(errorMessage)}</div>` : ""}
    <div id="authLoginPanel"><div class="auth-copy"><span class="eyebrow">安全访问</span><h1>进入企业治理工作空间</h1><p>身份与企业、角色绑定，所有审批动作都会写入审计日志。</p></div>
      ${state.authStatus?.local_enabled !== false ? `<form id="authLoginForm" class="auth-form" autocomplete="off" data-form-type="other"><label class="field"><span>企业邮箱</span><input name="email" type="email" required autocomplete="email" autocapitalize="none" spellcheck="false"></label><label class="field"><span>密码</span><input name="password" type="password" required autocomplete="current-password"></label><button class="button button-primary auth-submit" type="submit">登录工作空间</button></form>` : ""}${sso}
    </div>
    <div id="authRegisterPanel" hidden><div class="auth-copy"><span class="eyebrow">创建企业</span><h1>建立独立的治理空间</h1><p>首位注册者将成为企业管理员和技术负责人，随后通过邀请分配员工职责。</p></div>
      <form id="enterpriseRegisterForm" class="auth-form auth-form-grid">
        <label class="field field-wide"><span>企业名称</span><input name="organization_name" required maxlength="80"></label>
        <label class="field"><span>企业标识</span><input name="organization_slug" required pattern="[a-z0-9][a-z0-9-]{2,39}" placeholder="stellar-tech"><small>小写字母、数字和短横线。</small></label>
        <label class="field"><span>管理员姓名</span><input name="name" required maxlength="40"></label>
        <label class="field"><span>管理员邮箱</span><input name="email" type="email" required></label>
        <label class="field"><span>设置密码</span><input name="password" type="password" required minlength="8" placeholder="至少 8 位，包含字母和数字"></label>
        <button class="button button-primary auth-submit field-wide" type="submit">创建企业并进入系统</button>
      </form>
    </div>
    <div class="auth-security-note">${svg("lock")}<span>登录信息仅用于企业身份验证，成员加入、角色调整和审批操作都会留存安全审计。</span></div>
  </section></div>`;
  ensureAuthCanvas(gate);
}

/* ─── 登录页深空粒子引擎：星尘 · 星座连线 · 流星 · 鼠标互动 ─── */
let __authParticlesRunning = false;

function ensureAuthCanvas(gate) {
  let canvas = gate.querySelector("#authCanvas");
  if (!canvas) {
    canvas = document.createElement("canvas");
    canvas.id = "authCanvas";
    canvas.setAttribute("aria-hidden", "true");
    gate.insertBefore(canvas, gate.firstChild);
  }
  if (!__authParticlesRunning) {
    __authParticlesRunning = true;
    runAuthParticles(canvas, gate);
  }
}

function runAuthParticles(canvas, gate) {
  const ctx = canvas.getContext("2d");
  const dpr = Math.min(window.devicePixelRatio || 1, 2);
  let w = 0, h = 0, dots = [], raf = 0;
  const mouse = { x: -1e4, y: -1e4 };

  function resize() {
    w = gate.offsetWidth; h = gate.offsetHeight;
    canvas.width = Math.max(1, w * dpr); canvas.height = Math.max(1, h * dpr);
    canvas.style.width = w + "px"; canvas.style.height = h + "px";
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    const count = Math.max(42, Math.min(150, Math.floor(w * h / 7200)));
    dots = [];
    for (let i = 0; i < count; i++) {
      dots.push({
        x: Math.random() * w, y: Math.random() * h,
        vx: (Math.random() - .5) * .28,
        vy: (Math.random() - .5) * .28 + .06,
        r: Math.random() * 1.7 + .4,
        c: Math.random() > .85 ? "rgba(255,110,220,.9)" : Math.random() > .5 ? "rgba(90,220,255,.9)" : "rgba(150,190,255,.8)",
        tw: Math.random() * Math.PI * 2
      });
    }
  }

  function frame(now) {
    ctx.clearRect(0, 0, w, h);
    for (let i = 0; i < dots.length; i++) {
      const a = dots[i];
      for (let j = i + 1; j < dots.length; j++) {
        const b = dots[j];
        const dx = a.x - b.x, dy = a.y - b.y;
        const d2 = dx * dx + dy * dy;
        if (d2 < 115 * 115) {
          const alpha = (1 - Math.sqrt(d2) / 115) * .22;
          ctx.strokeStyle = "rgba(120,210,255," + alpha.toFixed(3) + ")";
          ctx.lineWidth = .6;
          ctx.beginPath(); ctx.moveTo(a.x, a.y); ctx.lineTo(b.x, b.y); ctx.stroke();
        }
      }
      const mdx = a.x - mouse.x, mdy = a.y - mouse.y;
      const md2 = mdx * mdx + mdy * mdy;
      if (md2 < 150 * 150) {
        const alpha = (1 - Math.sqrt(md2) / 150) * .4;
        ctx.strokeStyle = "rgba(90,240,255," + alpha.toFixed(3) + ")";
        ctx.lineWidth = .7;
        ctx.beginPath(); ctx.moveTo(a.x, a.y); ctx.lineTo(mouse.x, mouse.y); ctx.stroke();
      }
      a.tw += .03;
      const pulse = .55 + Math.sin(a.tw) * .45;
      ctx.globalAlpha = pulse;
      ctx.fillStyle = a.c;
      ctx.beginPath(); ctx.arc(a.x, a.y, a.r, 0, Math.PI * 2); ctx.fill();
      ctx.globalAlpha = 1;
      a.x += a.vx; a.y += a.vy;
      if (a.y > h + 10) { a.y = -10; a.x = Math.random() * w; }
      if (a.x > w + 10) a.x = -10;
      if (a.x < -10) a.x = w + 10;
    }
    if (Math.random() < .005) {
      const sx = Math.random() * w * .7 + w * .1, sy = Math.random() * h * .4;
      ctx.strokeStyle = "rgba(190,245,255,.9)";
      ctx.lineWidth = 1.4;
      ctx.beginPath(); ctx.moveTo(sx, sy); ctx.lineTo(sx - 60, sy + 34); ctx.stroke();
      ctx.strokeStyle = "rgba(190,245,255,.22)";
      ctx.lineWidth = 5;
      ctx.beginPath(); ctx.moveTo(sx, sy); ctx.lineTo(sx - 60, sy + 34); ctx.stroke();
    }
    if (!gate.hidden) {
      raf = requestAnimationFrame(frame);
    } else {
      __authParticlesRunning = false;
    }
  }

  resize();
  if (!canvas.dataset.bound) {
    canvas.dataset.bound = "1";
    window.addEventListener("resize", resize);
    gate.addEventListener("mousemove", (e) => {
      const r = gate.getBoundingClientRect();
      mouse.x = e.clientX - r.left; mouse.y = e.clientY - r.top;
    });
    gate.addEventListener("mouseleave", () => { mouse.x = -1e4; mouse.y = -1e4; });
  }
  raf = requestAnimationFrame(frame);
}

function switchAuthTab(tab) {
  document.querySelectorAll("[data-auth-tab]").forEach(button => button.classList.toggle("active", button.dataset.authTab === tab));
  document.querySelector("#authLoginPanel").hidden = tab !== "login";
  document.querySelector("#authRegisterPanel").hidden = tab !== "register";
}

function roleBadge(role) {
  const cls = role === "技术负责人" ? "owner" : role === "数据库审核人" ? "reviewer" : "developer";
  return `<span class="role-badge role-${cls}">${escapeHTML(roleLabel(role))}</span>`;
}

function roleCaption(role) {
  if (role === "技术负责人") return "高风险批准与规则管理";
  if (role === "数据库审核人") return "风险复核与变更审批";
  return "变更提交与整改执行";
}

async function renderEnterprise(main) {
  setHeader("企业与成员");
  const admin = Boolean(actor().enterprise_admin);
  try {
    const calls = [api("/api/enterprise"), api("/api/enterprise/members")];
    if (admin) calls.push(api("/api/enterprise/invites"));
    const values = await Promise.all(calls);
    state.organization = values[0];
    state.users = values[1];
    state.invites = values[2] || [];
  } catch (error) {
    main.innerHTML = `<div class="empty-state"><h3>无法读取企业工作空间</h3><p>${escapeHTML(error.message)}</p></div>`;
    return;
  }
  const org = state.organization;
  const members = state.users;
  const invites = state.invites;
  const active = members.filter(item => item.active).length;
  const reviewers = members.filter(item => item.active && item.role !== "后端开发").length;
  main.innerHTML = pageHeading("企业工作空间", "管理企业身份、员工职责和单点登录加入策略，保证提交人与审核人职责分离。", admin ? `<button class="button button-primary" data-invite-create>${svg("plus")}邀请员工</button>` : "") + `
    <section class="enterprise-summary">
      <article class="enterprise-identity"><div class="enterprise-logo">${escapeHTML(initials(org.name))}</div><div><span class="eyebrow">当前企业</span><h3>${escapeHTML(org.name)}</h3><p>${escapeHTML(org.slug)} · ${org.sso_enforced ? "强制单点登录" : "允许密码登录"}</p></div><span class="status status-approved"><i></i>独立空间</span></article>
      <article><span>启用成员</span><strong>${active}</strong><small>管理员 ${members.filter(item => item.enterprise_admin && item.active).length} 人</small></article>
      <article><span>审核力量</span><strong>${reviewers}</strong><small>审核人与技术负责人</small></article>
      <article><span>待接受邀请</span><strong>${invites.filter(item => item.status === "PENDING").length}</strong><small>有效邀请链接</small></article>
    </section>
    <div class="enterprise-layout">
      <section class="panel enterprise-members-panel"><div class="panel-header"><div><h3>成员与职责</h3><p>角色直接决定提交、整改、复核和高风险批准权限。</p></div><span>${members.length} 位成员</span></div>
        <div class="table-wrap"><table class="data-table enterprise-member-table"><thead><tr><th>成员</th><th>企业职责</th><th>登录方式</th><th>最近登录</th><th>状态</th><th>操作</th></tr></thead><tbody>
        ${members.map(member => `<tr class="${member.active ? "" : "member-disabled"}"><td><div class="member-person"><span class="avatar">${escapeHTML(initials(member.name))}</span><div><strong>${escapeHTML(member.name)}</strong><small>${escapeHTML(member.email || "未登记邮箱")}</small></div>${member.enterprise_admin ? "<b>企业管理员</b>" : ""}</div></td><td>${roleBadge(member.role)}<small class="role-caption">${roleCaption(member.role)}</small></td><td><span class="identity-source">${member.identity_provider ? "企业单点登录" : "邮箱密码"}</span></td><td>${member.last_login_at ? formatDate(member.last_login_at) : "尚未登录"}</td><td><span class="status ${member.active ? "status-approved" : "status-draft"}"><i></i>${member.active ? "已启用" : "已停用"}</span></td><td>${admin ? `<button class="button button-small button-secondary" data-member-edit="${member.id}">管理</button>` : '<span class="muted">只读</span>'}</td></tr>`).join("")}
        </tbody></table></div>
      </section>
      <aside class="enterprise-side">
        <section class="panel enterprise-policy-card"><div class="panel-header"><div><h3>企业加入策略</h3><p>域名自动加入只适用于身份平台已验证的企业邮箱。</p></div></div>
          <form id="organizationForm" class="enterprise-settings"><label class="field"><span>企业名称</span><input name="name" value="${escapeHTML(org.name)}" ${admin ? "" : "disabled"}></label><label class="field"><span>企业邮箱域名</span><input name="email_domains" value="${escapeHTML((org.email_domains || []).join("，"))}" ${admin ? "" : "disabled"}></label>
          <label class="enterprise-switch"><input type="checkbox" name="allow_domain_join" ${org.allow_domain_join ? "checked" : ""} ${admin ? "" : "disabled"}><span><strong>允许同域名员工通过单点登录自动加入</strong><small>未受邀员工默认成为后端开发。</small></span></label>
          <label class="enterprise-switch"><input type="checkbox" name="sso_enforced" ${org.sso_enforced ? "checked" : ""} ${admin ? "" : "disabled"}><span><strong>强制企业单点登录（SSO）</strong><small>启用后成员不能使用密码登录。</small></span></label>${admin ? '<button class="button button-secondary button-block" type="submit">保存企业策略</button>' : ""}</form>
        </section>
        <section class="panel invite-list-card"><div class="panel-header"><div><h3>最近邀请</h3><p>邀请令牌只在创建时展示一次。</p></div></div><div class="invite-list">
          ${invites.length ? invites.slice(0,6).map(invite => `<article><div><strong>${escapeHTML(invite.email)}</strong>${roleBadge(invite.role)}</div><p>${invite.status === "PENDING" ? "有效期至 " + formatDate(invite.expires_at) : invite.status === "ACCEPTED" ? "已接受" : "已失效"}</p>${admin && invite.status === "PENDING" ? `<button class="auth-text-button danger" data-invite-revoke="${invite.id}">撤销邀请</button>` : ""}</article>`).join("") : '<div class="invite-empty">暂无邀请记录</div>'}
        </div></section>
      </aside>
    </div>
    <section class="responsibility-grid"><article><b>01</b><h4>后端开发</h4><p>创建变更、提交检查和执行整改，不能审批自己的变更。</p></article><article><b>02</b><h4>数据库审核人</h4><p>派发整改、独立复核，并审批中低风险变更。</p></article><article><b>03</b><h4>技术负责人</h4><p>管理风险规则，并负责高风险变更批准。</p></article><article><b>04</b><h4>企业管理员</h4><p>邀请、停用员工，维护企业域名与单点登录策略。</p></article></section>`;
}

function openInviteModal() {
  document.querySelector("#inviteForm").reset();
  document.querySelector("#inviteResult")?.setAttribute("hidden", "");
  setModalOpen(document.querySelector("#inviteModal"), true);
}
function closeInviteModal() {
  setModalOpen(document.querySelector("#inviteModal"), false);
  document.querySelector("#inviteForm")?.reset();
  document.querySelector("#inviteResult")?.setAttribute("hidden", "");
}
async function openMemberModal(member) {
  if (!member) return;
  try {
    const access = await api("/api/enterprise/members/" + member.id);
    state.editingMember = access.user;
    state.editingMemberAccess = access.application_grants || [];
    const current = access.user;
    document.querySelector("#memberModalTitle").textContent = "管理成员 · " + current.name;
    document.querySelector("#memberModalEmail").textContent = current.email || "未登记邮箱";
    document.querySelector("#memberRole").value = current.role;
    document.querySelector("#memberActive").checked = Boolean(current.active);
    document.querySelector("#memberEnterpriseAdmin").checked = Boolean(current.enterprise_admin);
  document.querySelector("#memberAdminHint").textContent = current.enterprise_admin ? "企业管理员拥有全部服务权限；下方配置会在撤销管理员身份后生效。" : "未勾选的服务不会出现在该成员的工作台。";
    const grantByApp = new Map(state.editingMemberAccess.map(item => [item.application_id, item]));
    document.querySelector("#memberApplicationGrants").innerHTML = state.apps.map(app => {
      const grant = grantByApp.get(app.id) || {};
      return "<article data-grant-app=\"" + escapeHTML(app.id) + "\"><div><strong>" + escapeHTML(app.name) + "</strong><small>" + escapeHTML(app.database) + " / " + escapeHTML(app.schema) + "</small></div><label><input type=\"checkbox\" data-grant-submit " + (grant.can_submit ? "checked" : "") + ">可提交</label><label><input type=\"checkbox\" data-grant-review " + (grant.can_review ? "checked" : "") + ">可审核</label></article>";
  }).join("") || "<p class=\"muted\">请先纳管业务服务。</p>";
    setModalOpen(document.querySelector("#memberModal"), true);
  } catch (error) {
    toast("成员权限读取失败", "error", error.message);
  }
}
function closeMemberModal() {
  setModalOpen(document.querySelector("#memberModal"), false);
  state.editingMember = null;
  state.editingMemberAccess = [];
}

function renderApps(main) {
  setHeader("服务配置");
  const canManage = Boolean(actor().enterprise_admin || actor().role === "技术负责人");
  const actions = canManage ? `<button class="button button-primary" data-app-create>${svg("plus")}纳管服务</button>` : "";
  const content = state.apps.length ? `<div class="app-card-grid">${state.apps.map(app => {
    const resource = app.database ? `${app.database}${app.schema ? ` / ${app.schema}` : ""}` : "未绑定数据库";
    const dependencies = (app.dependencies || []).map(item => state.apps.find(candidate => candidate.id === item)?.name || item);
    return `<article class="app-card service-card"><div class="app-card-head"><span class="app-icon">${svg("server")}</span><span class="status status-approved"><i></i>${escapeHTML(app.lifecycle || "生产运行")}</span></div><div class="service-card-title"><div><h3>${escapeHTML(app.name)}</h3><span>${escapeHTML(app.tier || "T2")} · ${escapeHTML(app.kind || "后端服务")}</span></div></div><p>${escapeHTML(app.description || "暂无业务说明")}</p><div class="app-card-meta"><div><span>运行时</span><strong>${escapeHTML(app.runtime || "待补充")}</strong></div><div><span>负责人</span><strong>${escapeHTML(app.owner || "待分配")}</strong></div><div><span>资源适配器</span><strong>${escapeHTML(resource)}</strong></div><div><span>上游依赖</span><strong>${escapeHTML(dependencies.slice(0,2).join("、") || "未登记")}</strong></div></div>${app.repository_url ? `<div class="repository-link">${svg("code")}<span>${escapeHTML(app.repository_url)}</span></div>` : ""}${canManage ? `<button class="button button-secondary button-small app-edit-button" data-app-edit="${app.id}">维护服务</button>` : ""}</article>`;
  }).join("")}</div>` : `<article class="panel app-onboarding-empty"><span class="app-icon">${svg("apps")}</span><h3>先纳管第一个业务服务</h3><p>登记服务、代码仓库、运行时、依赖和可选数据库资源后，员工才能提交统一研发变更。</p>${canManage ? '<button class="button button-primary" data-app-create>纳管第一个服务</button>' : '<span class="muted">请联系企业管理员或技术负责人完成服务纳管。</span>'}</article>`;
  main.innerHTML = pageHeading("服务配置", "维护服务归属、代码仓库、上下游依赖和治理环境；平台不保存仓库令牌、数据库密码或生产凭据。", actions) + content;
}

function openAppModal(application = null) {
  state.editingApp = application;
  const form = document.querySelector("#appForm");
  form.reset();
  document.querySelector("#appModalTitle").textContent = application ? "维护业务服务" : "纳管业务服务";
  document.querySelector("#saveAppButton").textContent = application ? "保存服务设置" : "确认纳管";
  const dependencySelect = form.elements.namedItem("dependencies");
  const selectedDependencies = new Set(application?.dependencies || []);
  const availableDependencies = state.apps.filter(app => app.id !== application?.id);
  dependencySelect.innerHTML = availableDependencies.length
    ? availableDependencies.map(app => `<option value="${app.id}" ${selectedDependencies.has(app.id) ? "selected" : ""}>${escapeHTML(app.name)} · ${escapeHTML(app.owner || app.kind || "未分配")}</option>`).join("")
    : `<option value="" disabled>请先纳管其他业务服务</option>`;
  dependencySelect.disabled = !availableDependencies.length;
  if (application) {
    for (const key of ["name","owner","kind","runtime","repository_url","tier","lifecycle","database","schema","environment","description"]) {
      const field = form.elements.namedItem(key);
      if (field) {
        const value = application[key] || "";
        if (field.tagName === "SELECT" && value && !Array.from(field.options).some(option => option.value === value)) {
          field.add(new Option(value, value));
        }
        field.value = value;
      }
    }
    form.elements.namedItem("tags").value = (application.tags || []).join("，");
  } else {
    form.elements.namedItem("kind").value = "后端服务";
    form.elements.namedItem("tier").value = "重要";
    form.elements.namedItem("lifecycle").value = "生产运行";
    form.elements.namedItem("environment").value = "生产环境";
  }
  setModalOpen(document.querySelector("#appModal"), true);
}

function closeAppModal() {
  setModalOpen(document.querySelector("#appModal"), false);
  state.editingApp = null;
}
function renderAudits(main) {
  setHeader("审计日志");
  const renderRows = items => items.length ? items.map(item => `<tr><td><code class="audit-id">${escapeHTML(item.id)}</code></td><td>${escapeHTML(item.action)}</td><td>${escapeHTML(item.detail)}</td><td><div class="person-cell"><span class="avatar">${escapeHTML(initials(item.actor_name))}</span><span>${escapeHTML(item.actor_name)}</span></div></td><td>${escapeHTML(item.change_id || "系统")}</td><td>${formatDate(item.created_at)}</td></tr>`).join("") : `<tr><td colspan="6"><div class="table-empty">没有符合条件的审计记录</div></td></tr>`;
  main.innerHTML = pageHeading("操作审计", "记录变更单关键动作、操作人和时间，支持责任链追溯。") + `<article class="panel"><div class="toolbar"><div class="filter-group"><input class="filter-input" id="auditSearch" placeholder="搜索操作、详情、变更单或操作人"></div><span class="result-count" id="auditResultCount">最近 ${state.audits.length} 条记录</span></div><div class="table-wrap"><table class="data-table"><thead><tr><th>事件编号</th><th>动作</th><th>详情</th><th>操作人</th><th>变更单</th><th>时间</th></tr></thead><tbody id="auditRows">${renderRows(state.audits)}</tbody></table></div></article>`;
  document.querySelector("#auditSearch")?.addEventListener("input", event => {
    const keyword = event.target.value.trim().toLowerCase();
    const filtered = state.audits.filter(item => !keyword || [item.id, item.action, item.detail, item.actor_name, item.change_id].join(" ").toLowerCase().includes(keyword));
    document.querySelector("#auditRows").innerHTML = renderRows(filtered);
    document.querySelector("#auditResultCount").textContent = `共 ${filtered.length} 条记录`;
  });
}

function usageBarHTML(label, used, limit) {
  const u = Math.max(0, Number(used) || 0);
  const lim = Math.max(0, Number(limit) || 0);
  const pctVal = lim > 0 ? Math.min(100, Math.round((u / lim) * 100)) : 0;
  const level = pctVal >= 90 ? "danger" : pctVal >= 70 ? "warn" : "ok";
  return `<div class="usage-row">
    <div class="usage-row-head"><span>${escapeHTML(label)}</span><strong>${u} / ${lim || "—"}</strong></div>
    <div class="usage-track" aria-hidden="true"><i class="usage-${level}" style="width:${pctVal}%"></i></div>
  </div>`;
}

async function renderSettings(main) {
  setHeader("集成设置");
  const config = state.config || {};
  const waiting = '<span class="status status-draft"><i></i>待配置</span>';
  const gitlabState = config.gitlab_configured
    ? '<span class="status status-approved"><i></i>Webhook 已就绪</span>'
    : waiting;
  let llmStatus = null;
  try {
    llmStatus = await api("/api/enterprise/llm");
  } catch (_) {
    llmStatus = {
      configured: !!config.llm_configured,
      source: config.llm_source || "none",
      message: config.llm_message || "",
      base_url: config.llm_base_url || "",
      model: config.llm_model || "deepseek-chat",
      api_key_hint: config.llm_api_key_hint || "",
      enabled: !!config.llm_enabled,
    };
  }
  let outboundStatus = null;
  try {
    outboundStatus = await api("/api/enterprise/outbound");
  } catch (_) {
    outboundStatus = {
      configured: !!config.outbound_webhook_configured,
      source: config.outbound_webhook_source || "none",
      message: config.outbound_webhook_message || "",
      url: config.outbound_webhook_url || "",
      enabled: !!config.outbound_webhook_configured,
    };
  }
  let usage = config.llm_usage || null;
  try { usage = await api("/api/enterprise/llm/usage"); } catch (_) { /* keep fallback */ }
  let agentRuntime = null;
  try { agentRuntime = await api("/api/agent-runtime/summary"); } catch (_) { /* optional enterprise gateway */ }
  let agentRuntimeEvents = {events: [], total: 0, truncated: false, verified: false};
  if (agentRuntime) {
    try { agentRuntimeEvents = await api("/api/agent-runtime/events?limit=20"); } catch (_) { /* compatible rolling upgrade */ }
  }

  const modelState = llmStatus.configured
    ? `<span class="status status-approved"><i></i>${llmStatus.source === "platform" ? "演示模型" : "已接入"}</span>`
    : '<span class="status status-failed"><i></i>未接入</span>';
  const outboundState = outboundStatus.configured
    ? `<span class="status status-approved"><i></i>${outboundStatus.source === "platform" ? "平台配置" : "已配置"}</span>`
    : waiting;
  let events = state.integrationEvents || [];
  try {
    events = await api("/api/integrations/events?limit=10");
    state.integrationEvents = events;
  } catch (_) { /* ignore */ }
  const eventRows = (events || []).length
    ? events.map(item => {
        const changeLink = item.change_id
          ? `<button class="panel-link" data-open-change="${escapeHTML(item.change_id)}">${escapeHTML(item.change_id)}</button>`
          : '<span class="muted">—</span>';
        return `<tr>
          <td><code>${escapeHTML(item.provider || "gitlab")}</code><br><small>${escapeHTML(item.event_type || "")}</small></td>
          <td><span class="status ${item.status === "accepted" ? "status-approved" : item.status === "failed" ? "status-failed" : "status-draft"}"><i></i>${escapeHTML(item.status || "—")}</span></td>
          <td><strong>${escapeHTML(item.title || "")}</strong><br><small>${escapeHTML(item.project_path || item.detail || "")}</small></td>
          <td>${changeLink}</td>
          <td>${formatDate(item.created_at)}</td>
        </tr>`;
      }).join("")
    : `<tr><td colspan="5"><div class="table-empty">尚无 GitLab 事件</div></td></tr>`;
  const canEdit = !!(actor()?.enterprise_admin || actor()?.role === "技术负责人");
  let presets = [];
  try { presets = await api("/api/enterprise/llm/presets"); } catch (_) { presets = []; }
  const presetButtons = (presets || []).map(p =>
    `<button type="button" class="button button-secondary button-small" data-llm-preset="${escapeHTML(p.id)}">${escapeHTML(p.name)}</button>`
  ).join("");
  const orgBase = llmStatus.source === "organization" ? (llmStatus.base_url || "") : "";
  const orgModel = llmStatus.source === "organization" && llmStatus.model ? llmStatus.model : "deepseek-chat";
  const orgOutboundURL = outboundStatus.source === "organization" ? (outboundStatus.url || "") : "";
  const usagePanel = usage ? `
    <div class="usage-panel">
      <div class="usage-panel-head"><strong>当日 AI 用量</strong><span>${escapeHTML(usage.day || "今天")}</span></div>
      ${usageBarHTML("当前用户", usage.user_used, usage.user_limit)}
      ${usageBarHTML("本企业", usage.org_used, usage.org_limit)}
      ${usageBarHTML("全站", usage.global_used, usage.global_limit)}
      <p class="field-hint">超出额度后回退到规则分析，不会阻断变更流程。</p>
    </div>` : "";
  const runtimeMetrics = agentRuntime?.metrics || {};
  const auditChain = agentRuntime?.audit_chain || {};
  const metricsState = agentRuntime?.metrics_state || {};
  const runtimeSLO = agentRuntime?.slo || {};
  const runtimeHealthy = !!(agentRuntime?.upstream_ready && auditChain.verified && metricsState.verified !== false && runtimeSLO.status !== "degraded");
  const sloWindowHours = Math.max(1, Math.round(Number(metricsState.window_seconds || 86400) / 3600));
  const sloLabel = runtimeSLO.status === "healthy" ? "SLO 正常" : runtimeSLO.status === "observing" ? "观察中" : "SLO 告警";
  const sloClass = runtimeSLO.status === "healthy" ? "is-healthy" : runtimeSLO.status === "observing" ? "is-observing" : "is-degraded";
  const agentOperationLabel = operation => ({"agent-ask":"Agent 问答","submit-check":"规则提交"}[operation] || operation || "未知操作");
  const agentOutcomeMeta = outcome => outcome === "success" ? ["成功","status-approved"] : outcome === "rejected" ? ["已拒绝","status-waiting"] : outcome === "rate_limited" ? ["已限流","status-waiting"] : ["失败","status-failed"];
  const runtimeEventItems = Array.isArray(agentRuntimeEvents?.events) ? agentRuntimeEvents.events : [];
  const runtimeEventCards = runtimeEventItems.length ? runtimeEventItems.map(item => {
    const outcome = agentOutcomeMeta(item.outcome);
    const change = item.change_id
      ? `<button type="button" class="panel-link agent-runtime-change" data-open-change="${escapeHTML(item.change_id)}">${escapeHTML(item.change_id)}</button>`
      : '<span class="muted">无关联变更</span>';
    const trace = item.trace_id ? escapeHTML(item.trace_id) : "—";
    return `<article class="agent-runtime-event">
      <header><span class="status ${outcome[1]}"><i></i>${outcome[0]}</span><time>${formatDate(item.timestamp)}</time></header>
      <div class="agent-runtime-event-title"><strong>${escapeHTML(agentOperationLabel(item.operation))}</strong>${change}</div>
      <div class="agent-runtime-event-meta">
        <span><b>HTTP</b>${Number(item.http_status || 0) || "—"}</span>
        <span><b>耗时</b>${Number(item.duration_ms || 0)}ms</span>
        <span><b>工具</b>${Number(item.tool_calls || 0)}</span>
        <span><b>模型</b>${escapeHTML(item.model || item.provider || "—")}</span>
      </div>
      <div class="agent-runtime-trace"><span>Trace</span><code title="${trace}">${trace}</code>${item.injection_suspected ? '<b>注入告警</b>' : ""}</div>
    </article>`;
  }).join("") : '<div class="agent-runtime-empty">尚无已完成的受保护操作</div>';
  const runtimePanel = agentRuntime ? `
    <article class="panel agent-runtime-panel">
      <header class="panel-header"><div><h3>Agent 运行保障</h3><p>关键分析请求经限流、注入检测和 HMAC 审计链保护；不记录问题正文、模型回复或密钥。</p></div>
        <span class="status ${runtimeHealthy ? "status-approved" : "status-failed"}"><i></i>${runtimeHealthy ? "受保护" : "需检查"}</span>
      </header>
      <div class="panel-body">
        <div class="agent-runtime-grid">
          <div class="agent-runtime-stat"><span>受保护调用</span><strong>${Number(runtimeMetrics.total || 0)}</strong><small>当前 ${sloWindowHours} 小时固定窗口</small></div>
          <div class="agent-runtime-stat"><span>P95 耗时</span><strong>${Number(runtimeMetrics.p95_duration_ms || 0)}ms</strong><small>最近 512 次调用</small></div>
          <div class="agent-runtime-stat"><span>注入告警</span><strong>${Number(runtimeMetrics.injection_suspected_total || 0)}</strong><small>仅标记，不替代规则结论</small></div>
          <div class="agent-runtime-stat"><span>失败 / 拒绝</span><strong>${Number(runtimeMetrics.failed || 0) + Number(runtimeMetrics.rejected || 0)}</strong><small>上游失败或本地门禁</small></div>
        </div>
        <div class="agent-runtime-slo ${sloClass}">
          <div><span>运行目标</span><strong>${sloLabel}</strong><small>安全拒绝不计入分母；重启保持窗口连续</small></div>
          <dl>
            <div><dt>可用性</dt><dd>${Number(runtimeSLO.availability_percent ?? 100).toFixed(2)}%</dd><small>目标 ${Number(runtimeSLO.availability_target_percent || 99).toFixed(2)}%</small></div>
            <div><dt>P95</dt><dd>${Number(runtimeSLO.p95_duration_ms || 0)}ms</dd><small>目标 ≤ ${Number(runtimeSLO.p95_target_ms || 30000)}ms</small></div>
            <div><dt>有效样本</dt><dd>${Number(runtimeSLO.eligible_requests || 0)}</dd><small>成功 + 服务失败</small></div>
          </dl>
        </div>
        <div class="agent-runtime-foot">
          <span>审计链 <code>${auditChain.verified ? "HMAC 已验证" : "校验失败"}</code></span>
          <span>指标状态 <code>${metricsState.verified === false ? "校验失败" : "HMAC 已持久化"}</code></span>
          <span>指标窗口 <code>${formatDate(metricsState.window_started_at)} → ${formatDate(metricsState.window_ends_at)}</code></span>
          <span>事件 <code>${Number(auditChain.events || 0)}</code></span>
          <span>链尾 <code>${escapeHTML(auditChain.last_hash_prefix || "—")}</code></span>
          <span>限流 <code>${Number(agentRuntime.rate_per_minute || 0)}/分钟 · burst ${Number(agentRuntime.rate_burst || 0)}</code></span>
          <span>启动 <code>${formatDate(agentRuntime.started_at)}</code></span>
        </div>
        <div class="agent-runtime-events-head"><div><h4>最近受保护操作</h4><p>仅展示脱敏运行元数据，可用于故障定位与审计交接。</p></div><button type="button" class="button button-secondary button-small" data-agent-audit-export>导出脱敏 JSON</button></div>
        <div class="agent-runtime-events">${runtimeEventCards}</div>
        ${agentRuntimeEvents?.truncated ? '<p class="field-hint">审计文件较大，当前仅检索最近的安全窗口。</p>' : ""}
      </div>
    </article>` : "";

  const llmForm = canEdit ? `
    <div class="llm-preset-row">
      <span>快捷填充</span>
      <div class="llm-preset-actions">${presetButtons || '<span class="muted">DeepSeek / OpenAI / 内网</span>'}</div>
    </div>
    <form id="orgLlmForm" class="llm-connect-form">
      <label class="field field-check"><input type="checkbox" name="enabled" ${llmStatus.enabled || llmStatus.source === "organization" ? "checked" : ""}> 启用本企业模型</label>
      <label class="field"><span>服务地址</span>
        <input name="base_url" placeholder="https://api.deepseek.com" value="${escapeHTML(orgBase)}">
        <small class="field-hint">OpenAI 兼容；DeepSeek 填 https://api.deepseek.com 即可</small>
      </label>
      <label class="field"><span>模型名</span>
        <input name="model" placeholder="deepseek-chat" value="${escapeHTML(orgModel)}">
      </label>
      <label class="field"><span>API Key</span>
        <div class="input-with-toggle">
          <input name="api_key" type="password" autocomplete="off" placeholder="${llmStatus.api_key_hint ? "已保存 " + escapeHTML(llmStatus.api_key_hint) + "，留空不改" : "sk-..."}">
          <button type="button" class="button button-secondary button-small" data-toggle-key="llm">显示</button>
        </div>
      </label>
      <label class="field"><span>max_tokens</span>
        <input name="max_tokens" type="number" min="100" max="8000" value="${Number(llmStatus.max_tokens || 700)}">
      </label>
      <label class="field field-check"><input type="checkbox" name="clear_api_key"> 清除已保存的 Key</label>
      <div class="form-actions-inline">
        <button class="button button-secondary" type="button" id="llmTestBtn">测试连接</button>
        <button class="button button-primary" type="submit">保存接入</button>
        <span class="muted" id="llmFormHint">${escapeHTML(llmStatus.message || "")}</span>
      </div>
    </form>` : `<p class="setting-callout">${escapeHTML(llmStatus.message || "仅企业管理员可配置模型接入。")}</p>
      <div class="config-row"><span>状态</span><code>${escapeHTML(llmStatus.source || "none")}</code></div>
      <div class="config-row"><span>模型</span><code>${escapeHTML(llmStatus.model || "—")}</code></div>
      <div class="config-row"><span>Key</span><code>${escapeHTML(llmStatus.api_key_hint || "未配置")}</code></div>`;

  const outboundForm = canEdit ? `
    <form id="orgOutboundForm" class="llm-connect-form">
      <label class="field field-check"><input type="checkbox" name="enabled" ${outboundStatus.enabled && outboundStatus.source === "organization" ? "checked" : ""}> 启用本企业出站 Webhook</label>
      <label class="field"><span>Webhook URL</span>
        <input name="url" placeholder="https://ci.example.com/hooks/changeguard" value="${escapeHTML(orgOutboundURL)}">
        <small class="field-hint">审批通过/驳回时 POST JSON；Token 可选</small>
      </label>
      <label class="field"><span>Bearer Token（可选）</span>
        <div class="input-with-toggle">
          <input name="token" type="password" autocomplete="off" placeholder="${outboundStatus.token_hint ? "已保存 " + escapeHTML(outboundStatus.token_hint) + "，留空不改" : "可选"}">
          <button type="button" class="button button-secondary button-small" data-toggle-key="outbound">显示</button>
        </div>
      </label>
      <label class="field field-check"><input type="checkbox" name="clear_token"> 清除已保存的 Token</label>
      <div class="form-actions-inline">
        <button class="button button-secondary" type="button" id="outboundTestBtn">发送探测</button>
        <button class="button button-primary" type="submit">保存出站配置</button>
        <span class="muted" id="outboundFormHint">${escapeHTML(outboundStatus.message || "")}</span>
      </div>
    </form>` : `<p class="setting-callout">${escapeHTML(outboundStatus.message || "仅企业管理员可配置出站通知。")}</p>
      <div class="config-row"><span>来源</span><code>${escapeHTML(outboundStatus.source || "none")}</code></div>
      <div class="config-row"><span>URL</span><code class="wrap-code">${escapeHTML(outboundStatus.url || "—")}</code></div>`;

  const gitlabURL = config.gitlab_webhook_url || "";
  const encConfigured = !!config.data_encryption_configured;

  main.innerHTML = pageHeading("集成", "按企业配置外部能力。新注册工作空间需自行接入 AI，不共用平台测试 Key。") + `
  <article class="panel llm-connect-panel">
    <header class="panel-header"><div><h3>接入 AI</h3><p>只读分析变更证据，不能审批、不能改生产。支持 DeepSeek / OpenAI / 内网兼容接口。</p></div>${modelState}</header>
    <div class="panel-body">
      <div class="llm-status-bar">
        <span>来源：<b>${escapeHTML(llmStatus.source === "platform" ? "平台演示" : llmStatus.source === "organization" ? "本企业" : "未接入")}</b></span>
        <span id="llmStatusMessage">${escapeHTML(llmStatus.message || "")}</span>
      </div>
      ${usagePanel}
      ${llmForm}
    </div>
  </article>
  ${runtimePanel}
  <article class="panel" style="margin-top:16px">
    <header class="panel-header"><div><h3>出站通知 Webhook</h3><p>审批通过/驳回后向下游（Jenkins、自建流水线）POST 事件 JSON。</p></div>${outboundState}</header>
    <div class="panel-body">
      <div class="llm-status-bar">
        <span>来源：<b>${escapeHTML(outboundStatus.source === "platform" ? "平台环境变量" : outboundStatus.source === "organization" ? "本企业" : "未配置")}</b></span>
        <span>${escapeHTML(outboundStatus.message || "")}</span>
      </div>
      ${outboundForm}
      ${outboundStatus.source === "platform" ? '<p class="field-hint">当前走平台 DBGUARD_OUTBOUND_WEBHOOK_URL；保存本企业配置后优先使用企业地址。</p>' : ""}
    </div>
  </article>
  <div class="settings-grid integration-settings" style="margin-top:16px">
    <article class="setting-card"><div class="setting-card-title"><div><h3>GitLab Webhook</h3><p>MR / Push 可自动建草稿。</p></div>${gitlabState}</div>
      <div class="config-row"><span>路径</span><code>${escapeHTML(config.gitlab_webhook_path || "/api/integrations/gitlab/webhook")}</code></div>
      <div class="config-row"><span>URL</span><code class="wrap-code" id="gitlabWebhookURL">${escapeHTML(gitlabURL || "设置 DBGUARD_PUBLIC_URL")}</code></div>
      <div class="form-actions-inline" style="margin-top:12px">
        <button type="button" class="button button-secondary button-small" data-copy-text="${escapeHTML(gitlabURL || config.gitlab_webhook_path || "/api/integrations/gitlab/webhook")}" ${gitlabURL ? "" : "disabled"}>复制 Webhook URL</button>
      </div>
      <p class="field-hint" style="margin-top:10px">Header <code>X-Gitlab-Token</code> 需与 <code>DBGUARD_GITLAB_WEBHOOK_SECRET</code> 一致。</p>
    </article>
    <article class="setting-card"><div class="setting-card-title"><div><h3>Prometheus</h3><p>HTTP 与业务计数。</p></div><span class="status status-approved"><i></i>可用</span></div>
      <div class="config-row"><span>端点</span><code>/metrics</code></div>
      <div class="config-row"><span>含出站成功/失败</span><code>dbguard_outbound_*</code></div>
      <p class="field-hint" style="margin-top:10px">生产需配置 <code>DBGUARD_METRICS_TOKEN</code> 鉴权访问。</p>
    </article>
    <article class="setting-card"><div class="setting-card-title"><div><h3>预发布 Worker</h3><p>异步验证任务。</p></div><span class="status status-approved"><i></i>可用</span></div>
      <div class="config-row"><span>模式</span><code>${escapeHTML(config.experiment_mode || "simulated")}</code></div>
      <div class="config-row"><span>说明</span><span class="muted" style="font-size:13px">${config.experiment_mode === "postgres" ? "影子库真实 SQL 演练" : "确定性模拟验证"}</span></div>
    </article>
    <article class="setting-card"><div class="setting-card-title"><div><h3>数据加密</h3><p>企业 Key / 出站 Token 落库。</p></div>
      ${encConfigured ? '<span class="status status-approved"><i></i>AES-GCM</span>' : '<span class="status status-draft"><i></i>开发兜底</span>'}
    </div>
      <div class="config-row"><span>模式</span><code>${escapeHTML(config.data_encryption_mode || (encConfigured ? "aes-gcm" : "dev-fallback"))}</code></div>
      <div class="config-row"><span>Store</span><code>${escapeHTML(config.store_mode || "—")}</code></div>
      <div class="config-row"><span>Session</span><code>${escapeHTML(config.session_mode || "—")}</code></div>
      ${!encConfigured ? '<p class="field-hint" style="margin-top:10px">生产请设置 <code>DBGUARD_DATA_ENCRYPTION_KEY</code>。</p>' : ""}
    </article>
    <article class="setting-card"><div class="setting-card-title"><div><h3>变更通行证 CP</h3><p>审批通过后签发 Ed25519 短时票。</p></div>
      ${config.passport_enabled ? '<span class="status status-approved"><i></i>已启用</span>' : '<span class="status status-draft"><i></i>不可用</span>'}
    </div>
      <div class="config-row"><span>算法</span><code>${escapeHTML(config.passport_alg || "Ed25519")}</code></div>
      <div class="config-row"><span>Key ID</span><code>${escapeHTML(config.passport_key_id || "—")}</code></div>
      <div class="config-row"><span>公钥</span><code class="wrap-code" style="max-width:220px">${escapeHTML((config.passport_public_key || "").slice(0, 28))}${(config.passport_public_key || "").length > 28 ? "…" : ""}</code></div>
      <div class="form-actions-inline" style="margin-top:12px">
        <button type="button" class="button button-secondary button-small" data-copy-text="${escapeHTML(config.passport_public_key || "")}" ${config.passport_public_key ? "" : "disabled"}>复制公钥</button>
      </div>
      <p class="field-hint" style="margin-top:10px">验签接口 <code>POST /api/passport/verify</code>（免登录，便于 CD）。生产请设置 <code>DBGUARD_PASSPORT_SEED</code>。</p>
    </article>
  </div>
  <article class="panel" style="margin-top:16px">
    <header class="panel-header"><div><h3>最近集成事件</h3><p>GitLab Webhook</p></div><span>${(events || []).length}</span></header>
    <div class="panel-body"><div class="table-wrap"><table class="data-table"><thead><tr><th>来源</th><th>状态</th><th>摘要</th><th>变更单</th><th>时间</th></tr></thead><tbody>${eventRows}</tbody></table></div></div>
  </article>`;

  document.querySelector("[data-agent-audit-export]")?.addEventListener("click", async event => {
    const button = event.currentTarget;
    const original = button.textContent;
    button.disabled = true;
    button.textContent = "正在导出…";
    try {
      const page = await api("/api/agent-runtime/events?limit=100");
      const payload = {
        schema: "changeguard-agent-audit-export/v1",
        exported_at: new Date().toISOString(),
        gateway_version: agentRuntime?.version || "",
        audit_verified: !!page.verified,
        total_audit_events: Number(page.total || 0),
        slo: agentRuntime?.slo || {},
        metrics_state: agentRuntime?.metrics_state || {},
        events: Array.isArray(page.events) ? page.events : [],
      };
      const blob = new Blob([JSON.stringify(payload, null, 2)], {type:"application/json"});
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = `changeguard-agent-audit-${new Date().toISOString().slice(0,10)}.json`;
	  anchor.style.display = "none";
	  document.body.append(anchor);
      anchor.click();
	  window.setTimeout(() => {
	    anchor.remove();
	    URL.revokeObjectURL(url);
	  }, 30000);
      toast("脱敏审计记录已导出", "success", `${payload.events.length} 条操作记录`);
    } catch (error) {
      toast("导出失败", "error", error.message || "无法读取 Agent 审计记录");
    } finally {
      button.disabled = false;
      button.textContent = original;
    }
  });

  const form = document.querySelector("#orgLlmForm");
  const readLlmForm = () => {
    const data = new FormData(form);
    return {
      enabled: data.get("enabled") === "on",
      base_url: String(data.get("base_url") || "").trim(),
      model: String(data.get("model") || "").trim() || "deepseek-chat",
      api_key: String(data.get("api_key") || "").trim(),
      max_tokens: Number(data.get("max_tokens") || 700),
      clear_api_key: data.get("clear_api_key") === "on",
    };
  };
  document.querySelectorAll("[data-llm-preset]").forEach(btn => {
    btn.addEventListener("click", () => {
      const id = btn.getAttribute("data-llm-preset");
      const preset = (presets || []).find(item => item.id === id);
      if (!preset || !form) return;
      form.elements.namedItem("enabled").checked = true;
      form.elements.namedItem("base_url").value = preset.base_url || "";
      form.elements.namedItem("model").value = preset.model || "";
      form.elements.namedItem("max_tokens").value = Number(preset.max_tokens || 700);
      const hint = document.querySelector("#llmFormHint");
      if (hint) hint.textContent = preset.hint || "请粘贴 API Key 后测试连接";
      toast("已填充 " + (preset.name || id), "success", "请填写 API Key");
    });
  });
  document.querySelectorAll("[data-toggle-key]").forEach(btn => {
    btn.addEventListener("click", event => {
      const which = event.currentTarget.getAttribute("data-toggle-key");
      const input = which === "outbound"
        ? document.querySelector("#orgOutboundForm")?.elements?.namedItem("token")
        : form?.elements?.namedItem("api_key");
      if (!input) return;
      const show = input.type === "password";
      input.type = show ? "text" : "password";
      event.currentTarget.textContent = show ? "隐藏" : "显示";
    });
  });
  document.querySelector("#llmTestBtn")?.addEventListener("click", async () => {
    if (!form) return;
    const body = readLlmForm();
    const hint = document.querySelector("#llmFormHint");
    const statusMsg = document.querySelector("#llmStatusMessage");
    try {
      if (hint) hint.textContent = "测试中…";
      const result = await api("/api/enterprise/llm/test", { method: "POST", body: JSON.stringify(body) });
      const text = result.ok
        ? (result.message + (result.provider_reply ? " · 回复 " + result.provider_reply : ""))
        : (result.message || "连接失败");
      if (hint) hint.textContent = text;
      if (statusMsg) statusMsg.textContent = text;
      toast(result.ok ? "连通正常" : "连接失败", result.ok ? "success" : "error", result.message || "");
    } catch (error) {
      if (hint) hint.textContent = error.message || "测试失败";
      toast(error.message || "测试失败", "error");
    }
  });
  if (form) {
    form.addEventListener("submit", async event => {
      event.preventDefault();
      const body = readLlmForm();
      const hint = document.querySelector("#llmFormHint");
      try {
        if (hint) hint.textContent = "保存中…";
        if (body.enabled && body.base_url && (body.api_key || llmStatus.api_key_hint)) {
          const probe = await api("/api/enterprise/llm/test", { method: "POST", body: JSON.stringify(body) });
          if (!probe.ok) {
            if (hint) hint.textContent = "保存前检测失败：" + (probe.message || "");
            toast("连接失败，未保存", "error", probe.message || "");
            return;
          }
        }
        const saved = await api("/api/enterprise/llm", { method: "PUT", body: JSON.stringify(body) });
        toast("模型接入已保存", "success", saved.configured ? "提交变更时可生成模型辅助分析" : "已关闭企业模型");
        if (state.config) {
          state.config.llm_configured = !!saved.configured;
          state.config.llm_source = saved.source;
          state.config.llm_message = saved.message;
          state.config.llm_model = saved.model;
        }
        await renderSettings(main);
      } catch (error) {
        if (hint) hint.textContent = error.message || "保存失败";
        toast(error.message || "保存失败", "error");
      }
    });
  }
  const readOutboundForm = () => {
    const el = document.querySelector("#orgOutboundForm");
    if (!el) return { enabled: false, url: "", token: "", clear_token: false };
    const data = new FormData(el);
    return {
      enabled: data.get("enabled") === "on",
      url: String(data.get("url") || "").trim(),
      token: String(data.get("token") || "").trim(),
      clear_token: data.get("clear_token") === "on",
    };
  };
  document.querySelector("#outboundTestBtn")?.addEventListener("click", async () => {
    const body = readOutboundForm();
    const hint = document.querySelector("#outboundFormHint");
    try {
      if (hint) hint.textContent = "发送探测中…";
      const result = await api("/api/enterprise/outbound/test", { method: "POST", body: JSON.stringify(body) });
      const text = result.ok
        ? `${result.message}${result.duration_ms != null ? " · " + result.duration_ms + "ms" : ""}`
        : (result.message || "探测失败");
      if (hint) hint.textContent = text;
      toast(result.ok ? "出站探测成功" : "出站探测失败", result.ok ? "success" : "error", result.message || "");
    } catch (error) {
      if (hint) hint.textContent = error.message || "探测失败";
      toast(error.message || "探测失败", "error");
    }
  });
  const outboundFormEl = document.querySelector("#orgOutboundForm");
  if (outboundFormEl) {
    outboundFormEl.addEventListener("submit", async event => {
      event.preventDefault();
      const body = readOutboundForm();
      const hint = document.querySelector("#outboundFormHint");
      try {
        if (hint) hint.textContent = "保存中…";
        const saved = await api("/api/enterprise/outbound", { method: "PUT", body: JSON.stringify(body) });
        toast("出站 Webhook 已保存", "success", saved.source === "organization" ? "使用本企业配置" : saved.message || "");
        if (state.config) {
          state.config.outbound_webhook_configured = !!saved.configured;
          state.config.outbound_webhook_source = saved.source;
          state.config.outbound_webhook_message = saved.message;
          state.config.outbound_webhook_url = saved.url;
        }
        await renderSettings(main);
      } catch (error) {
        if (hint) hint.textContent = error.message || "保存失败";
        toast(error.message || "保存失败", "error");
      }
    });
  }
  document.querySelectorAll("[data-copy-text]").forEach(btn => {
    btn.addEventListener("click", async () => {
      const text = btn.getAttribute("data-copy-text") || "";
      if (!text) return;
      try {
        await navigator.clipboard.writeText(text);
        toast("已复制", "success", text.slice(0, 80));
      } catch (_) {
        const ta = document.createElement("textarea");
        ta.value = text;
        document.body.appendChild(ta);
        ta.select();
        document.execCommand("copy");
        ta.remove();
        toast("已复制", "success", text.slice(0, 80));
      }
    });
  });
}

function defaultPlannedAtForApp(applicationId) {
  // 同服务窗口保护 90 分钟：按已有未闭环变更错开 2 小时，起点 36h 后
  const active = (state.changes || []).filter(c =>
    c.application_id === applicationId &&
    !["COMPLETED", "REJECTED"].includes(c.status)
  );
  const hours = 36 + active.length * 2;
  const date = new Date(Date.now() + hours * 3600 * 1000);
  date.setMinutes(date.getMinutes() - date.getTimezoneOffset());
  return date.toISOString().slice(0, 16);
}

function setModalOpen(modal, open) {
  if (!modal) return;
  modal.classList.toggle("open", !!open);
  modal.setAttribute("aria-hidden", open ? "false" : "true");
  document.body.classList.toggle("modal-open", !!document.querySelector(".modal-layer.open"));
}

function openCreate(change = null) {
  const form = document.querySelector("#createForm");
  if (!form) return;
  state.editingId = change?.id || null;
  const titleEl = document.querySelector("#createTitle");
  const saveBtn = document.querySelector("#saveChangeButton");
  if (titleEl) titleEl.textContent = change ? "编辑变更" : "新建变更";
  if (saveBtn) {
    saveBtn.textContent = change ? "保存修改" : "保存变更单";
    saveBtn.disabled = false;
  }
  if (change) {
    form.elements.namedItem("title").value = change.title || "";
    form.elements.namedItem("application_id").value = change.application_id || "";
    form.elements.namedItem("change_type").value = change.change_type || "配置变更";
    form.elements.namedItem("environment").value = change.environment || "预发布环境";
    form.elements.namedItem("description").value = change.description || "";
    form.elements.namedItem("repository_url").value = change.repository_url || "";
    form.elements.namedItem("branch").value = change.branch || "main";
    form.elements.namedItem("commit_sha").value = change.commit_sha || "";
    form.elements.namedItem("sql").value = change.sql || "";
    form.elements.namedItem("rollback_sql").value = change.rollback_sql || "";
    form.elements.namedItem("rollback_plan").value = change.rollback_plan || change.rollback_sql || "";
    const artifacts = change.artifacts || [];
    const artifactContent = kind => artifacts.find(item => item.kind === kind)?.content || "";
    form.elements.namedItem("code_diff").value = artifactContent("CODE");
    form.elements.namedItem("config_diff").value = artifactContent("CONFIG");
    form.elements.namedItem("kubernetes_manifest").value = artifactContent("KUBERNETES");
    form.elements.namedItem("api_diff").value = artifactContent("API");
    const release = change.release_plan || {};
    form.elements.namedItem("deployment_strategy").value = release.strategy || "金丝雀发布";
    form.elements.namedItem("canary_percent").value = Number(release.canary_percent ?? 10);
    form.elements.namedItem("observation_minutes").value = Number(release.observation_minutes ?? 30);
    form.elements.namedItem("success_metrics").value = (release.success_metrics || []).join(", ") || "HTTP 5xx, P99 延迟, 核心业务成功率";
    form.elements.namedItem("auto_rollback").checked = release.auto_rollback !== false;
    const date = new Date(change.planned_at);
    if (!Number.isNaN(date.getTime())) {
      date.setMinutes(date.getMinutes() - date.getTimezoneOffset());
      form.elements.namedItem("planned_at").value = date.toISOString().slice(0,16);
    }
  } else {
    form.reset();
    form.elements.namedItem("environment").value = "预发布环境";
    form.elements.namedItem("change_type").value = "配置变更";
    form.elements.namedItem("branch").value = "main";
    form.elements.namedItem("deployment_strategy").value = "金丝雀发布";
    form.elements.namedItem("canary_percent").value = 10;
    form.elements.namedItem("observation_minutes").value = 30;
    form.elements.namedItem("success_metrics").value = "HTTP 5xx, P99 延迟, 核心业务成功率";
    form.elements.namedItem("auto_rollback").checked = true;
    form.elements.namedItem("rollback_plan").value = "回滚到上一稳定版本；核对核心接口错误率与延迟。";
    form.elements.namedItem("description").value = "预发布验证配置调整：观察错误率、延迟与核心成功率。";
    form.elements.namedItem("config_diff").value = "service: demo\nenvironment: staging\nhttp:\n  read_timeout_ms: 1200\nlog_level: INFO\n";
    form.elements.namedItem("commit_sha").value = Array.from(crypto.getRandomValues(new Uint8Array(16))).map(b => b.toString(16).padStart(2, "0")).join("").slice(0, 32);
    const [route, assetID] = currentRoute();
    if (route === "assets" && assetID && state.apps.some(app => app.id === assetID)) {
      form.elements.namedItem("application_id").value = assetID;
    } else if (state.apps?.[0]) {
      form.elements.namedItem("application_id").value = state.apps[0].id;
    }
    const selected = state.apps.find(app => app.id === form.elements.namedItem("application_id").value);
    if (selected?.repository_url) form.elements.namedItem("repository_url").value = selected.repository_url;
    const appName = selected?.name || "服务";
    form.elements.namedItem("title").value = `${appName} 配置变更 · 预发布`;
    form.elements.namedItem("planned_at").value = defaultPlannedAtForApp(form.elements.namedItem("application_id").value);
  }
  // 切换服务时重算计划时间 / 标题（仅新建）
  const appSelect = form.elements.namedItem("application_id");
  appSelect.onchange = () => {
    if (state.editingId) return;
    const app = state.apps.find(a => a.id === appSelect.value);
    if (app?.repository_url) {
      form.elements.namedItem("repository_url").value = app.repository_url;
    }
    form.elements.namedItem("planned_at").value = defaultPlannedAtForApp(appSelect.value);
    const titleInput = form.elements.namedItem("title");
    if (titleInput && (!titleInput.dataset.touched || titleInput.value.endsWith("配置变更 · 预发布"))) {
      titleInput.value = `${app?.name || "服务"} 配置变更 · 预发布`;
    }
  };
  form.elements.namedItem("title").oninput = () => {
    form.elements.namedItem("title").dataset.touched = "1";
  };
  // 生产 + 全量 → 自动改金丝雀，避免无感阻断
  const envSelect = form.elements.namedItem("environment");
  const strategySelect = form.elements.namedItem("deployment_strategy");
  const guardProd = () => {
    if (String(envSelect.value).includes("生产") && strategySelect.value === "全量发布") {
      strategySelect.value = "金丝雀发布";
      toast("生产环境已自动改为金丝雀发布", "success", "全量发布会被规则阻断");
    }
  };
  envSelect.onchange = guardProd;
  strategySelect.onchange = guardProd;

  setModalOpen(document.querySelector("#createModal"), true);
  requestAnimationFrame(() => form.elements.namedItem("title")?.focus({ preventScroll: true }));
  const input = form.elements.namedItem("planned_at");
  if (input && !input.value) {
    input.value = defaultPlannedAtForApp(form.elements.namedItem("application_id").value);
  }
}
function closeCreate() {
  setModalOpen(document.querySelector("#createModal"), false);
  document.querySelector("#createForm")?.reset();
  state.editingId = null;
  const title = document.querySelector("#createTitle");
  if (title) title.textContent = "新建变更";
  const saveBtn = document.querySelector("#saveChangeButton");
  if (saveBtn) {
    saveBtn.textContent = "保存变更单";
    saveBtn.disabled = false;
  }
}
function openReview(action, id) {
  state.review = {action,id};
  const approve = action === "approve";
  const reviewTitle = document.querySelector("#reviewTitle");
  if (reviewTitle) reviewTitle.textContent = approve ? "批准研发变更" : "驳回研发变更";
  const reviewDescription = document.querySelector("#reviewDescription");
  if (reviewDescription) reviewDescription.textContent = approve ? "确认已核对全部证据，并填写执行约束。" : "请填写明确、可执行的驳回原因。";
  const button = document.querySelector("#reviewSubmitButton");
  if (button) {
    button.textContent = approve ? "确认批准" : "确认驳回";
    button.className = approve ? "button button-primary" : "button button-danger";
  }
  const form = document.querySelector("#reviewForm");
  form?.reset();
  setModalOpen(document.querySelector("#reviewModal"), true);
  requestAnimationFrame(() => form?.elements?.namedItem("comment")?.focus({ preventScroll: true }));
}
function closeReview() {
  setModalOpen(document.querySelector("#reviewModal"), false);
  document.querySelector("#reviewForm")?.reset();
  state.review = null;
}
function openFindingAction(button) {
  const action = button.dataset.findingAction;
  const findingId = button.dataset.findingId;
  const changeId = button.dataset.changeId;
  const approved = button.dataset.approved === "true";
  const finding = (state.currentChange?.findings || []).find(item => item.id === findingId) || {};
  state.findingAction = {action, findingId, changeId, approved};

  const assignMode = action === "assign";
  document.querySelector("#findingOwnerField").classList.toggle("is-hidden", !assignMode);
  document.querySelector("#findingDueField").classList.toggle("is-hidden", !assignMode);
  document.querySelector("#findingContentField").classList.toggle("is-hidden", assignMode);

  const ownerSelect = document.querySelector("#findingOwner");
  ownerSelect.innerHTML = state.users.map(user => '<option value="' + user.id + '">' + escapeHTML(user.name) + ' · ' + escapeHTML(user.role) + '</option>').join("");
  ownerSelect.value = finding.owner_id || state.currentChange?.submitter_id || state.users[0]?.id || "";

  const dueAt = document.querySelector("#findingDueAt");
  const dueDate = finding.due_at ? new Date(finding.due_at) : new Date(Date.now() + 72 * 3600 * 1000);
  dueDate.setMinutes(dueDate.getMinutes() - dueDate.getTimezoneOffset());
  dueAt.value = dueDate.toISOString().slice(0, 16);

  const title = document.querySelector("#findingTitle");
  const description = document.querySelector("#findingDescription");
  const content = document.querySelector("#findingContent");
  const contentLabel = document.querySelector("#findingContentLabel");
  const submit = document.querySelector("#findingSubmitButton");

  if (assignMode) {
    title.textContent = "派发风险整改任务";
    description.textContent = "指定实际负责人和 SLA 截止时间。";
    submit.textContent = "确认派单";
    submit.className = "button button-primary";
  } else if (action === "resolve") {
    title.textContent = "提交整改结果";
    description.textContent = "说明采用的规避措施、验证方式和剩余风险。";
    contentLabel.innerHTML = "整改说明 <b>*</b>";
    content.placeholder = "例如：已将全表更新改为按主键分批执行，并在预发布环境核对影响行数";
    content.value = finding.resolution || "";
    submit.textContent = "提交复核";
    submit.className = "button button-primary";
  } else {
    title.textContent = approved ? "复核通过风险整改" : "退回风险整改";
    description.textContent = approved ? "确认整改证据有效，风险项将进入闭环状态。" : "填写明确的退回原因和补充要求。";
    contentLabel.innerHTML = "复核意见 " + (approved ? "" : "<b>*</b>");
    content.placeholder = approved ? "可填写复核依据和上线约束" : "说明证据缺口或需要补充的整改内容";
    content.value = "";
    submit.textContent = approved ? "确认复核通过" : "确认退回";
    submit.className = approved ? "button button-primary" : "button button-danger";
  }

  setModalOpen(document.querySelector("#findingModal"), true);
}

function closeFinding() {
  setModalOpen(document.querySelector("#findingModal"), false);
  document.querySelector("#findingForm")?.reset();
  state.findingAction = null;
}
function toast(message, type = "success", detail = "") {
  const element = document.createElement("div");
  element.className = "toast " + (type === "error" ? "error" : "");
  element.innerHTML = `<span class="toast-icon">${type === "error" ? "!" : "✓"}</span><div><strong>${escapeHTML(message)}</strong>${detail ? `<span>${escapeHTML(detail)}</span>` : ""}</div>`;
  document.querySelector("#toastStack").appendChild(element);
  setTimeout(() => element.remove(), 3800);
}
async function refreshData(render = true) {
  const [users, apps, dashboard, changesRaw, policies, audits, config] = await Promise.all([
    api("/api/users"), api("/api/apps"), api("/api/dashboard"),
    api("/api/changes?page=1&page_size=100"),
    api("/api/policies"), api("/api/audits?limit=100"), api("/api/config/status")
  ]);
  const changes = Array.isArray(changesRaw) ? changesRaw : (changesRaw?.items || []);
  Object.assign(state, {users, apps, dashboard, changes, policies, audits, config});
  if (!users.some(user => user.id === state.actorId)) state.actorId = users[0]?.id || "";
  renderActor();
  renderNav();
  updateNotificationBadge();
  if (render) await renderPage();
}

function pollChange(id) {
  let count = 0;
  const timer = setInterval(async () => {
    count++;
    try {
      const change = await api("/api/changes/" + id);
      state.currentChange = change;
      // 同步列表里的同一条，便于通知铃/概览
      const idx = (state.changes || []).findIndex(item => item.id === id);
      if (idx >= 0) state.changes[idx] = change;
      if (!["EXPERIMENT_QUEUED","EXPERIMENT_RUNNING"].includes(change.status) || count > 18) {
        clearInterval(timer);
        await refreshData(false);
        if (currentRoute()[1] === id) await renderPage();
        const done = change.status === "WAITING_APPROVAL";
        const failed = change.status === "CHECK_FAILED";
        toast(
          done ? "预发布验证已完成" : failed ? "预发布验证未通过" : "验证任务状态已更新",
          failed ? "error" : "success",
          statusInfo(change.status)[0]
        );
      } else if (currentRoute()[1] === id) {
        // 排队/执行中刷新详情按钮区
        await renderPage();
      }
    } catch (_) { clearInterval(timer); }
  }, 1000);
}

async function performAction(action, id) {
  if (!action || !id) {
    toast("操作无效", "error", "缺少变更单或动作");
    return;
  }
  const labels = {
    submit: "提交规则检查",
    experiment: "开始预发布验证",
    complete: "标记执行完成",
  };
  const label = labels[action] || action;
  document.querySelectorAll(`[data-action="${action}"][data-id="${id}"]`).forEach(btn => {
    btn.disabled = true;
    btn.setAttribute("aria-busy", "true");
  });
  try {
    const result = await api("/api/changes/" + id + "/" + action, { method: "POST", body: "{}" });
    await refreshData(false);
    const idx = (state.changes || []).findIndex(item => item.id === id);
    if (idx >= 0) state.changes[idx] = result;
    if (state.currentChange?.id === id) state.currentChange = result;
    if (action === "submit" && result.status === "CHECK_FAILED") {
      const n = (result.findings || []).filter(f => f.severity === "HIGH" || f.blocking).length || (result.findings || []).length;
      toast("规则检查未通过", "error", `命中 ${n} 项问题，请在「确定性规则检查」中查看并修改后重提`);
    } else if (action === "submit" && result.status === "READY_FOR_EXPERIMENT") {
      toast("规则检查通过", "success", "可以点击「开始预发布验证」");
    } else {
      toast(label + "成功", "success", statusInfo(result.status)[0] + " · " + result.id);
    }
    if (action === "experiment") pollChange(id);
    await renderPage();
    updateNotificationBadge();
  } catch (error) {
    toast(label + "失败", "error", error.message || "请稍后重试");
    document.querySelectorAll(`[data-action="${action}"][data-id="${id}"]`).forEach(btn => {
      btn.disabled = false;
      btn.removeAttribute("aria-busy");
    });
  }
}
function connectEvents() {
  if (!window.EventSource) return;
  const source = new EventSource("/api/events");
  source.addEventListener("change", async () => {
    try {
      await refreshData(false);
      const route = currentRoute();
      if (["dashboard","panorama","assets","risks","calendar","approvals","experiments","observability","incidents"].includes(route[0]) || route[1]) await renderPage();
    } catch (_) {}
  });
  source.onerror = async () => {
    if (!state.authStatus?.enabled) return;
    try {
      const response = await fetch("/api/auth/session", {headers:{"Accept":"application/json"}, cache:"no-store"});
      if (response.status === 401) {
        source.close();
        state.session = null;
        state.organization = null;
        renderAuthGate("登录状态已失效，请重新登录");
      }
    } catch (_) {}
  };
}
function hideAuthGate() {
  document.body.classList.remove("auth-required");
  const gate = document.querySelector("#authGate");
  if (gate) { gate.hidden = true; gate.innerHTML = ""; }
}

function showIdentityError(form, message) {
  const card = form.closest(".auth-card");
  if (!card) return;
  let notice = card.querySelector(":scope > .auth-message.error");
  if (!notice) {
    notice = document.createElement("div");
    notice.className = "auth-message error";
    const tabs = card.querySelector(":scope > .auth-tabs");
    if (tabs) tabs.insertAdjacentElement("afterend", notice);
    else form.insertAdjacentElement("beforebegin", notice);
  }
  notice.textContent = message;
  const password = form.querySelector('input[name="password"]');
  if (password) {
    password.value = "";
    password.focus({preventScroll:true});
  }
}

async function handleIdentityForm(form) {
  const button = form.querySelector('button[type="submit"]');
  const original = button?.textContent || "";
  if (button) { button.disabled = true; button.textContent = "正在处理…"; }
  try {
    const values = Object.fromEntries(new FormData(form).entries());
    let endpoint = "/api/auth/login";
    if (form.id === "enterpriseRegisterForm") endpoint = "/api/auth/register";
    if (form.id === "acceptInviteForm") endpoint = "/api/auth/invitations/accept";
    const session = await api(endpoint, {method:"POST", body:JSON.stringify(values)});
    state.session = session;
    state.organization = session.organization;
    state.actorId = session.user.id;
    localStorage.setItem("dbguard_actor", state.actorId);
    location.hash = "#/dashboard";
    location.reload();
  } catch (error) {
    showIdentityError(form, error.message);
  } finally {
    if (button && document.contains(button)) { button.disabled = false; button.textContent = original; }
  }
}

async function createEnterpriseInvite(form) {
  const button = form.querySelector('button[type="submit"]');
  const original = button?.textContent || "生成邀请链接";
  if (button) {
    button.disabled = true;
    button.textContent = "正在生成…";
  }

  try {
    const values = new FormData(form);
    const payload = {
      email: String(values.get("email") || "").trim(),
      role: String(values.get("role") || "").trim(),
      expires_in_hours: Number(values.get("expires_in") || 72),
    };
    const created = await api("/api/enterprise/invites", {
      method: "POST",
      body: JSON.stringify(payload),
    });
    const inviteURL = String(created?.invite_url || "");
    if (!inviteURL) throw new Error("服务器未返回邀请链接");

    const input = document.querySelector("#inviteURL");
    const result = document.querySelector("#inviteResult");
    if (input) input.value = inviteURL;
    if (result) result.hidden = false;
    if (created?.invite) {
      state.invites = [
        created.invite,
        ...(state.invites || []).filter(item => item.id !== created.invite.id),
      ];
    }
    toast("邀请链接已生成", "success", payload.email);
  } catch (error) {
    toast("邀请创建失败", "error", error.message);
  } finally {
    if (button && document.contains(button)) {
      button.disabled = false;
      button.textContent = original;
    }
  }
}

async function revokeEnterpriseInvite(id) {
  try {
    await api("/api/enterprise/invites/" + encodeURIComponent(id), {method: "DELETE"});
    await renderPage();
    toast("邀请已撤销");
  } catch (error) {
    toast("邀请撤销失败", "error", error.message);
  }
}

async function saveOrganization(form) {
  const values = new FormData(form);
  const payload = {
    name: String(values.get("name") || "").trim(),
    email_domains: String(values.get("email_domains") || "")
      .split(/[，,]/)
      .map(item => item.trim())
      .filter(Boolean),
    allow_domain_join: Boolean(form.elements.namedItem("allow_domain_join")?.checked),
    sso_enforced: Boolean(form.elements.namedItem("sso_enforced")?.checked),
  };
  const button = form.querySelector('button[type="submit"]');
  const original = button?.textContent || "保存企业策略";
  if (button) {
    button.disabled = true;
    button.textContent = "正在保存…";
  }

  try {
    state.organization = await api("/api/enterprise", {
      method: "PUT",
      body: JSON.stringify(payload),
    });
    await renderPage();
    toast("企业策略已保存");
  } catch (error) {
    toast("企业策略保存失败", "error", error.message);
  } finally {
    if (button && document.contains(button)) {
      button.disabled = false;
      button.textContent = original;
    }
  }
}

async function saveEnterpriseMember(form) {
  const member = state.editingMember;
  if (!member?.id) {
    toast("成员信息已失效", "error", "请关闭弹窗后重新打开");
    return;
  }

  const applicationGrants = Array.from(
    document.querySelectorAll("#memberApplicationGrants [data-grant-app]"),
  ).map(item => ({
    application_id: item.dataset.grantApp,
    can_submit: Boolean(item.querySelector("[data-grant-submit]")?.checked),
    can_review: Boolean(item.querySelector("[data-grant-review]")?.checked),
  })).filter(item => item.can_submit || item.can_review);
  const payload = {
    role: String(form.elements.namedItem("role")?.value || ""),
    active: Boolean(form.elements.namedItem("active")?.checked),
    enterprise_admin: Boolean(form.elements.namedItem("enterprise_admin")?.checked),
    application_grants: applicationGrants,
  };
  const button = form.querySelector('button[type="submit"]');
  const original = button?.textContent || "保存成员设置";
  if (button) {
    button.disabled = true;
    button.textContent = "正在保存…";
  }

  try {
    await api("/api/enterprise/members/" + encodeURIComponent(member.id), {
      method: "PUT",
      body: JSON.stringify(payload),
    });
    closeMemberModal();
    await renderPage();
    toast("成员设置已保存");
  } catch (error) {
    toast("成员设置保存失败", "error", error.message);
  } finally {
    if (button && document.contains(button)) {
      button.disabled = false;
      button.textContent = original;
    }
  }
}

async function saveManagedApplication(form) {
  const values = new FormData(form);
  const payload = Object.fromEntries(values.entries());
  payload.dependencies = values.getAll("dependencies").map(item => String(item).trim()).filter(Boolean);
  payload.tags = String(payload.tags || "").split(/[，,]/).map(item => item.trim()).filter(Boolean);
  const button = document.querySelector("#saveAppButton");
  const editing = state.editingApp;
  button.disabled = true; button.textContent = "正在保存…";
  try {
    const saved = await api(editing ? "/api/apps/" + editing.id : "/api/apps", {
      method: editing ? "PUT" : "POST",
      body: JSON.stringify(payload)
    });
    closeAppModal();
    await refreshData(false);
    await renderApps(document.querySelector("#mainContent"));
    toast(editing ? "服务设置已更新" : "服务已纳入治理", "success", saved.name + " · " + (saved.runtime || saved.kind || "业务服务"));
  } catch (error) {
    toast("服务保存失败", "error", error.message);
  } finally {
    if (button && document.contains(button)) {
      button.disabled = false;
      button.textContent = editing ? "保存服务设置" : "确认纳管";
    }
  }
}

function closeAllOverlays() {
  closeCreate();
  closeReview();
  closeFinding();
  closePolicy();
  closeInviteModal();
  closeMemberModal();
  closeAppModal();
  closeNotifyPanel();
  document.body.classList.remove("sidebar-open");
}

function bindEvents() {
  document.addEventListener("click", event => {
    // 通知面板：开关 / 关闭 / 点外部收起
    if (event.target.closest("#notificationButton")) {
      event.preventDefault();
      event.stopPropagation();
      toggleNotifyPanel();
      return;
    }
    if (event.target.closest("[data-notify-close]")) {
      closeNotifyPanel();
      return;
    }
    if (!event.target.closest("#notifyPanel") && !event.target.closest("#notificationButton")) {
      closeNotifyPanel();
    }

    const routeButton = event.target.closest("[data-route]");
    if (routeButton && !routeButton.disabled && !routeButton.hasAttribute("disabled")) {
      event.preventDefault();
      closeAllOverlays();
      location.hash = "#/" + routeButton.dataset.route;
      return;
    }
    // 整行打开变更；仅当点到带业务动作的按钮时不跳转
    const row = event.target.closest("[data-open-change]");
    if (row) {
      const actionBtn = event.target.closest(
        "button[data-action], button[data-review], button[data-finding-action], button[data-export], button[data-edit], a[href]"
      );
      if (!actionBtn) {
        closeNotifyPanel();
        location.hash = "#/changes/" + row.dataset.openChange;
        return;
      }
    }

    if (event.target.closest("[data-create]") || event.target.closest("#createButton")) {
      event.preventDefault();
      openCreate();
      return;
    }
    const edit = event.target.closest("[data-edit]");
    if (edit && state.currentChange) { openCreate(state.currentChange); return; }
    // 关闭新建弹窗：点 X / 取消 / 遮罩（遮罩可能被 dialog 挡住，X 必须可靠）
    if (event.target.closest("#createModal [data-close-modal], #createModal .modal-header .icon-button, #createModal .modal-footer .button-secondary")) {
      closeCreate();
      return;
    }
    if (event.target.closest("[data-close-modal]")) { closeCreate(); return; }
    if (event.target.closest("[data-close-review]")) { closeReview(); return; }
    if (event.target.closest("[data-close-finding]")) { closeFinding(); return; }
    if (event.target.closest("[data-close-policy]")) { closePolicy(); return; }
    if (event.target.closest("[data-policy-create]")) { openPolicy(); return; }
    const policyEdit = event.target.closest("[data-policy-edit]");
    if (policyEdit) { openPolicy(state.policies.find(item => item.id === policyEdit.dataset.policyEdit)); return; }
    const policyToggle = event.target.closest("[data-policy-toggle]");
    if (policyToggle) { togglePolicy(policyToggle.dataset.policyToggle); return; }
    if (event.target.closest("[data-policy-export]")) { exportPolicies(); return; }
    const findingButton = event.target.closest("[data-finding-action]");
    if (findingButton) { openFindingAction(findingButton); return; }
    const exportButton = event.target.closest("[data-export]");
    if (exportButton) { exportReport(exportButton.dataset.id, exportButton.dataset.format || "xlsx"); return; }
    const action = event.target.closest("[data-action]");
    if (action && !action.disabled && !action.hasAttribute("disabled")) {
      event.preventDefault();
      event.stopPropagation();
      performAction(action.dataset.action, action.dataset.id);
      return;
    }
    const review = event.target.closest("[data-review]");
    if (review && !review.disabled) { openReview(review.dataset.review, review.dataset.id); return; }
    if (event.target.closest("[data-refresh]")) {
      refreshData().then(() => { updateNotificationBadge(); toast("数据已刷新"); });
      return;
    }
    if (event.target.closest("[data-app-create]")) { openAppModal(); return; }
    const appEdit = event.target.closest("[data-app-edit]");
    if (appEdit) { openAppModal(state.apps.find(item => item.id === appEdit.dataset.appEdit)); return; }
    if (event.target.closest("[data-close-app]")) { closeAppModal(); return; }
    if (event.target.closest("[data-invite-create]")) { openInviteModal(); return; }
    if (event.target.closest("[data-close-invite]")) {
      closeInviteModal();
      if (currentRoute()[0] === "enterprise") renderPage();
      return;
    }
    if (event.target.closest("[data-close-member]")) { closeMemberModal(); return; }
    const memberEdit = event.target.closest("[data-member-edit]");
    if (memberEdit) { openMemberModal(state.users.find(item => item.id === memberEdit.dataset.memberEdit)); return; }
    const inviteRevoke = event.target.closest("[data-invite-revoke]");
    if (inviteRevoke) { revokeEnterpriseInvite(inviteRevoke.dataset.inviteRevoke); return; }
    const authTab = event.target.closest("[data-auth-tab]");
    if (authTab) {
      document.querySelector("#authGate .auth-message.error")?.remove();
      switchAuthTab(authTab.dataset.authTab);
      return;
    }
    if (event.target.closest("[data-sso-login]")) {
      location.assign("/auth/login?next=" + encodeURIComponent("/#/dashboard"));
      return;
    }
    if (event.target.closest("[data-auth-home]")) { history.replaceState({}, "", "/"); renderAuthGate(); return; }
    if (event.target.closest("[data-copy-invite]")) {
      const input = document.querySelector("#inviteURL");
      navigator.clipboard?.writeText(input.value).then(() => toast("邀请链接已复制")).catch(() => {
        input.select();
        document.execCommand("copy");
        toast("邀请链接已复制");
      });
      return;
    }
    if (event.target.closest("#logoutButton")) {
      const form = document.createElement("form");
      form.method = "POST";
      form.action = "/auth/logout";
      const token = document.createElement("input");
      token.type = "hidden";
      token.name = "csrf_token";
      token.value = state.session?.csrf_token || "";
      form.appendChild(token);
      document.body.appendChild(form);
      form.submit();
      return;
    }
    if (event.target.closest("[data-reconnect]")) location.reload();

    const copyPP = event.target.closest("[data-copy-passport]");
    if (copyPP) {
      event.preventDefault();
      const compact = state.currentChange?.passport?.compact || document.querySelector("#passportCompactText")?.textContent || "";
      if (!compact) { toast("无通行证内容", "error"); return; }
      navigator.clipboard?.writeText(compact).then(() => toast("CP Compact 已复制")).catch(() => {
        const ta = document.createElement("textarea");
        ta.value = compact; document.body.appendChild(ta); ta.select(); document.execCommand("copy"); ta.remove();
        toast("CP Compact 已复制");
      });
      return;
    }
    const dlPP = event.target.closest("[data-download-passport]");
    if (dlPP) {
      event.preventDefault();
      const pp = state.currentChange?.passport;
      if (!pp) { toast("无通行证", "error"); return; }
      const blob = new Blob([JSON.stringify(pp, null, 2)], { type: "application/json" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = (pp.passport_id || "change-passport") + ".json";
      a.click();
      URL.revokeObjectURL(url);
      toast("通行证 JSON 已下载");
      return;
    }
    const verifyPP = event.target.closest("[data-verify-passport]");
    if (verifyPP) {
      event.preventDefault();
      const id = verifyPP.getAttribute("data-verify-passport") || state.currentChange?.id;
      (async () => {
        try {
          const body = {
            change_id: id,
            compact: state.currentChange?.passport?.compact || "",
            expected_commit: state.currentChange?.commit_sha || "",
            check_revocation: true,
          };
          const result = await api("/api/passport/verify", { method: "POST", body: JSON.stringify(body) });
          toast(result.ok ? "CP 验签通过" : "CP 验签失败", result.ok ? "success" : "error", result.message || "");
        } catch (error) {
          toast("验签请求失败", "error", error.message);
        }
      })();
      return;
    }
  });
  document.addEventListener("submit", async event => {
    if (["authLoginForm","enterpriseRegisterForm","acceptInviteForm"].includes(event.target.id)) { event.preventDefault(); await handleIdentityForm(event.target); return; }
    if (event.target.id === "appForm") { event.preventDefault(); await saveManagedApplication(event.target); return; }
    if (event.target.id === "organizationForm") { event.preventDefault(); await saveOrganization(event.target); return; }
    if (event.target.id === "inviteForm") { event.preventDefault(); await createEnterpriseInvite(event.target); return; }
    if (event.target.id === "memberForm") { event.preventDefault(); await saveEnterpriseMember(event.target); return; }
    if (event.target.id === "agentQAForm") {
      event.preventDefault();
      const form = event.target;
      const question = new FormData(form).get("question")?.trim();
      if (!question) { toast("请输入要追问的问题", "error"); return; }
      const button = form.querySelector("#agentQASubmit") || form.querySelector("button[type=submit]");
      const area = form.querySelector("textarea[name=question]");
      const changeId = form.getAttribute("data-change-id") || state.currentChange?.id;
      if (button) { button.disabled = true; button.textContent = "Agent 分析中…"; }
      if (area) area.disabled = true;
      showAgentQAPending(question);
      try {
        const result = await api("/api/changes/" + changeId + "/agent-ask", {method:"POST", body:JSON.stringify({question})});
        // 就地补丁 DOM，不再 renderPage 整页刷新
        const ok = patchAgentQAUI(result?.entry, result?.change);
        if (!ok && result?.change) {
          // 极端情况：面板已离开详情页时才退回全量渲染
          state.currentChange = result.change;
          await renderPage();
        }
        if (area) { area.value = ""; area.disabled = false; area.focus(); }
        toast("预审回答已写入", "success", result?.entry?.tool_calls ? `调用 ${result.entry.tool_calls} 次工具` : "已就地更新");
      } catch (error) {
        document.querySelectorAll(".agent-qa-item.is-pending").forEach(node => node.remove());
        const list = document.querySelector("#agentQAList");
        const empty = document.querySelector("#agentQAEmpty");
        if (list && !list.children.length && empty) {
          empty.hidden = false;
          list.hidden = true;
        }
        if (area) area.disabled = false;
        toast("预审问答失败", "error", error.message);
      } finally {
        if (button) { button.disabled = false; button.textContent = "提问"; }
      }
      return;
    }
    if (event.target.id !== "commentForm") return;
    event.preventDefault();
    const content = new FormData(event.target).get("content")?.trim();
    if (!content) {
      toast("请输入评论内容", "error");
      return;
    }
    const button = event.target.querySelector("button");
    button.disabled = true;
    try {
      await api("/api/changes/" + state.currentChange.id + "/comments", {method:"POST", body:JSON.stringify({content})});
      toast("评论已添加");
      await renderPage();
    } catch (error) {
      toast("评论发送失败", "error", error.message);
    } finally {
      button.disabled = false;
    }
  });
  document.addEventListener("click", event => {
    const chip = event.target.closest("[data-qa-suggest]");
    if (chip) {
      const form = document.querySelector("#agentQAForm");
      const area = form?.querySelector("textarea[name=question]");
      if (!area) return;
      area.value = chip.getAttribute("data-qa-suggest") || "";
      area.focus();
      return;
    }
    const blastBtn = event.target.closest("[data-load-blast]");
    if (blastBtn) {
      event.preventDefault();
      const id = document.querySelector("#blastRadiusPanel")?.getAttribute("data-change-id") || state.currentChange?.id;
      if (id) loadBlastRadius(id);
      return;
    }
    const effBtn = event.target.closest("[data-load-efficacy]");
    if (effBtn) {
      event.preventDefault();
      const id = document.querySelector("#efficacyPanel")?.getAttribute("data-change-id") || state.currentChange?.id;
      if (id) loadEfficacy(id);
      return;
    }
  });
  document.querySelector("#actorSelect")?.addEventListener("change", event => {
    if (event.target.disabled) return;
    state.actorId = event.target.value;
    localStorage.setItem("dbguard_actor", state.actorId);
    renderActor();
    renderPage();
    toast("已切换当前成员", "success", actor().name + " · " + roleLabel(actor().role));
  });
  document.querySelector("#applicationSelect")?.addEventListener("change", event => {
    const selected = state.apps.find(app => app.id === event.target.value);
    const repository = document.querySelector('#createForm [name="repository_url"]');
    if (repository && selected?.repository_url && !repository.value.trim()) repository.value = selected.repository_url;
  });
  document.querySelector("#menuButton")?.addEventListener("click", () => document.body.classList.toggle("sidebar-open"));
  document.querySelector("#mobileBackdrop")?.addEventListener("click", () => document.body.classList.remove("sidebar-open"));
  document.querySelector("#createForm").addEventListener("submit", async event => {
    event.preventDefault();
    const button = document.querySelector("#saveChangeButton");
    button.disabled = true; button.textContent = "正在保存…";
    const form = new FormData(event.currentTarget);
    const payload = Object.fromEntries(form.entries());
    const artifactDefinitions = [
      ["CODE", "代码 Diff", "code_diff", "Git Diff", "Go"],
      ["CONFIG", "配置 Diff", "config_diff", "配置文件", "YAML"],
      ["KUBERNETES", "Kubernetes 清单", "kubernetes_manifest", "部署清单", "YAML"],
      ["API", "API / OpenAPI Diff", "api_diff", "接口契约", "OpenAPI"]
    ];
    payload.artifacts = artifactDefinitions.map(([kind,name,field,source,language]) => ({
      kind, name, source, language, content: String(form.get(field) || "").trim()
    })).filter(item => item.content);
    let strategy = String(form.get("deployment_strategy") || "金丝雀发布");
    const environment = String(form.get("environment") || "");
    if (environment.includes("生产") && strategy === "全量发布") {
      strategy = "金丝雀发布";
      toast("生产环境禁止全量发布", "error", "已改为金丝雀，请确认后再次保存");
      const strategyEl = event.currentTarget.elements.namedItem("deployment_strategy");
      if (strategyEl) strategyEl.value = "金丝雀发布";
      button.disabled = false;
      button.textContent = state.editingId ? "保存修改" : "保存变更单";
      return;
    }
    payload.release_plan = {
      strategy,
      canary_percent: Number(form.get("canary_percent") || 10),
      observation_minutes: Number(form.get("observation_minutes") || 30),
      auto_rollback: form.get("auto_rollback") === "on",
      success_metrics: String(form.get("success_metrics") || "").split(/[，,、;；]/).map(item => item.trim()).filter(Boolean)
    };
    if (!String(payload.title || "").trim()) {
      toast("请填写变更标题", "error");
      button.disabled = false;
      button.textContent = state.editingId ? "保存修改" : "保存变更单";
      return;
    }
    if (!String(payload.rollback_plan || "").trim()) {
      toast("请填写回滚方案", "error");
      button.disabled = false;
      button.textContent = state.editingId ? "保存修改" : "保存变更单";
      return;
    }
    if (!payload.release_plan.success_metrics.length) {
      toast("请填写成功判定指标", "error", "至少一项，用逗号分隔");
      button.disabled = false;
      button.textContent = state.editingId ? "保存修改" : "保存变更单";
      return;
    }
    if (payload.release_plan.observation_minutes < 1) {
      toast("观察窗口至少 1 分钟", "error");
      button.disabled = false;
      button.textContent = state.editingId ? "保存修改" : "保存变更单";
      return;
    }
    const hasArtifact = payload.artifacts?.length || String(form.get("sql") || "").trim();
    if (!hasArtifact) {
      toast("请至少填写一类变更证据", "error", "配置 Diff / 代码 Diff / SQL 等");
      button.disabled = false;
      button.textContent = state.editingId ? "保存修改" : "保存变更单";
      return;
    }
    ["code_diff","config_diff","kubernetes_manifest","api_diff","deployment_strategy","canary_percent","observation_minutes","auto_rollback","success_metrics"].forEach(key => delete payload[key]);
    if (payload.planned_at) {
      const planned = new Date(payload.planned_at);
      if (Number.isNaN(planned.getTime())) {
        toast("计划执行时间格式不正确", "error");
        button.disabled = false;
        button.textContent = state.editingId ? "保存修改" : "保存变更单";
        return;
      }
      payload.planned_at = planned.toISOString();
    } else delete payload.planned_at;
    const formEl = event.currentTarget;
    try {
      const editingId = state.editingId;
      const change = await api(editingId ? "/api/changes/" + editingId : "/api/changes",{method:editingId ? "PUT" : "POST",body:JSON.stringify(payload)});
      formEl?.reset();
      closeCreate();
      await refreshData(false);
      toast(editingId ? "变更方案已更新" : "变更单已创建","success",change.id);
      location.hash = "#/changes/" + change.id;
    } catch (error) {
      toast(state.editingId ? "保存失败" : "创建失败","error",error.message);
    } finally {
      if (button) { button.disabled = false; button.textContent = state.editingId ? "保存修改" : "保存变更单"; }
    }
  });
  document.querySelector("#policyForm").addEventListener("submit", async event => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const splitList = value => String(value || "").split(/[，,]/).map(item => item.trim()).filter(Boolean);
    const editing = state.editingPolicy;
    const payload = {
      code: editing?.code || String(form.get("code") || "").trim().toUpperCase(),
      name: String(form.get("name") || "").trim(),
      description: String(form.get("description") || "").trim(),
      pattern: editing?.builtin ? editing.pattern || "" : String(form.get("pattern") || "").trim(),
      suggestion: String(form.get("suggestion") || "").trim(),
      severity: form.get("severity"),
      blocking: form.get("blocking") === "on",
      enabled: form.get("enabled") === "on",
      environments: splitList(form.get("environments")),
      change_types: splitList(form.get("change_types")),
      artifact_kinds: splitList(form.get("artifact_kinds"))
    };
    const button = document.querySelector("#savePolicyButton");
    button.disabled = true; button.textContent = "正在保存…";
    try {
      const saved = await api(editing ? "/api/policies/" + editing.id : "/api/policies", {method: editing ? "PUT" : "POST", body:JSON.stringify(payload)});
      closePolicy();
      await refreshData(false);
      if (currentRoute()[0] === "policies") renderPolicies(document.querySelector("#mainContent"));
      toast(editing ? "规则新版本已发布" : "自定义规则已创建", "success", saved.code + " · v" + saved.version);
    } catch (error) {
      toast("规则保存失败", "error", error.message);
    } finally {
      button.disabled = false; button.textContent = editing ? "保存并生成新版本" : "创建并启用规则";
    }
  });
  document.querySelector("#reviewForm").addEventListener("submit", async event => {
    event.preventDefault();
    if (!state.review) return;
    const formEl = event.currentTarget;
    const form = new FormData(formEl);
    const comment = form.get("comment") || "";
    if (state.review.action === "reject" && !comment.trim()) { toast("请填写驳回原因","error"); return; }
    const {action,id} = state.review;
    const submitBtn = document.querySelector("#reviewSubmitButton");
    if (submitBtn) submitBtn.disabled = true;
    try {
      const result = await api(`/api/changes/${id}/${action}`,{method:"POST",body:JSON.stringify({comment})});
      // await 后 event.currentTarget 可能为 null；关闭时会 reset 表单
      closeReview();
      await refreshData(false);
      toast(action === "approve" ? "审批已通过" : "变更已驳回","success",result.id);
      await renderPage();
    } catch (error) {
      toast("审批失败","error",error.message);
    } finally {
      if (submitBtn) submitBtn.disabled = false;
    }
  });
  document.querySelector("#findingForm").addEventListener("submit", async event => {
    event.preventDefault();
    if (!state.findingAction) return;
    const current = state.findingAction;
    const form = new FormData(event.currentTarget);
    let payload = {};
    if (current.action === "assign") {
      const dueAt = form.get("due_at");
      if (!form.get("owner_id") || !dueAt) {
        toast("请选择负责人和整改期限", "error");
        return;
      }
      payload = {owner_id: form.get("owner_id"), due_at: new Date(dueAt).toISOString()};
    } else if (current.action === "resolve") {
      const resolution = String(form.get("content") || "").trim();
      if (resolution.length < 5) {
        toast("整改说明至少填写 5 个字符", "error");
        return;
      }
      payload = {resolution};
    } else {
      const comment = String(form.get("content") || "").trim();
      if (!current.approved && !comment) {
        toast("退回整改时必须填写原因", "error");
        return;
      }
      payload = {approved: current.approved, comment};
    }
    const button = document.querySelector("#findingSubmitButton");
    button.disabled = true;
    try {
      await api("/api/changes/" + current.changeId + "/findings/" + current.findingId + "/" + current.action, {method:"POST", body:JSON.stringify(payload)});
      const message = current.action === "assign" ? "风险项已派单" : current.action === "resolve" ? "整改结果已提交" : current.approved ? "风险项已复核闭环" : "整改已退回";
      closeFinding();
      await refreshData(false);
      await renderPage();
      toast(message);
    } catch (error) {
      toast("风险处理失败", "error", error.message);
    } finally {
      button.disabled = false;
    }
  });
  document.querySelector("#globalSearch")?.addEventListener("keydown", event => {
    if (event.key !== "Enter") return;
    const keyword = event.target.value.trim();
    state.pendingChangeFilter = keyword;
    state.changeListPage = 1;
    closeAllOverlays();
    if (currentRoute()[0] === "changes" && !currentRoute()[1]) {
      renderPage();
    } else {
      location.hash = "#/changes";
    }
  });
  window.addEventListener("hashchange", () => {
    closeAllOverlays();
    renderPage();
  });
  document.addEventListener("keydown", event => {
    if (event.key === "Escape") closeAllOverlays();
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
      event.preventDefault();
      document.querySelector("#globalSearch")?.focus();
    }
  });
}

async function start() {
  bindEvents();
  try {
    if (!await bootstrapAuthentication()) return;
    await loadBase();
    await renderPage();
    connectEvents();
  } catch (error) {
    document.querySelector("#mainContent").innerHTML = `<div class="empty-state"><div class="empty-state-icon">${svg("alert")}</div><h3>无法连接治理服务</h3><p>${escapeHTML(error.message)}</p><button class="button button-primary" data-reconnect>重新连接</button></div>`;
  }
}
start();
