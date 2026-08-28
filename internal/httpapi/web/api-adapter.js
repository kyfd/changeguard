(function (global) {
  "use strict";

  const jsonHeaders = { Accept: "application/json" };
  let csrfToken = "";
  let actorId = localStorage.getItem("changeguard_actor") || localStorage.getItem("dbguard_actor") || "";

  class APIError extends Error {
    constructor(message, status, payload) {
      super(message || "请求失败");
      this.name = "APIError";
      this.status = status;
      this.payload = payload;
    }
  }

  function setSession(session) {
    csrfToken = session?.csrf_token || "";
    if (session?.user?.id) {
      actorId = session.user.id;
      localStorage.setItem("changeguard_actor", actorId);
      localStorage.setItem("dbguard_actor", actorId);
    }
  }

  function setActor(id) {
    actorId = id || actorId;
    if (actorId) {
      localStorage.setItem("changeguard_actor", actorId);
      localStorage.setItem("dbguard_actor", actorId);
    }
  }

  async function request(path, options = {}) {
    const headers = { ...jsonHeaders, ...(options.headers || {}) };
    if (options.body != null && !(options.body instanceof FormData)) headers["Content-Type"] = "application/json";
    if (csrfToken) headers["X-CSRF-Token"] = csrfToken;
    if (actorId) headers["X-Actor-ID"] = actorId;
    const response = await fetch(path, { credentials: "same-origin", cache: "no-store", ...options, headers });
    const contentType = response.headers.get("content-type") || "";
    let payload = null;
    if (response.status !== 204) {
      if (contentType.includes("json")) payload = await response.json().catch(() => null);
      else payload = await response.text().catch(() => "");
    }
    if (!response.ok) {
      const message = payload?.error || payload?.message || (typeof payload === "string" && payload) || `请求失败（HTTP ${response.status}）`;
      throw new APIError(message, response.status, payload);
    }
    return payload;
  }

  async function optionalRequest(paths) {
    for (const path of paths) {
      try {
        return { supported: true, path, data: await request(path) };
      } catch (error) {
        if (error.status !== 404 && error.status !== 405) throw error;
      }
    }
    return { supported: false, path: "", data: null };
  }

  async function softRequest(path, fallback) {
    try {
      return await request(path);
    } catch (error) {
      if (error.status === 401) throw error;
      return fallback;
    }
  }

  function listFrom(value, keys = []) {
    if (Array.isArray(value)) return value;
    for (const key of keys) if (Array.isArray(value?.[key])) return value[key];
    return [];
  }

  function passportListFrom(value) {
    const items = listFrom(value, ["passports", "items", "data"]);
    if (items.length) return items;
    for (const key of ["passport", "gate_passport", "change_passport"]) {
      if (value?.[key] && typeof value[key] === "object") return [value[key]];
    }
    if (value?.id && (value?.change_id || value?.artifact_sha256) && value?.status) return [value];
    return [];
  }

  function evidenceState(change) {
    const explicit = change?.evidence_state || change?.validation_state || change?.experiment?.evidence_state;
    const explicitState = explicit ? String(explicit).toUpperCase() : "";
    const report = change?.experiment || change?.validation_report;
    const status = String(report?.status || "").toUpperCase();
    const mode = String(report?.mode || "").toUpperCase();
    if (explicitState === "FAILED" || status === "FAILED") return "FAILED";
    if (explicitState === "DEMO_ONLY" || mode.includes("SIMULATED") || mode.includes("DEMO")) return "DEMO_ONLY";

    const kinds = Array.isArray(change?.artifacts) ? change.artifacts.map(item => String(item?.kind || "").toUpperCase()) : [];
    const databaseChange = Boolean(String(change?.sql || "").trim()) || kinds.includes("DATABASE");
    if (databaseChange) {
      return status === "PASSED" && mode === "POSTGRES" && report?.rollback_verified === true ? "REAL" : "NOT_RUN";
    }
    const check = change?.check_run || change?.checkRun;
    const checkPassed = String(check?.status || "").toUpperCase() === "PASSED" && Number(check?.blocking || 0) === 0;
    if (checkPassed && check?.artifact_sha256 && check?.rule_set_version) return "REAL";
    return explicitState === "REAL" ? "REAL" : "NOT_RUN";
  }

  function normalizePassport(raw, change) {
    const passport = raw || change?.passport || change?.gate_passport || change?.change_passport || null;
    if (!passport) return { available: false, state: "NOT_RUN" };
    let status = String(passport.status || passport.state || "UNKNOWN").toUpperCase();
    const expiresAt = passport.expires_at || "";
    if (status === "ACTIVE" && expiresAt && new Date(expiresAt).getTime() <= Date.now()) status = "EXPIRED";
    const consumeState = String(passport.consume_state || passport.consumption_status || (passport.consumed_at ? "CONSUMED" : "UNUSED")).toUpperCase();
    return {
      exists: true,
      available: status === "ACTIVE",
      id: passport.id || passport.passport_id || passport.token_id || "",
      changeId: passport.change_id || passport.aggregate_id || change?.id || "",
      status,
      state: status,
      digest: passport.artifact_sha256 || passport.digest || passport.content_digest || passport.artifact_digest || passport.sha256 || "",
      environment: passport.environment || passport.target_environment || change?.environment || "",
      approver: passport.approver_name || passport.approver || change?.reviewer_name || "",
      issuedAt: passport.issued_at || passport.created_at || "",
      expiresAt,
      consumedAt: passport.consumed_at || "",
      consumeState,
      revokedAt: passport.revoked_at || "",
      evidenceState: String(passport.evidence_state || evidenceState(change)).toUpperCase(),
      verifyPath: passport.verify_path || passport.verify_endpoint || ""
    };
  }

  function normalizeChange(change) {
    const artifacts = Array.isArray(change?.artifacts) ? change.artifacts : [];
    const passport = normalizePassport(null, change);
    return {
      ...change,
      artifact_sha256: change?.artifact_sha256 || change?.artifact_digest || change?.content_digest || change?.sha256 || "",
      artifacts,
      findings: Array.isArray(change?.findings) ? change.findings : [],
      timeline: Array.isArray(change?.timeline) ? change.timeline : [],
      comments: Array.isArray(change?.comments) ? change.comments : [],
      evidence_state: evidenceState(change),
      passport,
      risk_score: Number.isFinite(Number(change?.risk_score)) ? Number(change.risk_score) : null
    };
  }

  function normalizePassportBundle(result, changes) {
    if (!result.supported) return { supported: false, items: [], verifyPath: "" };
    const raw = result.data || {};
    const items = passportListFrom(raw);
    const byChange = new Map(changes.map(item => [item.id, item]));
    return {
      supported: true,
      path: result.path,
      verifyPath: raw.verify_path || raw.verify_endpoint || raw.gate_verify_path || "",
      issuePath: raw.issue_path || raw.issue_endpoint || "",
      items: items.map(item => normalizePassport(item, byChange.get(item.change_id || item.aggregate_id)))
    };
  }

  const API = {
    APIError,
    request,
    setSession,
    setActor,
    evidenceState,
    normalizeChange,
    normalizePassport,
    authStatus: () => request("/api/auth/status"),
    session: () => request("/api/auth/session"),
    login: payload => request("/api/auth/login", { method: "POST", body: JSON.stringify(payload) }),
    register: payload => request("/api/auth/register", { method: "POST", body: JSON.stringify(payload) }),
    acceptInvite: payload => request("/api/auth/invitations/accept", { method: "POST", body: JSON.stringify(payload) }),
    dashboard: () => request("/api/dashboard"),
    apps: () => request("/api/apps"),
    users: () => request("/api/users"),
    changes: async () => listFrom(await request("/api/changes"), ["changes", "items"]).map(normalizeChange),
    change: async id => normalizeChange(await request(`/api/changes/${encodeURIComponent(id)}`)),
    createChange: payload => request("/api/changes", { method: "POST", body: JSON.stringify(payload) }),
    updateChange: (id, payload) => request(`/api/changes/${encodeURIComponent(id)}`, { method: "PUT", body: JSON.stringify(payload) }),
    changeAction: (id, action, payload = {}) => request(`/api/changes/${encodeURIComponent(id)}/${action}`, { method: "POST", body: JSON.stringify(payload) }),
    findingAction: (changeId, findingId, action, payload) => request(`/api/changes/${encodeURIComponent(changeId)}/findings/${encodeURIComponent(findingId)}/${action}`, { method: "POST", body: JSON.stringify(payload) }),
    policies: () => request("/api/policies"),
    createPolicy: payload => request("/api/policies", { method: "POST", body: JSON.stringify(payload) }),
    updatePolicy: (id, payload) => request(`/api/policies/${encodeURIComponent(id)}`, { method: "PUT", body: JSON.stringify(payload) }),
    togglePolicy: id => request(`/api/policies/${encodeURIComponent(id)}/toggle`, { method: "POST", body: "{}" }),
    testPolicies: payload => request("/api/policies/test", { method: "POST", body: JSON.stringify(payload) }),
    audits: limit => request(`/api/audits?limit=${encodeURIComponent(limit || 250)}`),
    config: () => request("/api/config/status"),
    operations: () => optionalRequest(["/api/operations/outbox"]),
    conflicts: () => softRequest("/api/conflicts", null),
    integrationStatus: () => softRequest("/api/integrations/status", {}),
    integrationEvents: limit => softRequest(`/api/integrations/events?limit=${encodeURIComponent(limit || 100)}`, []),
    async passports(changes) {
      const globalResult = await optionalRequest(["/api/passports", "/api/gate/passports", "/api/ci/passports"]);
      if (globalResult.supported) return normalizePassportBundle(globalResult, changes);
      const rows = await Promise.all(changes.map(async change => {
        const result = await optionalRequest([`/api/changes/${encodeURIComponent(change.id)}/passports`]);
        if (!result.supported) return { supported: false, items: [] };
        return { supported: true, items: passportListFrom(result.data).map(item => normalizePassport(item, change)) };
      }));
      const items = rows.flatMap(row => row.items);
      const supported = rows.some(row => row.supported);
      return { supported, path: supported ? "/api/changes/{id}/passports" : "", verifyPath: supported ? "/api/gate/verify" : "", issuePath: supported ? "/api/changes/{id}/passports" : "", items };
    },
    logout: () => request("/auth/logout", { method: "POST", body: "{}" }),
    enterprise: () => request("/api/enterprise"),
    updateEnterprise: payload => request("/api/enterprise", { method: "PUT", body: JSON.stringify(payload) }),
    enterpriseMembers: () => request("/api/enterprise/members"),
    enterpriseMember: id => request(`/api/enterprise/members/${encodeURIComponent(id)}`),
    updateMember: (id, payload) => request(`/api/enterprise/members/${encodeURIComponent(id)}`, { method: "PUT", body: JSON.stringify(payload) }),
    enterpriseInvites: () => request("/api/enterprise/invites"),
    createInvite: payload => request("/api/enterprise/invites", { method: "POST", body: JSON.stringify(payload) }),
    revokeInvite: id => request(`/api/enterprise/invites/${encodeURIComponent(id)}`, { method: "DELETE" }),
    issuePassport: id => request(`/api/changes/${encodeURIComponent(id)}/passports`, { method: "POST", body: "{}" }),
    revokePassport: (changeId, passportId) => request(`/api/changes/${encodeURIComponent(changeId)}/passports/${encodeURIComponent(passportId)}/revoke`, { method: "POST", body: "{}" }),
    gateMetadata: () => optionalRequest(["/api/gate/metadata"]),
    gateVerify: payload => request("/api/gate/verify", { method: "POST", body: JSON.stringify(payload) }),
    gateConsume: payload => request("/api/gate/consume", { method: "POST", body: JSON.stringify(payload) }),
    async loadWorkspace() {
      const [dashboard, apps, users, changes, policies, audits, config, conflicts, integrationStatus, integrationEvents] = await Promise.all([
        API.dashboard(), API.apps(), API.users(), API.changes(), API.policies(), API.audits(250), API.config(),
        API.conflicts(), API.integrationStatus(), API.integrationEvents(100)
      ]);
      const passportBundle = await API.passports(changes);
      const passportByChange = new Map();
      [...passportBundle.items].sort((a, b) => new Date(a.issuedAt || 0) - new Date(b.issuedAt || 0)).forEach(item => {
        const key = item.changeId || item.change_id;
        const current = passportByChange.get(key);
        if (!current || item.available || !current.available) passportByChange.set(key, item);
      });
      const normalizedChanges = changes.map(change => {
        const direct = passportByChange.get(change.id);
        return direct ? { ...change, passport: direct } : change;
      });
      const eventItems = listFrom(integrationEvents, ["events", "items", "data"]);
      return {
        dashboard, apps, users, changes: normalizedChanges, policies, audits, config, passports: passportBundle,
        conflicts, integrationStatus: integrationStatus || {}, integrationEvents: eventItems
      };
    }
  };

  global.ChangeGuardAPI = API;
})(window);
