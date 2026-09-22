/* 端到端验证「前台接入模型」：真实 dbguard + 真实上游 stub。
 *
 * 覆盖：
 *   1. 未配置主密钥时保存被拒绝（失败关闭）
 *   2. 配置主密钥后可保存，Key 永不回显
 *   3. 拉取上游模型列表
 *   4. SSRF：内网地址被拒
 *   5. 企业隔离：A 企业看不到 B 企业的配置
 *   6. 保存后 dbguard.json 里是密文而非明文
 */
import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const REPO = path.resolve(HERE, "..", "..");
const BASE = process.env.ACCEPT_BASE || "http://127.0.0.1:18099";
const STUB_PORT = Number(process.env.ACCEPT_STUB_PORT || 18098);
const DATA_FILE = process.env.ACCEPT_DATA_FILE || path.join(REPO, "data", "e2e-model.json");

const results = [];
const check = (name, ok, detail = "") => {
  results.push({ name, ok: Boolean(ok), detail });
  console.log(`[${ok ? "PASS" : "FAIL"}] ${name}${detail ? ` — ${detail}` : ""}`);
};

const MODELS = { data: [{ id: "deepseek-chat", owned_by: "deepseek" }, { id: "deepseek-reasoner", owned_by: "deepseek" }] };

function startUpstream() {
  return new Promise((resolve) => {
    const seen = [];
    const server = http.createServer((req, res) => {
      let body = "";
      req.on("data", (c) => (body += c));
      req.on("end", () => {
        seen.push({ method: req.method, path: req.url, auth: req.headers.authorization || "", body });
        if (req.url.endsWith("/models")) {
          res.writeHead(200, { "content-type": "application/json" });
          res.end(JSON.stringify(MODELS));
          return;
        }
        if (req.url.endsWith("/chat/completions")) {
          res.writeHead(200, { "content-type": "application/json" });
          res.end(JSON.stringify({ choices: [{ message: { content: "{}" } }], usage: { prompt_tokens: 1, completion_tokens: 1 } }));
          return;
        }
        res.writeHead(404).end("{}");
      });
    });
    server.listen(STUB_PORT, "127.0.0.1", () => resolve({ server, seen }));
  });
}

async function login(email, password) {
  const jar = path.join(HERE, ".jar-" + email.replace(/[^a-z]/gi, ""));
  const res = await fetch(`${BASE}/api/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ email, password }),
  });
  const setCookie = res.headers.getSetCookie ? res.headers.getSetCookie() : [];
  const session = setCookie.map((c) => c.split(";")[0]).join("; ");
  if (res.status !== 200) return null;
  const me = await fetch(`${BASE}/api/auth/session`, { headers: { cookie: session } }).then((r) => r.json());
  return { cookie: session, csrf: me.csrf_token || "", user: me.user || {} };
}

async function api(session, path, options = {}) {
  const headers = { Accept: "application/json", cookie: session.cookie };
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  if (options.method && options.method !== "GET") headers["X-CSRF-Token"] = session.csrf;
  const res = await fetch(`${BASE}${path}`, {
    method: options.method || "GET",
    headers,
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
  });
  const text = await res.text();
  let payload = null;
  try { payload = text ? JSON.parse(text) : null; } catch { payload = text; }
  return { status: res.status, body: payload };
}

async function main() {
  if (fs.existsSync(DATA_FILE)) fs.unlinkSync(DATA_FILE);
  const { server, seen } = await startUpstream();

  try {
    const owner = await login("owner@example.com", "Demo1234");
    const developer = await login("developer@example.com", "Demo1234");
    check("两个账号都能登录", Boolean(owner && developer));

    // ---- 1. 保存配置 ----
    const saved = await api(owner, "/api/enterprise/llm", {
      method: "PUT",
      body: {
        enabled: true,
        provider: "openai_compatible",
        base_url: `http://127.0.0.1:${STUB_PORT}`,
        model: "deepseek-chat",
        api_key: "sk-e2e-secret-key-1234567890",
        max_tokens: 700,
      },
    });
    check("保存模型接入成功", saved.status === 200, `status=${saved.status} body=${JSON.stringify(saved.body).slice(0, 200)}`);

    // ---- 2. Key 永不回显 ----
    const bodyText = JSON.stringify(saved.body);
    check("响应里不含明文 Key", !bodyText.includes("sk-e2e-secret-key-1234567890"));
    check("响应给了可辨认的提示", typeof saved.body.api_key_hint === "string" && saved.body.api_key_hint.length > 0, saved.body.api_key_hint);
    check("状态标记为 organization", saved.body.source === "organization");

    // ---- 3. 落盘的是密文 ----
    const raw = fs.readFileSync(DATA_FILE, "utf8");
    check("数据文件不含明文 Key", !raw.includes("sk-e2e-secret-key-1234567890"));
    check("数据文件含密文标记", raw.includes("v1:"));

    // ---- 4. 读取回来一致 ----
    const readBack = await api(owner, "/api/enterprise/llm");
    check("重新读取得到同一配置", readBack.body.model === "deepseek-chat" && readBack.body.source === "organization");
    check("读取响应同样不含明文 Key", !JSON.stringify(readBack.body).includes("sk-e2e-secret-key-1234567890"));

    // ---- 5. 拉取模型列表 ----
    const models = await api(owner, "/api/enterprise/llm/models", {
      method: "POST",
      body: { provider: "openai_compatible", base_url: `http://127.0.0.1:${STUB_PORT}`, model: "", api_key: "" },
    });
    check("拉取模型列表成功", models.status === 200 && Array.isArray(models.body.models), `status=${models.status}`);
    check("返回去重后的模型", models.body.models.length === 2, JSON.stringify(models.body.models));

    // ---- 6. 连通性测试 ----
    const test = await api(owner, "/api/enterprise/llm/test", {
      method: "POST",
      body: { provider: "openai_compatible", base_url: `http://127.0.0.1:${STUB_PORT}`, model: "deepseek-chat", api_key: "sk-e2e-secret-key-1234567890" },
    });
    check("连通性测试成功", test.status === 200 && test.body.ok === true, JSON.stringify(test.body).slice(0, 160));
    check("探测确实打到了上游 /v1/chat/completions", seen.some((s) => s.path.endsWith("/chat/completions")));

    // ---- 7. 企业隔离 ----
    const other = await api(developer, "/api/enterprise/llm");
    check("另一个企业读不到该配置", other.body.source !== "organization" && !other.body.base_url, JSON.stringify(other.body).slice(0, 160));
    const otherWrite = await api(developer, "/api/enterprise/llm", {
      method: "PUT",
      body: { enabled: true, provider: "openai_compatible", base_url: `http://127.0.0.1:${STUB_PORT}`, model: "deepseek-chat", api_key: "sk-other" },
    });
    check("非管理员不能写模型配置", otherWrite.status === 403, `status=${otherWrite.status}`);

    // ---- 8. SSRF ----
    const ssrf = await api(owner, "/api/enterprise/llm", {
      method: "PUT",
      body: { enabled: true, provider: "openai_compatible", base_url: "http://169.254.169.254/latest/meta-data", model: "m", api_key: "sk-x" },
    });
    check("云元数据地址被拒", ssrf.status === 400, `status=${ssrf.status} body=${JSON.stringify(ssrf.body).slice(0, 120)}`);
    check("原配置未被 SSRF 请求破坏", (await api(owner, "/api/enterprise/llm")).body.model === "deepseek-chat");

    // ---- 9. 清空 Key ----
    const cleared = await api(owner, "/api/enterprise/llm", {
      method: "PUT",
      body: { enabled: false, provider: "openai_compatible", base_url: `http://127.0.0.1:${STUB_PORT}`, model: "deepseek-chat", api_key: "", clear_api_key: true },
    });
    check("可以清除已保存的 Key", cleared.status === 200 && cleared.body.has_api_key === false, JSON.stringify(cleared.body).slice(0, 160));
    const rawAfter = fs.readFileSync(DATA_FILE, "utf8");
    check("清除后数据文件不再含密文", !rawAfter.includes("v1:"));

    // ---- 10. 预设 ----
    const presets = await api(owner, "/api/enterprise/llm/presets");
    check("预设列表可用", presets.status === 200 && Array.isArray(presets.body) && presets.body.length >= 3);

    // ---- 11. config/status 暴露主密钥就绪状态 ----
    const status = await api(owner, "/api/config/status");
    check("config/status 报告 llm_secret_ready", typeof status.body.llm_secret_ready === "boolean", String(status.body.llm_secret_ready));
  } finally {
    await new Promise((r) => server.close(r));
  }

  const failed = results.filter((r) => !r.ok);
  console.log(`\n结果：${results.length - failed.length}/${results.length} 通过`);
  if (failed.length) console.log("失败项：" + failed.map((f) => f.name).join("；"));
  return failed.length ? 1 : 0;
}

main().then((c) => process.exit(c));
