const base = "/api/joint-releases";
const route = "joint-releases";
const terminalFailure = status => status === "FAILED" || status === "CANCELED";
const canWrite = user => user?.enterprise_admin || ["技术负责人", "数据库审核人"].includes(user?.role);
const releaseLabel = {
  IN_PROGRESS: ["进行中", "waiting"], COMPLETED: ["部署全部成功", "approved"], SUPERSEDED: ["已被替代", "draft"],
  NOT_CONSUMED: ["未消费", "draft"], CONSUMED: ["已消费", "completed"], NOT_RUN: ["未收到证据", "waiting"],
  SUCCESS: ["已核实成功", "approved"], FAILED: ["部署失败", "failed"], CANCELED: ["部署取消", "failed"],
};

export async function renderJointReleases(main, id, mode, ui) {
  const {api, actor, escapeHTML: e, setHeader, pageHeading, formatDate} = ui;
  const request = location.hash;
  const canManage = canWrite(actor);
  const button = (path, label, primary = false) => `<a class="button button-${primary ? 'primary' : 'secondary'}" href="#/${e(path)}">${e(label)}</a>`;
  const badge = value => {
    const [label, tone] = releaseLabel[value] || ["未知", "draft"];
    return `<span class="status status-${tone}"><i></i>${label}</span>`;
  };
  setHeader("联合发布");
  const refresh = '<button class="button button-secondary" type="button" data-joint-refresh>刷新证据</button>';
  const back = button(route, "返回发布列表");
  try {
    if (!id) {
      const result = await api(base);
      if (location.hash !== request) return;
      main.innerHTML = pageHeading("联合发布", "按计划顺序发布独立变更。只有前一步的 Gate 已消费且 GitLab 部署成功，才可继续。",
        refresh + (canManage ? button(route + "/new", "新建联合发布", true) : "")) +
        `<section class="panel joint-panel" aria-label="发布计划">${result.items?.length ? `<ul class="joint-list">${result.items.map(plan =>
          `<li><a href="#/${route}/${encodeURIComponent(plan.id)}"><strong>${e(plan.name)}</strong></a>${badge(plan.status)}<span>${e(plan.environment)} · ${plan.steps.length} 个步骤 · ${formatDate(plan.created_at)}</span></li>`).join("")}</ul>` : '<p class="muted">暂无当前账号可查看的联合发布。</p>'}</section>`;
    } else if (id === "new" && !mode) {
      if (!canManage) throw new Error("当前角色没有创建联合发布的权限。");
      renderEditor(main, null, ui);
    } else {
      const plan = await api(`${base}/${encodeURIComponent(id)}`);
      if (location.hash !== request) return;
      const invalid = step => {
        const change = ui.changes.find(item => item.id === step.change_id);
        return change && (change.status === "REJECTED" || change.environment !== plan.environment || change.application_id !== step.application_id ||
          (change.artifact_version || 1) !== step.artifact_version || change.artifact_sha256 !== step.artifact_sha256 ||
          (change.commit_sha || "").toLowerCase() !== step.commit_sha);
      };
      const canReplace = canManage && !plan.superseded_by &&
        !plan.steps.some(step => step.consumption && !step.deployment) &&
        plan.steps.some(step => terminalFailure(step.deployment?.status) || invalid(step));
      if (mode === "supersede" && canReplace) {
        renderEditor(main, plan, ui);
        return;
      }
      setHeader("联合发布", plan.name);
      const actions = back + refresh + (canReplace ? button(`${route}/${encodeURIComponent(plan.id)}/supersede`, "创建替代计划", true) : "");
      const related = [plan.supersedes && button(`${route}/${encodeURIComponent(plan.supersedes)}`, "查看被替代的计划"),
        plan.superseded_by && button(`${route}/${encodeURIComponent(plan.superseded_by)}`, "查看新计划")].filter(Boolean).join("");
      main.innerHTML = pageHeading(e(plan.name), `${e(plan.environment)} · ${formatDate(plan.created_at)} · ${e(plan.id)}`, actions) +
        `<section class="panel joint-panel"><div class="joint-summary">${badge(plan.status)}${related}</div><h3>执行时间线</h3>
        <ol class="joint-timeline">${plan.steps.map((step, index) => {
          const event = plan.timeline?.[index] || {};
          const [approval, tone] = ui.statusInfo(event.approval_status === "COMPLETED" && step.consumption ? "APPROVED" : event.approval_status);
          const changeRef = ui.routeAllowed ? `<a href="#/changes/${encodeURIComponent(step.change_id)}">${e(step.change_id)}</a>` : e(step.change_id);
          return `<li><article><h4>${e(step.title || step.change_id)}</h4><p>${changeRef} · 版本 ${e(step.artifact_version)} · ${e(step.gitlab_job)}（项目 ${e(step.gitlab_project_id)}）</p>
          ${step.replaces_change_id ? `<p>替代变更 ${e(step.replaces_change_id)}</p>` : ""}
          ${step.inherited_from ? `<p>已沿用 <a href="#/${route}/${encodeURIComponent(step.inherited_from)}">${e(step.inherited_from)}</a> 的成功部署证据</p>` : ""}
          <dl class="joint-evidence">
            <div><dt>独立审批</dt><dd><span class="status status-${e(tone)}"><i></i>${e(approval)}</span>${event.reviewer_name ? `<small>${e(event.reviewer_name)}</small>` : ""}</dd></div>
            <div><dt>Gate</dt><dd>${badge(event.gate_status)}${step.consumption ? `<small>作业 #${e(step.consumption.job_id)} · 流水线 #${e(step.consumption.pipeline_id)} · ${formatDate(step.consumption.at)}</small>` : ""}</dd></div>
            <div><dt>GitLab 部署</dt><dd>${badge(event.deployment_status)}${step.deployment ? `<small>部署 #${e(step.deployment.deployment_id)} · ${formatDate(step.deployment.occurred_at)}</small>` : ""}</dd></div>
          </dl>${event.blocking_cause ? `<p class="joint-blocking"><strong>当前原因：</strong>${e(event.blocking_cause)}</p>` : ""}
          <details><summary>固定制品与提交</summary><p>SHA-256 <code>${e(step.artifact_sha256)}</code></p><p>Commit <code>${e(step.commit_sha)}</code></p></details></article></li>`;
        }).join("")}</ol></section>`;
    }
    main.querySelector("[data-joint-refresh]")?.addEventListener("click", () => void renderJointReleases(main, id, mode, ui));
  } catch (error) {
    if (location.hash !== request) return;
    main.innerHTML = pageHeading("联合发布", "无法读取发布数据。", back) + `<p class="panel joint-panel" role="alert">${e(error.message)}</p>`;
  }
}

function renderEditor(main, old, ui) {
  const {api, escapeHTML: e, pageHeading, setHeader, changes, toast} = ui;
  const title = old ? "创建替代计划" : "新建联合发布";
  const back = old ? `${route}/${encodeURIComponent(old.id)}` : route;
  const minimum = old ? 1 : 2;
  const inherited = old?.steps.filter(step => step.deployment?.status === "SUCCESS").length || 0;
  const maximum = 32 - inherited;
  const rowData = new WeakMap();
  setHeader("联合发布", title);
  main.innerHTML = pageHeading(title, old
    ? "新计划沿用已成功的步骤和证据。失败步骤必须使用同一应用的新变更；后续未完成步骤保持顺序。"
    : "选择至少两条独立变更，核对当前制品版本，按执行顺序填写 GitLab 作业。") + `
    <form class="panel joint-panel joint-editor">
      ${old ? `<p>原计划：<a href="#/${e(back)}">${e(old.name)}</a>。提交后原计划不能继续消费 Gate。</p>` : ""}
      <div class="joint-fields"><label class="field"><span>计划名称</span><input name="releaseName" required maxlength="120" value="${e(old ? [...old.name].slice(0, 115).join("") + " · 替代" : "")}"></label>
        <label class="field"><span>发布环境</span><input name="environment" required maxlength="64" value="${e(old?.environment || "")}" ${old ? "readonly" : ""}></label></div>
      <datalist id="joint-change-options">${changes.filter(change => change.status !== "COMPLETED").map(change => `<option value="${e(change.id)}">${e(change.title)} · ${e(change.environment)}</option>`).join("")}</datalist>
      <ol class="joint-edit-steps" aria-label="执行顺序"></ol>
      <p role="alert" class="joint-error" hidden></p>
      <div class="joint-form-actions"><button type="button" class="button button-secondary" data-joint-add>添加步骤</button>
        <a class="button button-secondary" href="#/${e(back)}">取消</a><button type="submit" class="button button-primary">${old ? "确认替代并创建" : "创建计划"}</button></div>
    </form>`;
  const form = main.querySelector("form");
  const list = form.querySelector("ol");
  const formError = form.querySelector('[role="alert"]');
  const fail = message => { formError.hidden = !message; formError.textContent = message; };
  const add = form.querySelector("[data-joint-add]");
  function updateRows() {
    const rows = [...list.children];
    for (const [index, row] of rows.entries()) {
      const number = index + 1;
      row.querySelector("h4").textContent = `步骤 ${number}`;
      for (const control of row.querySelectorAll("input")) {
        control.setAttribute("aria-label", `步骤 ${number} ${control.previousElementSibling.textContent}`);
      }
      for (const action of ["up", "down", "remove"]) {
        const control = row.querySelector(`[data-joint-row="${action}"]`);
        if (!control) continue;
        control.setAttribute("aria-label", `${control.textContent}步骤 ${number}`);
        control.disabled = action === "up" ? !row.previousElementSibling || !!rowData.get(row.previousElementSibling).original
          : action === "down" ? index === rows.length - 1 : rows.length <= minimum;
      }
    }
    add.disabled = rows.length >= maximum;
  }
  function addRow(original = null) {
    const failed = terminalFailure(original?.deployment?.status);
    const row = document.createElement("li");
    rowData.set(row, {original, change: null});
    row.innerHTML = `<h4></h4>${original ? `<p>${failed ? '失败步骤，须选新变更' : '保留原顺序；制品失效时可重新固定或更换变更'} · ${e(original.title || original.change_id)}（${e(original.change_id)}）</p>` : ""}
      <div class="joint-fields joint-step-fields">
        <label class="field"><span>变更 ID</span><input data-pin="change" list="joint-change-options" required maxlength="128" value="${e(failed ? "" : original?.change_id || "")}" autocomplete="off"></label>
        <label class="field"><span>GitLab 项目 ID</span><input data-pin="project" type="number" min="1" required value="${e(original?.gitlab_project_id || "")}"></label>
        <label class="field"><span>GitLab 作业名</span><input data-pin="job" required maxlength="255" value="${e(original?.gitlab_job || "")}"></label>
      </div><div class="joint-row-actions"><button type="button" class="button button-secondary button-small" data-joint-row="read">核对当前制品</button>
        ${original ? "" : [["up", "上移"], ["down", "下移"], ["remove", "移除"]].map(([action, label]) => `<button type="button" class="button button-secondary button-small" data-joint-row="${action}">${label}</button>`).join("")}</div>
      <p class="muted joint-pin" role="status">核对后会显示实际版本、制品摘要与提交。</p>`;
    list.append(row);
    updateRows();
    return row;
  }
  if (old) for (const step of old.steps) if (step.deployment?.status !== "SUCCESS") addRow(step);
  while (list.children.length < minimum) addRow();
  add.addEventListener("click", () => addRow().querySelector("input").focus());
  function invalidate(row) {
    const entry = rowData.get(row);
    entry.change = null;
    entry.read = null;
    row.querySelector(".joint-pin").textContent = "需要重新核对当前制品。";
    row.querySelector('[data-joint-row="read"]').disabled = false;
  }
  form.elements.environment.addEventListener("input", () => [...list.children].forEach(invalidate));
  list.addEventListener("input", event => {
    if (event.target.dataset.pin === "change") invalidate(event.target.closest("li"));
  });
  list.addEventListener("click", async event => {
    const control = event.target.closest("[data-joint-row]");
    if (!control) return;
    const row = control.closest("li");
    fail("");
    switch (control.dataset.jointRow) {
      case "up": list.insertBefore(row, row.previousElementSibling); break;
      case "down": row.nextElementSibling.after(row); break;
      case "remove": row.remove(); add.focus(); break;
      case "read": {
        const entry = rowData.get(row);
        const input = row.querySelector('[data-pin="change"]');
        const output = row.querySelector(".joint-pin");
        const id = input.value.trim();
        const environment = form.elements.environment.value.trim();
        invalidate(row);
        if (!id || !environment) { output.textContent = "先填写变更 ID 和发布环境。"; return; }
        const read = Symbol();
        entry.read = read;
        control.disabled = true;
        output.textContent = "正在核对…";
        try {
          const change = await api(`/api/changes/${encodeURIComponent(id)}`);
          if (entry.read !== read) return;
          if (change.id !== id || change.environment !== environment || change.status === "COMPLETED" ||
              !/^[a-f0-9]{64}$/i.test(change.artifact_sha256 || "") || !/^[a-f0-9]{40}$/i.test(change.commit_sha || "")) {
            throw new Error("变更必须属于该环境、尚未消费，且具有制品摘要和完整提交 SHA。");
          }
          if (entry.original && (change.application_id !== entry.original.application_id ||
              terminalFailure(entry.original.deployment?.status) && id === entry.original.change_id)) {
            throw new Error("请选择同一应用的新变更来替代失败步骤。");
          }
          entry.change = change;
          output.textContent = `${change.title} · 版本 ${change.artifact_version || 1} · SHA-256 ${change.artifact_sha256} · 提交 ${change.commit_sha}`;
        } catch (error) {
          if (entry.read === read) output.textContent = error.message;
        } finally {
          if (entry.read === read) control.disabled = false;
        }
        return;
      }
    }
    updateRows();
    if (control.isConnected) control.focus();
  });
  form.addEventListener("submit", async event => {
    event.preventDefault();
    fail("");
    const steps = [];
    for (const [index, row] of [...list.children].entries()) {
      const {original, change} = rowData.get(row);
      const project = Number(row.querySelector('[data-pin="project"]').value);
      const job = row.querySelector('[data-pin="job"]').value.trim();
      if (!change || !Number.isSafeInteger(project) || project < 1 || !job) {
        fail(`请核对步骤 ${index + 1} 的当前制品，并填写项目 ID 与作业名。`);
        row.querySelector('[data-joint-row="read"]').focus();
        return;
      }
      if (steps.some(step => step.change_id === change.id)) { fail(`步骤 ${index + 1} 重复选择了同一变更。`); return; }
      steps.push({change_id: change.id, artifact_version: change.artifact_version || 1, artifact_sha256: change.artifact_sha256,
        gitlab_project_id: project, gitlab_job: job, ...(original && change.id !== original.change_id ? {replaces_change_id: original.change_id} : {})});
    }
    const payload = {name: form.elements.releaseName.value.trim(), environment: form.elements.environment.value.trim(), steps,
      ...(old ? {supersedes: old.id} : {})};
    const submit = form.querySelector('[type="submit"]');
    submit.disabled = true;
    try {
      const plan = await api(base, {method: "POST", body: JSON.stringify(payload)});
      if (!form.isConnected) return;
      toast(old ? "替代计划已创建" : "联合发布已创建");
      location.hash = `#/${route}/${encodeURIComponent(plan.id)}`;
    } catch (error) {
      fail(error.message);
    } finally {
      submit.disabled = false;
    }
  });
}
