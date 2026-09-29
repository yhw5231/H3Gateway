/* ═══ H3Gateway 后台控制台 ═══ */
"use strict";

const $ = (s) => document.querySelector(s);
const $$ = (s) => Array.from(document.querySelectorAll(s));

const state = {
  page: "dash",
  session: null,
  settings: null,
  ipv6Supported: false,
  taskOffset: 0,
  taskLimit: 25,
  taskTotal: 0,
  keyCache: [],
  // Plaintext secrets are fetched one at a time, only when the operator asks to
  // see or copy one, and cached for the rest of the session.
  keySecrets: new Map(),
  keyRevealed: new Set(),
};

/* ── HTTP ──────────────────────────────────────────────────── */
async function api(path, opts = {}) {
  const headers = Object.assign({}, opts.headers || {});
  if (opts.body !== undefined && typeof opts.body !== "string") {
    headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(opts.body);
  }
  // State-changing verbs carry a same-origin marker the server requires.
  const method = (opts.method || "GET").toUpperCase();
  if (method !== "GET" && method !== "HEAD") headers["X-Requested-With"] = "h3admin";

  const resp = await fetch(path, Object.assign({}, opts, { headers, credentials: "same-origin" }));
  const text = await resp.text();
  let data = null;
  if (text) { try { data = JSON.parse(text); } catch { data = { raw: text }; } }
  if (!resp.ok) {
    const err = new Error((data && (data.error || data.message)) || `HTTP ${resp.status}`);
    err.status = resp.status;
    err.data = data;
    throw err;
  }
  return data;
}

/* ── toast ─────────────────────────────────────────────────── */
function toast(msg, kind = "", ms = 3200) {
  const el = document.createElement("div");
  el.className = "toast " + kind;
  el.textContent = msg;
  $("#toasts").appendChild(el);
  setTimeout(() => el.remove(), ms);
}

/* ── clipboard ─────────────────────────────────────────────── */
// navigator.clipboard only exists in a secure context, and the console is
// routinely opened over plain http on a LAN address. Fall back to the legacy
// execCommand path so "复制" never depends on https.
async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    try { await navigator.clipboard.writeText(text); return true; } catch { /* fall through */ }
  }
  try {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.top = "-1000px";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    ta.setSelectionRange(0, text.length);
    const ok = document.execCommand("copy");
    ta.remove();
    return ok;
  } catch { return false; }
}

function esc(s) {
  return String(s === undefined || s === null ? "" : s)
    .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
}

function fmtTime(v) {
  if (!v) return "-";
  const d = new Date(v);
  if (isNaN(d)) return String(v);
  return d.toLocaleString("zh-CN", { hour12: false });
}

function statusBadge(s) {
  const map = {
    succeeded: ["ok", "已完成"],
    failed: ["err", "失败"],
    running: ["info", "生成中"],
    queued: ["warn", "排队中"],
    canceled: ["", "已取消"],
  };
  const [cls, label] = map[s] || ["", s || "-"];
  return `<span class="badge ${cls}">${esc(label)}</span>`;
}

/* ── banners ───────────────────────────────────────────────── */
function banner(id, kind, html) {
  let el = document.getElementById("bn-" + id);
  if (!el) {
    el = document.createElement("div");
    el.id = "bn-" + id;
    el.className = "banner " + kind;
    $("#banners").appendChild(el);
  }
  el.className = "banner " + kind;
  el.innerHTML = `<div>${html}</div><button class="close" title="关闭">✕</button>`;
  el.querySelector(".close").onclick = () => el.remove();
}
function clearBanner(id) {
  const el = document.getElementById("bn-" + id);
  if (el) el.remove();
}

/* ── modal ─────────────────────────────────────────────────── */
function openModal(html, onMount) {
  const host = $("#modal-host");
  host.innerHTML = `<div class="modal-host"><div class="modal">${html}</div></div>`;
  host.querySelector(".modal-host").addEventListener("mousedown", (e) => {
    if (e.target === e.currentTarget) closeModal();
  });
  if (onMount) onMount(host.querySelector(".modal"));
}
function closeModal() { $("#modal-host").innerHTML = ""; }
document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeModal(); });

/* ── navigation ────────────────────────────────────────────── */
$("#nav").addEventListener("click", (e) => {
  const btn = e.target.closest("button[data-page]");
  if (btn) go(btn.dataset.page);
});

function go(page) {
  state.page = page;
  $$("#nav button").forEach((b) => b.classList.toggle("on", b.dataset.page === page));
  $$(".page").forEach((p) => p.classList.toggle("hidden", p.id !== "page-" + page));
  if (page === "dash") loadDash();
  if (page === "tasks") loadTasks();
  if (page === "keys") loadKeys();
  if (page === "settings") loadSettings();
  if (page === "xff") loadProbe();
  if (page === "account") loadAccount();
}

/* ── auth ──────────────────────────────────────────────────── */
$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    const r = await api("/admin/api/login", {
      method: "POST",
      body: { username: $("#login-user").value.trim(), password: $("#login-pass").value },
    });
    $("#login-pass").value = "";
    await boot(r);
  } catch (err) {
    toast(err.message || "登录失败", "err");
  }
});

$("#logout").addEventListener("click", async () => {
  try { await api("/admin/api/logout", { method: "POST" }); } catch { /* ignore */ }
  state.session = null;
  $("#app").classList.add("hidden");
  $("#login").classList.remove("hidden");
});

async function boot(loginResult) {
  let session = loginResult;
  if (!session) {
    session = await api("/admin/api/session");
  }
  state.session = session;
  $("#who").textContent = session.username;
  $("#login").classList.add("hidden");
  $("#app").classList.remove("hidden");
  clearBanner("pw");
  if (session.must_change_password) {
    banner("pw", "warn",
      "<b>当前仍在使用默认密码 admin/admin。</b>请立即在「账号安全」中修改，修改前除改密以外的后台接口都会被拒绝。");
    go("account");
    return;
  }
  go(state.page || "dash");
}

/* ═══ 仪表盘 ═══ */
async function loadDash() {
  try {
    const d = await api("/admin/api/stats");
    state.settings = d.settings;
    state.ipv6Supported = !!d.ipv6_supported;
    renderDashStats(d);
    renderDashXFF(d);
    renderRecent(d.recent || []);
  } catch (err) {
    if (err.status === 401) return showLogin();
    toast(err.message, "err");
  }
}

function showLogin() {
  $("#app").classList.add("hidden");
  $("#login").classList.remove("hidden");
}

function renderDashStats(d) {
  const t = d.tasks || {};
  const p = d.pipeline || {};
  const r = p.rotator || {};
  const by = t.by_status || {};
  const cards = [
    { k: "任务总数", v: t.total || 0, s: `近 24h 新增 ${t.tasks_today || 0}` },
    { k: "已完成", v: by.succeeded || 0, s: "succeeded", cls: "good" },
    { k: "进行中", v: (by.queued || 0) + (by.running || 0), s: `队列 ${by.queued || 0} · 生成 ${by.running || 0}`, cls: "warn" },
    { k: "失败", v: by.failed || 0, s: "failed", cls: (by.failed || 0) > 0 ? "bad" : "" },
    { k: "有效密钥", v: t.active_api_keys || 0, s: `共 ${t.api_keys || 0} 个` },
    { k: "已铸身份", v: r.identities_minted || 0, s: `退役 ${r.identities_exhausted || 0}` },
    { k: "在飞请求", v: p.in_flight || 0, s: `并发上限 ${p.max_concurrent || 0}` },
    { k: "身份策略", v: "1 : 1", s: "每次生成独立身份" },
  ];
  $("#dash-stats").innerHTML = cards.map((c) =>
    `<div class="stat ${c.cls || ""}"><div class="k">${esc(c.k)}</div><div class="v">${esc(c.v)}</div><div class="s">${esc(c.s)}</div></div>`
  ).join("");
}

function renderDashXFF(d) {
  const s = d.settings || {};
  const r = (d.pipeline || {}).rotator || {};
  const probe = d.probe;
  const ipv6Badge = d.ipv6_supported
    ? '<span class="badge ok">已实测支持</span>'
    : (probe ? '<span class="badge err">未通过</span>' : '<span class="badge warn">未测试</span>');
  const modeNames = { off: "关闭", ipv4: "随机 IPv4", ipv6: "随机 IPv6", mixed: "混合", auto: "自动" };
  $("#dash-xff").innerHTML = `
    <dt>配置模式</dt><dd>${esc(modeNames[s.xff_mode] || s.xff_mode || "-")}</dd>
    <dt>实际生效</dt><dd>${esc(modeNames[d.effective_xff_mode] || d.effective_xff_mode || "-")}</dd>
    <dt>IPv6 支持</dt><dd>${ipv6Badge}</dd>
    <dt>地址池</dt><dd>${s.xff_pool === "reserved" ? "保留/文档地址段" : "公网地址"}</dd>
    <dt>文本变体</dt><dd>${s.xff_variants ? "开启" : "关闭"}</dd>
    <dt>代理模式</dt><dd>${r.proxy_mode ? "已配置代理（不伪造 XFF）" : "直连（伪造 XFF）"}</dd>
    <dt>接口通道</dt><dd>${s.endpoint_mode === "plain" ? "plain（画面会脱离首帧图）" : "showcase（保持首帧图）"}</dd>
    <dt>上游地址</dt><dd class="mono">${esc(s.upstream_base || "-")}</dd>`;
}

function renderRecent(items) {
  const rows = items.map((t) => `<tr>
    <td class="mono"><a href="#" data-task="${esc(t.id)}">${esc(t.id)}</a></td>
    <td>${statusBadge(t.status)}</td>
    <td class="small">${esc(t.model)} · ${esc(t.duration)}s</td>
    <td class="mono small">${esc(t.forged_ip || "-")}</td>
    <td class="small nowrap">${fmtTime(t.created_at)}</td>
  </tr>`).join("");
  $("#dash-recent").innerHTML = rows || `<tr><td colspan="5" class="empty">暂无任务</td></tr>`;
}

$("#dash-recent").addEventListener("click", (e) => {
  const a = e.target.closest("a[data-task]");
  if (a) { e.preventDefault(); showTask(a.dataset.task); }
});
$("#dash-refresh").onclick = loadDash;

$("#dash-rotator-reset").onclick = async () => {
  if (!confirm("重置身份池与统计？已提交的任务不受影响，但复用中的地址会被丢弃。")) return;
  try {
    await api("/admin/api/rotator/reset", { method: "POST" });
    toast("身份池已重置", "ok");
    loadDash();
  } catch (err) { toast(err.message, "err"); }
};

$("#dash-upstream").onclick = async () => {
  $("#upstream-hint").textContent = "探测中…";
  $("#upstream-body").innerHTML = '<span class="muted small">请求中…</span>';
  try {
    const d = await api("/admin/api/upstream");
    $("#upstream-hint").textContent = "";
    if (d.ok) {
      $("#upstream-body").innerHTML = `
        <div class="banner ok" style="margin:0 0 12px"><div>上游可达，耗时 ${esc(d.latency_ms)} ms</div></div>
        <dl class="kv">
          <dt>生成接口</dt><dd class="mono small">${esc(d.generate_url)}</dd>
          <dt>配额接口</dt><dd class="mono small">${esc(d.usage_url)}</dd>
          <dt>通道</dt><dd>${esc(d.endpoint_mode)}</dd>
          <dt>日额度</dt><dd>${esc((d.usage || {}).limit)}（已用 ${esc((d.usage || {}).used)}）</dd>
          <dt>上游并发上限</dt><dd>${esc((d.usage || {}).max_concurrent)}</dd>
        </dl>`;
    } else {
      $("#upstream-body").innerHTML = `<div class="banner err" style="margin:0"><div><b>上游不可达</b><br>${esc(d.error)}</div></div>`;
    }
  } catch (err) {
    $("#upstream-hint").textContent = "";
    $("#upstream-body").innerHTML = `<div class="banner err" style="margin:0"><div>${esc(err.message)}</div></div>`;
  }
};

/* ═══ 任务 ═══ */
async function loadTasks() {
  const params = new URLSearchParams({
    limit: state.taskLimit,
    offset: state.taskOffset,
    status: $("#task-status").value || "all",
    q: $("#task-q").value.trim(),
  });
  try {
    const d = await api("/admin/api/tasks?" + params.toString());
    state.taskTotal = d.total;
    renderTasks(d.items || []);
  } catch (err) {
    if (err.status === 401) return showLogin();
    toast(err.message, "err");
  }
}

function renderTasks(items) {
  $("#task-rows").innerHTML = items.map((t) => `<tr>
    <td class="mono"><a href="#" data-task="${esc(t.id)}">${esc(t.id)}</a></td>
    <td>${statusBadge(t.status)}</td>
    <td class="small">${esc(t.duration)}s · ${esc(t.ratio)}</td>
    <td class="mono small">${esc(t.forged_ip || "-")} ${t.ip_family ? `<span class="badge">${esc(t.ip_family)}</span>` : ""}</td>
    <td class="small">${esc(t.attempts || 0)}</td>
    <td class="small">${esc(t.api_key || "-")}</td>
    <td class="small nowrap">${fmtTime(t.created_at)}</td>
    <td class="nowrap">
      <button class="btn sm" data-act="detail" data-id="${esc(t.id)}">详情</button>
      <button class="btn sm" data-act="retry" data-id="${esc(t.id)}">重投</button>
      <button class="btn sm danger" data-act="del" data-id="${esc(t.id)}">删除</button>
    </td>
  </tr>`).join("") || `<tr><td colspan="8" class="empty">没有匹配的任务</td></tr>`;

  const from = state.taskTotal === 0 ? 0 : state.taskOffset + 1;
  const to = Math.min(state.taskOffset + state.taskLimit, state.taskTotal);
  $("#task-count").textContent = `显示 ${from}-${to} / 共 ${state.taskTotal} 条`;
  $("#task-prev").disabled = state.taskOffset <= 0;
  $("#task-next").disabled = state.taskOffset + state.taskLimit >= state.taskTotal;
}

$("#task-refresh").onclick = () => { state.taskOffset = 0; loadTasks(); };
$("#task-status").onchange = () => { state.taskOffset = 0; loadTasks(); };
$("#task-q").addEventListener("keydown", (e) => { if (e.key === "Enter") { state.taskOffset = 0; loadTasks(); } });
$("#task-prev").onclick = () => { state.taskOffset = Math.max(0, state.taskOffset - state.taskLimit); loadTasks(); };
$("#task-next").onclick = () => { state.taskOffset += state.taskLimit; loadTasks(); };

$("#task-rows").addEventListener("click", async (e) => {
  const btn = e.target.closest("button[data-act]");
  const link = e.target.closest("a[data-task]");
  if (link) { e.preventDefault(); return showTask(link.dataset.task); }
  if (!btn) return;
  const id = btn.dataset.id;
  if (btn.dataset.act === "detail") return showTask(id);
  if (btn.dataset.act === "retry") {
    btn.disabled = true;
    try {
      await api(`/admin/api/tasks/${encodeURIComponent(id)}/retry`, { method: "POST" });
      toast("已重新提交", "ok");
      loadTasks();
    } catch (err) { toast(err.message, "err"); }
    finally { btn.disabled = false; }
  }
  if (btn.dataset.act === "del") {
    if (!confirm(`确认删除任务 ${id}？本地缓存视频也会被移除。`)) return;
    try {
      await api(`/admin/api/tasks/${encodeURIComponent(id)}`, { method: "DELETE" });
      toast("已删除", "ok");
      loadTasks();
    } catch (err) { toast(err.message, "err"); }
  }
});

async function showTask(id) {
  try {
    const t = await api(`/admin/api/tasks/${encodeURIComponent(id)}`);
    const videoURL = `/v1/videos/${encodeURIComponent(id)}/content`;
    openModal(`
      <h3>任务详情</h3>
      <dl class="kv">
        <dt>任务 ID</dt><dd class="mono">${esc(t.id)}</dd>
        <dt>状态</dt><dd>${statusBadge(t.status)}</dd>
        <dt>模型 / 时长</dt><dd>${esc(t.model)} · ${esc(t.duration)}s · ${esc(t.ratio)}</dd>
        <dt>伪造 XFF</dt><dd class="mono">${esc(t.forged_ip || "（未伪造）")} ${t.ip_family ? `[${esc(t.ip_family)}]` : ""}</dd>
        <dt>上游任务</dt><dd class="mono small">${esc(t.upstream || "-")}</dd>
        <dt>client_id</dt><dd class="mono small">${esc(t.client_id || "-")}</dd>
        <dt>access_token</dt><dd class="mono small">${esc(t.access_token || "-")}</dd>
        <dt>重投次数</dt><dd>${esc(t.attempts || 0)}</dd>
        <dt>来源</dt><dd>${esc(t.source || "-")} ${t.api_key ? "· 密钥 " + esc(t.api_key) : ""}</dd>
        <dt>创建时间</dt><dd>${fmtTime(t.created_at)}</dd>
        <dt>更新时间</dt><dd>${fmtTime(t.updated_at)}</dd>
        <dt>本地缓存</dt><dd>视频 ${t.video_cached ? "有" : "无"} · 输入图 ${t.input_cached ? "有" : "无"}</dd>
        ${t.sha256 ? `<dt>成片 SHA-256</dt><dd class="mono small" title="${esc(t.sha256)}">${esc(t.sha256.slice(0, 16))}…</dd>` : ""}
        ${t.duplicate ? `<dt>重复内容</dt><dd style="color:#ffa3aa">与任务 <span class="mono">${esc(t.duplicate)}</span> 的成片字节完全相同（输入图不同）——上游复用了同一条内容</dd>` : ""}
        <dt>提示词</dt><dd>${esc(t.prompt || "（空）")}</dd>
        ${t.error ? `<dt>错误</dt><dd style="color:#ffa3aa">${esc(t.error)}</dd>` : ""}
      </dl>
      ${t.status === "succeeded" ? `<div class="mt"><video src="${videoURL}" controls playsinline style="width:100%;border-radius:8px;background:#000"></video></div>` : ""}
      <div class="row end">
        ${t.status === "succeeded" ? `<a class="btn" href="${videoURL}" download>下载 MP4</a>` : ""}
        <button class="btn" data-close>关闭</button>
      </div>`, (m) => {
      m.querySelector("[data-close]").onclick = closeModal;
    });
  } catch (err) { toast(err.message, "err"); }
}

/* ═══ API 密钥 ═══ */
async function loadKeys() {
  try {
    const d = await api("/admin/api/keys");
    state.keyCache = d.items || [];
    // Drop cached plaintext (and reveal state) for keys that no longer exist.
    const live = new Set(state.keyCache.map((k) => k.id));
    for (const id of Array.from(state.keySecrets.keys())) {
      if (!live.has(id)) { state.keySecrets.delete(id); state.keyRevealed.delete(id); }
    }
    renderKeys(d);
  } catch (err) {
    if (err.status === 401) return showLogin();
    toast(err.message, "err");
  }
}

// keySecret fetches the plaintext of one key, at most once per session.
async function keySecret(id) {
  if (state.keySecrets.has(id)) return state.keySecrets.get(id);
  const d = await api(`/admin/api/keys/${encodeURIComponent(id)}/secret`);
  const secret = d.key || "";
  state.keySecrets.set(id, secret);
  return secret;
}

function renderKeys(d) {
  const open = d.open_access;
  $("#keys-access").innerHTML = open
    ? `<div class="banner warn"><div><b>当前 /v1/* 处于开放状态。</b>
        尚未创建任何启用的密钥，且未开启「强制要求 API 密钥」，任何能访问本端口的人都可以直接调用。
        生产环境请新建密钥并在「运行设置 → 访问控制」中开启强制校验。</div></div>`
    : `<div class="banner info"><div>已启用 <b>${state.keyCache.filter((k) => k.enabled).length}</b> 个密钥${d.require_api_key ? "，且已强制校验" : ""}。</div></div>`;

  $("#key-rows").innerHTML = state.keyCache.map((k) => `<tr>
    <td>${esc(k.name)}${k.note ? `<div class="muted small">${esc(k.note)}</div>` : ""}</td>
    <td class="small">${keyCell(k)}
      <div class="row" style="gap:6px;margin-top:5px">
        <button class="btn sm" data-kact="reveal" data-id="${esc(k.id)}" data-on="${state.keyRevealed.has(k.id) ? "1" : "0"}">${state.keyRevealed.has(k.id) ? "隐藏" : "显示"}</button>
        <button class="btn sm" data-kact="copy" data-id="${esc(k.id)}">复制</button>
      </div>
    </td>
    <td>${k.expired ? '<span class="badge err">已过期</span>' : (k.enabled ? '<span class="badge ok">启用</span>' : '<span class="badge">停用</span>')}</td>
    <td class="small">${esc(k.requests || 0)}</td>
    <td class="small nowrap">${fmtTime(k.last_used_at)}</td>
    <td class="small">${k.rate_limit_per_min ? esc(k.rate_limit_per_min) + " / 分钟" : "不限"}</td>
    <td class="small nowrap">${fmtTime(k.created_at)}</td>
    <td class="nowrap">
      <button class="btn sm" data-kact="toggle" data-id="${esc(k.id)}" data-on="${k.enabled ? "1" : "0"}">${k.enabled ? "停用" : "启用"}</button>
      <button class="btn sm danger" data-kact="del" data-id="${esc(k.id)}" data-name="${esc(k.name)}">删除</button>
    </td>
  </tr>`).join("") || `<tr><td colspan="8" class="empty">还没有 API 密钥</td></tr>`;
}

// keyCell renders either the masked prefix or the plaintext already revealed.
function keyCell(k) {
  const shown = state.keyRevealed.has(k.id) ? state.keySecrets.get(k.id) : "";
  return shown
    ? `<span class="mono" data-keytext style="word-break:break-all">${esc(shown)}</span>`
    : `<span class="mono" data-keytext>${esc(k.prefix)}</span>`;
}

$("#key-rows").addEventListener("click", async (e) => {
  const btn = e.target.closest("button[data-kact]");
  if (!btn) return;
  const id = btn.dataset.id;
  const act = btn.dataset.kact;

  if (act === "reveal" || act === "copy") {
    let secret;
    try { secret = await keySecret(id); }
    catch (err) { toast(err.message, "err"); return; }
    if (act === "copy") {
      const ok = await copyText(secret);
      toast(ok ? "完整密钥已复制" : "复制失败，请点「显示」后手动选中", ok ? "ok" : "err");
      return;
    }
    const tr = btn.closest("tr");
    const cell = tr && tr.querySelector("[data-keytext]");
    if (state.keyRevealed.has(id)) {
      state.keyRevealed.delete(id);
      btn.dataset.on = "0";
      btn.textContent = "显示";
      if (cell) {
        cell.style.wordBreak = "";
        cell.textContent = (state.keyCache.find((k) => k.id === id) || {}).prefix || "";
      }
    } else {
      state.keyRevealed.add(id);
      btn.dataset.on = "1";
      btn.textContent = "隐藏";
      if (cell) {
        cell.style.wordBreak = "break-all";
        cell.textContent = secret;
      }
    }
    return;
  }

  if (act === "toggle") {
    try {
      await api(`/admin/api/keys/${encodeURIComponent(id)}`, {
        method: "PATCH", body: { enabled: btn.dataset.on !== "1" },
      });
      toast("已更新", "ok");
      loadKeys();
    } catch (err) { toast(err.message, "err"); }
  }
  if (act === "del") {
    if (!confirm(`确认删除密钥「${btn.dataset.name}」？使用该密钥的客户端会立即失效。`)) return;
    try {
      await api(`/admin/api/keys/${encodeURIComponent(id)}`, { method: "DELETE" });
      state.keySecrets.delete(id);
      state.keyRevealed.delete(id);
      toast("已删除", "ok");
      loadKeys();
    } catch (err) { toast(err.message, "err"); }
  }
});

$("#key-new").onclick = () => {
  openModal(`
    <h3>新建 API 密钥</h3>
    <label class="field"><span class="lb">名称</span>
      <input type="text" id="nk-name" placeholder="例如：生产环境 / 张三的客户端" /></label>
    <label class="field"><span class="lb">备注（可选）</span>
      <input type="text" id="nk-note" /></label>
    <div class="grid cols-2">
      <label class="field"><span class="lb">有效期（天，留空为永久）</span>
        <input type="number" id="nk-exp" min="1" placeholder="永久" /></label>
      <label class="field"><span class="lb">限速（次/分钟，0 为不限）</span>
        <input type="number" id="nk-rate" min="0" value="0" /></label>
    </div>
    <div class="row end">
      <button class="btn" data-close>取消</button>
      <button class="btn primary" id="nk-ok">创建</button>
    </div>`, (m) => {
    m.querySelector("[data-close]").onclick = closeModal;
    m.querySelector("#nk-ok").onclick = async () => {
      try {
        const r = await api("/admin/api/keys", {
          method: "POST",
          body: {
            name: m.querySelector("#nk-name").value.trim(),
            note: m.querySelector("#nk-note").value.trim(),
            expires_in_days: parseInt(m.querySelector("#nk-exp").value, 10) || 0,
            rate_limit_per_min: parseInt(m.querySelector("#nk-rate").value, 10) || 0,
          },
        });
        showNewKey(r);
        loadKeys();
      } catch (err) { toast(err.message, "err"); }
    };
  });
};

function showNewKey(k) {
  openModal(`
    <h3>密钥已创建</h3>
    <div class="banner warn" style="margin-bottom:14px"><div>请立即复制保存。之后仍可在密钥列表里点「显示」查看、点「复制」再次复制。</div></div>
    <label class="field"><span class="lb">${esc(k.name)}</span>
      <input type="text" id="newkey" class="mono" readonly value="${esc(k.key)}" /></label>
    <div class="row end">
      <button class="btn" id="copykey">复制</button>
      <button class="btn primary" data-close>完成</button>
    </div>`, (m) => {
    m.querySelector("[data-close]").onclick = closeModal;
    m.querySelector("#copykey").onclick = async () => {
      const input = m.querySelector("#newkey");
      input.select();
      input.setSelectionRange(0, k.key.length);
      const ok = await copyText(k.key);
      toast(ok ? "已复制" : "复制失败，请手动复制", ok ? "ok" : "err");
    };
  });
}

/* ═══ 设置 ═══ */
async function loadSettings() {
  try {
    const d = await api("/admin/api/settings");
    state.settings = d.settings;
    state.ipv6Supported = !!d.ipv6_supported;
    fillSettings(d.settings, d);
  } catch (err) {
    if (err.status === 401) return showLogin();
    toast(err.message, "err");
  }
}

function fillSettings(s, d) {
  $("#set-xff-mode").value = s.xff_mode;
  $("#set-xff-pool").value = s.xff_pool || "public";
  $("#set-xff-variants").checked = !!s.xff_variants;
  $("#set-upstream").value = s.upstream_base || "";
  $("#set-trial-base").value = s.trial_base || "";
  $("#set-endpoint-mode").value = s.endpoint_mode || "showcase";
  $("#set-showcase").value = s.showcase_id || "";
  $("#set-source-host").value = s.source_host || "";
  $("#set-prompt").value = s.default_prompt || "";
  $("#set-max-conc").value = s.max_concurrent;
  $("#set-submit-timeout").value = s.submit_timeout_sec;
  $("#set-poll-interval").value = s.poll_interval_sec;
  $("#set-resubmits").value = s.task_resubmits;
  $("#set-resubmit-backoff").value = s.resubmit_backoff_sec;
  $("#set-image-max").value = s.image_max_bytes;
  $("#set-retention").value = s.task_retention;
  $("#set-proxy").value = (s.proxy_list || []).join(", ");
  $("#set-require-key").checked = !!s.require_api_key;
  $("#set-studio-session").checked = !!s.studio_session_enabled;
  $("#set-trust-proxy").checked = !!s.trust_proxy;
  $("#set-public-url").value = s.public_base_url || "";

  const ipv6Opt = $('#set-xff-mode option[value="ipv6"]');
  ipv6Opt.disabled = false;
  const supported = !!d.ipv6_supported;
  $("#set-xff-warn").innerHTML = supported
    ? `<div class="banner ok"><div><b>已实测：上游把 IPv6 形式的 X-Forwarded-For 也当作独立配额键。</b>
        可选择「随机公网 IPv6」或「混合」模式获得近乎无限的地址空间。</div></div>`
    : `<div class="banner warn"><div>尚未确认上游支持 IPv6 配额键。请先到
        <a href="#" id="go-xff">XFF / IPv6 测试</a> 运行一次完整测试，通过后再启用 IPv6 模式。</div></div>`;
  const goXFF = document.getElementById("go-xff");
  if (goXFF) goXFF.onclick = (e) => { e.preventDefault(); go("xff"); };

  $("#set-xff-mode-help").textContent = supported
    ? "当前测试结论：IPv4 与 IPv6 均被识别为独立配额键。"
    : "当前测试结论：未确认 IPv6，建议使用「自动」或「随机公网 IPv4」。";
}

$("#set-reload").onclick = loadSettings;

$("#set-save").onclick = async () => {
  const body = {
    xff_mode: $("#set-xff-mode").value,
    xff_pool: $("#set-xff-pool").value,
    xff_variants: $("#set-xff-variants").checked,
    upstream_base: $("#set-upstream").value.trim(),
    trial_base: $("#set-trial-base").value.trim(),
    endpoint_mode: $("#set-endpoint-mode").value,
    showcase_id: $("#set-showcase").value.trim(),
    source_host: $("#set-source-host").value.trim(),
    default_prompt: $("#set-prompt").value,
    max_concurrent: parseInt($("#set-max-conc").value, 10) || 0,
    submit_timeout_sec: parseInt($("#set-submit-timeout").value, 10) || 0,
    poll_interval_sec: parseFloat($("#set-poll-interval").value) || 0,
    task_resubmits: parseInt($("#set-resubmits").value, 10) || 0,
    resubmit_backoff_sec: parseInt($("#set-resubmit-backoff").value, 10) || 0,
    image_max_bytes: parseInt($("#set-image-max").value, 10) || 0,
    task_retention: parseInt($("#set-retention").value, 10) || 0,
    proxy_list: $("#set-proxy").value.split(",").map((x) => x.trim()).filter(Boolean),
    require_api_key: $("#set-require-key").checked,
    studio_session_enabled: $("#set-studio-session").checked,
    trust_proxy: $("#set-trust-proxy").checked,
    public_base_url: $("#set-public-url").value.trim(),
  };
  try {
    await api("/admin/api/settings", { method: "PUT", body });
    toast("设置已保存", "ok");
    loadSettings();
  } catch (err) { toast(err.message, "err"); }
};

/* ═══ XFF / IPv6 测试 ═══ */
async function loadProbe() {
  try {
    const d = await api("/admin/api/probe");
    renderProbe(d.probe);
  } catch (err) {
    if (err.status === 401) return showLogin();
    toast(err.message, "err");
  }
}

function renderProbe(p) {
  if (!p) {
    $("#probe-result").innerHTML = `<div class="card"><div class="empty">还没有测试记录。点击「运行完整测试」以确认 IPv6 是否可作为配额键。</div></div>`;
    return;
  }
  // A dry run only checks reachability: it never submits a generation, so it
  // cannot claim anything about the quota key. Say so instead of "未通过".
  const verdict = p.dry_run
    ? `<div class="banner ${p.ok ? "warn" : "err"}"><div><b>${p.ok ? "连通性正常（仅连通性检查，未验证配额键）" : "上游不可达。"}</b>${esc(p.message)}</div></div>`
    : p.ok && p.ipv6_accepted
      ? `<div class="banner ok"><div><b>结论：支持随机公网 IPv6。</b>${esc(p.message)}</div></div>`
      : p.ok && p.ipv4_accepted
        ? `<div class="banner warn"><div><b>结论：仅 IPv4 可用。</b>${esc(p.message)}</div></div>`
        : `<div class="banner err"><div><b>结论：未通过。</b>${esc(p.message)}</div></div>`;

  const rows = (p.steps || []).map((s) => esc(s)).join("\n");
  const cell = (label, ok) => p.dry_run
    ? `<div class="stat"><div class="k">${label}</div><div class="v">未测试</div></div>`
    : `<div class="stat ${ok ? "good" : "bad"}"><div class="k">${label}</div><div class="v">${ok ? "支持" : "不支持"}</div></div>`;
  $("#probe-result").innerHTML = `
    ${verdict}
    <div class="card">
      <h3>最近一次测试 <span class="hint">${fmtTime(p.ran_at)}${p.dry_run ? " · 仅连通性检查" : ""}</span></h3>
      <div class="grid cols-3" style="margin-bottom:14px">
        <div class="stat ${p.ok ? "good" : "bad"}"><div class="k">上游可达</div><div class="v">${p.ok ? "是" : "否"}</div></div>
        ${cell("IPv4 作为配额键", p.ipv4_accepted)}
        ${cell("IPv6 作为配额键", p.ipv6_accepted)}
      </div>
      <dl class="kv" style="margin-bottom:14px">
        <dt>上游</dt><dd class="mono small">${esc(p.upstream)}</dd>
      </dl>
      <div class="section-title">执行步骤</div>
      <pre class="steps">${rows || "（无）"}</pre>
      ${!p.dry_run && p.ipv6_accepted ? `<div class="row mt"><button class="btn primary" id="apply-ipv6">启用随机 IPv6 模式</button>
        <button class="btn" id="apply-mixed">启用混合模式</button></div>` : ""}
    </div>`;

  const apply = async (mode) => {
    try {
      const d = await api("/admin/api/settings");
      const s = Object.assign({}, d.settings, { xff_mode: mode });
      await api("/admin/api/settings", { method: "PUT", body: s });
      toast("已切换到 " + (mode === "ipv6" ? "随机 IPv6" : "混合") + " 模式", "ok");
      loadProbe();
    } catch (err) { toast(err.message, "err"); }
  };
  const b6 = document.getElementById("apply-ipv6");
  if (b6) b6.onclick = () => apply("ipv6");
  const bm = document.getElementById("apply-mixed");
  if (bm) bm.onclick = () => apply("mixed");
}

$("#probe-dry").onclick = () => runProbe(true);
$("#probe-run").onclick = () => runProbe(false);

async function runProbe(dryRun) {
  const btns = [$("#probe-dry"), $("#probe-run")];
  btns.forEach((b) => (b.disabled = true));
  const label = dryRun ? "连通性检查" : "完整测试";
  toast(label + "进行中…", "", 8000);
  try {
    const d = await api("/admin/api/probe", { method: "POST", body: { dry_run: dryRun } });
    renderProbe(d.probe);
    toast(label + "完成", "ok");
  } catch (err) {
    toast(err.message, "err");
  } finally {
    btns.forEach((b) => (b.disabled = false));
  }
}

/* ═══ 账号 ═══ */
async function loadAccount() {
  try {
    const d = await api("/admin/api/settings");
    $("#acct-info").innerHTML = `
      <dt>用户名</dt><dd>${esc((state.session || {}).username || "-")}</dd>
      <dt>会话到期</dt><dd>${fmtTime((state.session || {}).expires_at)}</dd>
      <dt>监听地址</dt><dd class="mono">${esc((d.env || {}).host)}:${esc((d.env || {}).port)}</dd>
      <dt>数据目录</dt><dd class="mono small">${esc((d.env || {}).data_dir)}</dd>
      <dt>数据文件</dt><dd class="mono small">${esc((d.env || {}).db_path)}</dd>`;
  } catch (err) {
    if (err.status === 401) return showLogin();
    toast(err.message, "err");
  }
}

$("#pw-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const cur = $("#pw-current").value, nw = $("#pw-new").value, cf = $("#pw-confirm").value;
  if (nw !== cf) return toast("两次输入的新密码不一致", "err");
  if (nw.length < 8) return toast("新密码至少 8 位", "err");
  try {
    await api("/admin/api/password", { method: "POST", body: { current_password: cur, new_password: nw } });
    toast("密码已更新", "ok");
    $("#pw-form").reset();
    clearBanner("pw");
    state.session.must_change_password = false;
    loadDash();
  } catch (err) { toast(err.message, "err"); }
});

/* ═══ boot ═══ */
(async function init() {
  try {
    await boot(null);
  } catch {
    $("#login").classList.remove("hidden");
  }
})();
