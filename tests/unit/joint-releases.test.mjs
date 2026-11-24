import assert from "node:assert/strict";
import test from "node:test";
import { renderJointReleases } from "../../internal/httpapi/web/joint-releases.mjs";

const escapeHTML = value => String(value).replace(/[&<>"']/g, char => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"}[char]));
const ui = {
  changes: [], escapeHTML, setHeader() {}, pageHeading: (title, text, actions) => `<h2>${title}</h2><p>${text}</p>${actions || ""}`,
  formatDate: () => "11-24 08:00", statusInfo: status => [status, "approved"], routeAllowed: true,
};

test("joint timeline separates approval, consumption, and matched deployment; replacement is role- and evidence-gated", async t => {
  const previous = Object.getOwnPropertyDescriptor(globalThis, "location");
  Object.defineProperty(globalThis, "location", {configurable: true, value: {hash: "#/joint-releases/release_test"}});
  t.after(() => previous ? Object.defineProperty(globalThis, "location", previous) : delete globalThis.location);
  const main = {innerHTML: "", querySelector: () => null};
  const step = {change_id: "chg_1", title: "<img src=x>", artifact_version: 1, gitlab_job: "migrate", gitlab_project_id: 42,
    consumption: {job_id: 123, pipeline_id: 456}, deployment: {status: "FAILED", deployment_id: 789}};
  const plan = {id: "release_test", name: "<release>", status: "IN_PROGRESS", steps: [step],
    timeline: [{approval_status: "COMPLETED", gate_status: "CONSUMED", deployment_status: "FAILED", blocking_cause: "<旧迁移失败>"}]};
  const render = async (role, state = plan) => {
    await renderJointReleases(main, state.id, "", {...ui, actor: {role}, api: async path => {
      assert.equal(path, "/api/joint-releases/release_test");
      return state;
    }});
    return main.innerHTML;
  };
  const result = await render("数据库审核人");
  for (const label of ["独立审批", "APPROVED", "已消费", "部署失败", "作业 #123", "部署 #789", "创建替代计划", "&lt;旧迁移失败&gt;"]) {
    assert(result.includes(label), label);
  }
  assert(!result.includes("<img src=x>"));
  assert(!(await render("后端开发")).includes("创建替代计划"));
  const awaiting = {...plan, steps: [{...step, deployment: null}], timeline: [{gate_status: "CONSUMED", deployment_status: "NOT_RUN"}]};
  const pending = await render("技术负责人", awaiting);
  assert(pending.includes("未收到证据"));
  assert(!pending.includes("已核实成功"));
  assert(!pending.includes("创建替代计划"));
});
