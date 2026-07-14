const state = {
  user: null,
  page: "overview",
  draft: null,
  active: null,
  venueTypes: [],
  credentialOptions: [],
  credentials: [],
  users: [],
  userRoleOptions: [],
  roles: [],
  permissions: [],
  runtime: { engine: { online: false, ready: false, version: 0, desired_version: 0 }, instruments: [] },
  selectedPairId: null,
  selectedRoleId: null,
  pairTab: "basic",
  search: "",
  dirty: false,
  ui: {
    createPair: false,
    createVenue: false,
    createCredential: false,
    editCredentialId: null,
    editUserId: null,
    passwordModal: false,
    validationIssues: [],
    safetyAdjustments: 0,
    pairInspection: {},
    confirm: null,
  },
};

const $ = (selector) => document.querySelector(selector);
const esc = (value) => String(value ?? "").replace(/[&<>"']/g, (char) => ({
  "&": "&amp;",
  "<": "&lt;",
  ">": "&gt;",
  '"': "&quot;",
  "'": "&#39;",
})[char]);

async function api(path, options = {}) {
  const response = await fetch(path, {
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
    ...options,
  });
  if (response.status === 204) return null;
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `请求失败 ${response.status}`);
  return body;
}

function toast(message, bad = false) {
  const node = $("#toast");
  node.textContent = message;
  node.className = bad ? "show bad" : "show";
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { node.className = ""; }, 3400);
}

function has(permission) {
  return state.user?.permissions?.includes(permission);
}

function emptyConfig() {
  return {
    mode: "shadow",
    poll_interval_ms: 2000,
    audit_path: "/var/lib/fluxmaker/audit/events.jsonl",
    audit_max_bytes: 104857600,
    audit_backups: 7,
    heartbeat_path: "/var/lib/fluxmaker/run/heartbeat",
    watchdog_timeout_seconds: 15,
    market_failure_threshold: 3,
    market_recovery_threshold: 3,
    market_error_grace_seconds: 15,
    trading_progress_timeout_seconds: 120,
    rules_refresh_seconds: 300,
    max_concurrent_instruments: 4,
    rpc: { urls: [], chain_id: 56, request_timeout_ms: 5000 },
    instruments: [],
    venues: {},
  };
}

async function boot() {
  try {
    state.user = await api("/api/me");
    await loadInitialData();
    render();
  } catch {
    state.user = null;
    renderLogin();
  }
}

async function loadInitialData() {
  await loadConfig(false);
  if (has("venue:view")) await Promise.all([loadCredentialOptions(false), loadVenueTypes(false)]);
  if (has("runtime:view")) await loadRuntime(false);
  startRuntimePolling();
}

let runtimePollTimer = null;

function startRuntimePolling() {
  if (runtimePollTimer) clearInterval(runtimePollTimer);
  if (!has("runtime:view")) return;
  runtimePollTimer = setInterval(async () => {
    if (!state.user) return;
    try {
      await loadRuntime(false);
      if (["overview", "runtime"].includes(state.page) || (state.page === "pair" && state.pairTab === "runtime")) render();
    } catch {
      // The next poll will retry. Authentication errors are handled by normal navigation.
    }
  }, 3000);
}

async function loadConfig(redraw = true) {
  state.draft = await api("/api/config/draft").catch((error) => (
    error.message.includes("no draft") ? emptyConfig() : Promise.reject(error)
  ));
  state.active = await api("/api/config/active").catch(() => null);
  state.ui.safetyAdjustments = applyLiveSafetyDefaults(state.draft);
  state.ui.validationIssues = [];
  state.dirty = state.ui.safetyAdjustments > 0;
  if (redraw) render();
}

async function loadCredentialOptions(redraw = true) {
  state.credentialOptions = (await api("/api/credential-options")) || [];
  if (redraw) render();
}

async function loadVenueTypes(redraw = true) {
  state.venueTypes = (await api("/api/venue-types")) || [];
  if (redraw) render();
}

function renderLogin() {
  $("#app").innerHTML = `
    <main class="login-shell">
      <section class="login-story">
        <div class="brand brand-large"><span class="brand-mark">F</span><span>FluxMaker</span></div>
        <div class="login-copy">
          <span class="eyebrow">Liquidity operations</span>
          <h1>让每一个币对，都有清晰可控的运行边界。</h1>
          <p>统一管理链上参考价、交易市场、账号凭证和库存风险。所有配置与敏感操作都会被记录。</p>
          <div class="login-points">
            <span>配置分步校验</span><span>凭证加密保存</span><span>Shadow 优先</span>
          </div>
        </div>
      </section>
      <section class="login-form-wrap">
        <form class="login-card" id="login">
          <div class="mobile-brand"><span class="brand-mark">F</span> FluxMaker</div>
          <span class="eyebrow">管理控制台</span>
          <h2>欢迎回来</h2>
          <p>使用管理员分配给你的账户登录。</p>
          <div class="field"><label>邮箱</label><input name="email" type="email" required autocomplete="username" placeholder="admin@example.com"></div>
          <div class="field"><label>密码</label><input name="password" type="password" required autocomplete="current-password" placeholder="输入密码"></div>
          <button class="btn primary btn-block" type="submit">登录控制台</button>
        </form>
      </section>
    </main>`;
  $("#login").onsubmit = login;
}

async function login(event) {
  event.preventDefault();
  const button = event.currentTarget.querySelector("button[type=submit]");
  button.disabled = true;
  button.textContent = "正在登录…";
  try {
    const result = await api("/api/login", {
      method: "POST",
      body: JSON.stringify(Object.fromEntries(new FormData(event.currentTarget))),
    });
    state.user = result.user;
    await loadInitialData();
    render();
  } catch (error) {
    toast(error.message, true);
    button.disabled = false;
    button.textContent = "登录控制台";
  }
}

const navigation = [
  { label: "运营", items: [
    ["overview", "总览", "dashboard:view", "◫"],
    ["runtime", "运行监控", "runtime:view", "◉"],
    ["pairs", "币对管理", "instrument:view", "◇"],
  ] },
  { label: "交易基础设施", items: [
    ["venues", "交易所", "venue:view", "⌁"],
    ["credentials", "交易所凭证", "secrets:manage", "⌘"],
  ] },
  { label: "系统", items: [
    ["settings", "运行与 RPC", "config:view", "⚙"],
    ["users", "用户", "users:manage", "◎"],
    ["roles", "角色与权限", "roles:manage", "▦"],
  ] },
];

function navHTML() {
  return navigation.map((group) => {
    const items = group.items.filter((item) => has(item[2]));
    if (!items.length) return "";
    return `<div class="nav-group"><span>${group.label}</span>${items.map(([id, label, , icon]) => `
      <button data-page="${id}" class="${state.page === id || (id === "pairs" && state.page === "pair") ? "active" : ""}">
        <i>${icon}</i><b>${label}</b>
      </button>`).join("")}</div>`;
  }).join("");
}

function render() {
  if (!state.user) return renderLogin();
  const meta = pageMeta();
  $("#app").innerHTML = `
    <div class="shell">
      <aside class="sidebar">
        <div class="brand"><span class="brand-mark">F</span><span>FluxMaker</span></div>
        <nav class="nav">${navHTML()}</nav>
        <div class="sidebar-foot">
          <div class="avatar">${esc(state.user.email?.slice(0, 1).toUpperCase())}</div>
          <div><strong>${esc(state.user.email)}</strong><span>${state.user.all_instruments ? "全部币对" : `${state.user.instruments?.length || 0} 个币对`}</span></div>
          <button class="icon-btn" id="change-password" title="修改我的密码">⌘</button><button class="icon-btn" id="logout" title="退出登录">↪</button>
        </div>
      </aside>
      <main class="main">
        <header class="topbar">
          <div><span class="eyebrow">${esc(meta.kicker)}</span><h1>${esc(meta.title)}</h1><p>${esc(meta.subtitle)}</p></div>
          <div class="top-actions">${topActions()}</div>
        </header>
        ${state.dirty ? `<div class="draft-banner"><span><b>${state.ui.safetyAdjustments ? "已自动应用 Live 安全修正" : "有未保存修改"}</b> · ${state.ui.safetyAdjustments ? `已关闭 ${state.ui.safetyAdjustments} 个币对的暖机 Spot，点保存即生效` : "点“保存”即刻生效"}</span><button class="btn small" id="discard">放弃修改</button></div>` : ""}
        ${state.ui.validationIssues.length ? validationIssuesPanel() : ""}
        <section id="content">${pageHTML()}</section>
      </main>
    </div>
    ${state.ui.passwordModal ? ownPasswordModal() : ""}
    ${state.ui.confirm ? confirmModal() : ""}`;
  bind();
}

function validationIssuesPanel() {
  return `<section class="validation-issues" role="alert"><div><span class="guide-icon">!</span><div><strong>保存前还需要处理 ${state.ui.validationIssues.length} 项</strong><p>下面是具体位置和原因，不需要逐页猜测。</p></div></div><button class="icon-btn" id="validation-close" type="button" aria-label="关闭">×</button><ol>${state.ui.validationIssues.map((issue, index) => `<li><span>${esc(issue)}</span><button class="btn small" data-validation-issue="${index}">去处理</button></li>`).join("")}</ol></section>`;
}

function confirmModal() {
  const c = state.ui.confirm;
  if (!c) return "";
  return `<div class="modal-backdrop" id="confirm-modal" role="dialog" aria-modal="true" aria-labelledby="confirm-modal-title">
    <section class="publish-modal confirm-modal">
      <div class="modal-head"><span class="modal-icon ${c.danger ? "danger" : ""}">${c.danger ? "!" : "?"}</span><div><h2 id="confirm-modal-title">${esc(c.title)}</h2><p>${esc(c.body)}</p></div></div>
      <div class="modal-actions"><button class="btn" id="confirm-cancel" type="button">取消</button><button class="btn ${c.danger ? "danger-solid" : "primary"}" id="confirm-ok" type="button">${esc(c.label)}</button></div>
    </section>
  </div>`;
}

function ownPasswordModal() {
  return `<div class="modal-backdrop" id="password-modal" role="dialog" aria-modal="true" aria-labelledby="password-modal-title"><section class="publish-modal"><div class="modal-head"><span class="modal-icon">⌘</span><div><span class="eyebrow">ACCOUNT SECURITY</span><h2 id="password-modal-title">修改我的密码</h2><p>更新当前账户的登录凭证。</p></div><button class="icon-btn" id="password-cancel-x" type="button" aria-label="关闭">×</button></div><form id="change-own-password"><div class="form-grid"><div class="field span-2"><label>当前密码</label><input name="current_password" type="password" required autocomplete="current-password"></div><div class="field span-2"><label>新密码</label><input name="new_password" type="password" minlength="12" required autocomplete="new-password"></div></div><div class="form-actions"><span>修改成功后，所有旧会话都会失效。</span><div><button class="btn" id="password-cancel" type="button">取消</button> <button class="btn primary" type="submit">修改并重新登录</button></div></div></form></section></div>`;
}

function pageMeta() {
  if (state.page === "pair") {
    const pair = selectedPair();
    return { kicker: "币对管理 / 详情", title: pair ? `${pair.base.symbol}/${pair.quote.symbol}` : "币对详情", subtitle: pair?.id || "" };
  }
  return ({
    overview: { kicker: "OPERATIONS", title: "运行总览", subtitle: "先确认配置完整性，再进入 Shadow 或实盘。" },
    runtime: { kicker: "LIVE OPERATIONS", title: "运行监控", subtitle: "实时查看指数价、盘口、库存、挂单、成交和连接状态。" },
    pairs: { kicker: "INSTRUMENTS", title: "币对管理", subtitle: "每个币对独立维护参考价、交易市场和风控参数。" },
    venues: { kicker: "INFRASTRUCTURE", title: "交易所", subtitle: "维护平台级连接能力，币对映射放在币对详情中。" },
    credentials: { kicker: "SECURITY", title: "交易所凭证", subtitle: "一个凭证代表一个交易账号，密钥保存后不再回显。" },
    settings: { kicker: "SYSTEM", title: "运行与 RPC", subtitle: "全局运行模式、BNB Chain 节点与安全超时。" },
    users: { kicker: "ACCESS", title: "用户", subtitle: "创建后台账户并分配角色。" },
    roles: { kicker: "ACCESS", title: "角色与权限", subtitle: "通过勾选菜单权限和币对范围完成授权。" },
  })[state.page] || { kicker: "", title: "控制台", subtitle: "" };
}

function topActions() {
  const configPages = ["overview", "pairs", "pair", "venues", "settings"];
  if (!configPages.includes(state.page)) return "";
  return has("config:edit")
    ? `<button class="btn primary" id="save" ${state.dirty ? "" : "disabled"}>${state.dirty ? "保存" : "已保存"}</button>`
    : "";
}

function stableJSON(value) {
  if (Array.isArray(value)) return `[${value.map(stableJSON).join(",")}]`;
  if (value && typeof value === "object") return `{${Object.keys(value).filter((key) => value[key] !== undefined).sort().map((key) => `${JSON.stringify(key)}:${stableJSON(value[key])}`).join(",")}}`;
  return JSON.stringify(value ?? null);
}

function pageHTML() {
  return ({
    overview: overviewPage,
    runtime: runtimePage,
    pairs: pairsPage,
    pair: pairPage,
    venues: venuesPage,
    credentials: credentialsPage,
    settings: settingsPage,
    users: usersPage,
    roles: rolesPage,
  })[state.page]?.() || "";
}

function overviewPage() {
  const allIssues = configIssues();
  const pairs = state.draft.instruments || [];
  const ready = pairs.filter((pair) => pairIssues(pair).length === 0).length;
  const activeVenues = Object.values(state.draft.venues || {}).filter((venue) => venue.enabled).length;
  const runtimePairs = state.runtime.instruments || [];
  const running = runtimePairs.filter((item) => item.status === "running").length;
  const paused = runtimePairs.filter((item) => item.paused).length;
  const applyingVersion = state.runtime.engine?.online && state.runtime.engine?.ready && Number(state.runtime.engine.desired_version || 0) > Number(state.runtime.engine.version || 0);
  return `
    <div class="hero-card">
      <div>
        <span class="status-dot ${state.draft.mode === "live" ? "live" : "shadow"}"></span>
        <span class="eyebrow">当前配置模式</span>
        <h2>${state.draft.mode === "live" ? "实盘模式" : "Shadow 模式"}</h2>
        <p>${state.draft.mode === "live" ? "已启用交易所会发送真实订单。" : "仅生成报价计划，不会向交易所发送订单。"}</p>
      </div>
      <div class="hero-side"><span>交易引擎</span><strong class="${state.runtime.engine?.online && state.runtime.engine?.ready ? "online-text" : "offline-text"}">${!state.runtime.engine?.online ? "离线" : applyingVersion ? "在线 · 应用新配置中" : state.runtime.engine?.ready ? "在线" : "在线 · 运行未就绪"}</strong><small>${running} 运行 · ${paused} 暂停</small><div class="progress"><i style="width:${state.runtime.engine?.online && state.runtime.engine?.ready ? 100 : 8}%"></i></div></div>
    </div>
    <div class="metrics">
      ${metric("币对", pairs.length, `${ready} 个配置就绪`)}
      ${metric("正在运行", running, `${paused} 个已暂停`)}
      ${metric("交易所", activeVenues, `${Object.keys(state.draft.venues || {}).length} 个已维护`)}
      ${metric("配置", state.active ? "已生效" : "未配置", state.active ? formatDate(state.active.published_at) : "—")}
    </div>
    <div class="content-grid overview-grid">
      <section class="panel span-2">
        <div class="panel-head"><div><h2>币对准备情况</h2><p>这里只展示配置状态，不冒充交易运行数据。</p></div><button class="btn small" data-page="pairs">管理币对</button></div>
        ${pairs.length ? `<div class="data-list">${pairs.map(pairRow).join("")}</div>` : emptyState("还没有币对", "从第一个币对开始配置链上价格和交易市场。", "新增币对", "show-create-pair")}
      </section>
      <section class="panel">
        <div class="panel-head"><div><h2>保存前检查</h2><p>严格校验在保存时执行。</p></div></div>
        ${allIssues.length ? `<ul class="issue-list">${allIssues.slice(0, 8).map((issue) => `<li><span>!</span>${esc(issue)}</li>`).join("")}</ul>` : `<div class="success-state"><span>✓</span><strong>配置检查通过</strong><p>可以保存并生效。</p></div>`}
      </section>
    </div>`;
}

function metric(label, value, helper) {
  return `<div class="metric"><span>${label}</span><strong>${esc(value)}</strong><small>${esc(helper)}</small></div>`;
}

function runtimePage() {
  const engine = state.runtime.engine || {};
  const instruments = state.runtime.instruments || [];
  const applying = engine.online && engine.ready && Number(engine.desired_version || 0) > Number(engine.version || 0);
  const engineHeading = !engine.online ? "交易引擎离线" : applying ? "正在应用新配置" : engine.ready ? "交易引擎在线" : "引擎在线，运行未就绪";
  const performance = engine.performance;
  const performanceText = performance ? ` · 最近一轮 ${performance.duration_ms}ms · ${performance.succeeded}/${performance.instruments} 成功 · 并发上限 ${performance.concurrent_limit}` : "";
  const monitoring = state.runtime.monitoring || { status: "healthy", critical: 0, warnings: 0, alerts: [] };
  const engineDetail = !engine.online ? "未检测到最近 15 秒内的引擎心跳" : applying ? `正在应用新配置 · ${engine.error || "预检中"}` : engine.ready ? `进程心跳 ${formatRelative(engine.last_heartbeat)} · 交易进度 ${engine.last_trading_progress ? formatRelative(engine.last_trading_progress) : "等待首轮"}${performanceText}` : (engine.error || "等待配置生效");
  return `
    <div class="engine-banner ${engine.online && engine.ready ? "online" : engine.online ? "waiting" : "offline"}">
      <div><span class="connection-pulse"></span><div><span class="eyebrow">ENGINE STATUS</span><h2>${engineHeading}</h2><p>${esc(engineDetail)}</p></div></div>
      <button class="btn" data-action="refresh-runtime">刷新状态</button>
    </div>
    ${monitoringPanel(monitoring)}
    ${instruments.length ? `<div class="runtime-grid">${instruments.map(runtimeCard).join("")}</div>` : emptyState("暂无运行币对", "保存配置并启动交易引擎后，运行快照会出现在这里。", "前往币对管理", "back-pairs")}`;
}

function monitoringPanel(monitoring) {
  if (monitoring.status === "healthy") return `<section class="monitor-panel healthy"><div><span>✓</span><div><strong>运行监控正常</strong><p>当前没有需要处理的运行告警</p></div></div><small>更新 ${formatRelative(monitoring.generated_at)}</small></section>`;
  return `<section class="monitor-panel ${monitoring.status}"><div class="monitor-head"><div><span>!</span><div><strong>${monitoring.critical ? `${monitoring.critical} 个严重告警` : `${monitoring.warnings} 个警告`}</strong><p>严重 ${monitoring.critical || 0} · 警告 ${monitoring.warnings || 0}</p></div></div><small>更新 ${formatRelative(monitoring.generated_at)}</small></div><div class="monitor-alerts">${(monitoring.alerts || []).slice(0, 12).map((alert) => `<div class="monitor-alert ${esc(alert.severity)}"><i>${alert.severity === "critical" ? "严重" : "警告"}</i><span><b>${esc(alert.message)}</b><small>${esc([alert.instrument_id, alert.venue, alert.code].filter(Boolean).join(" · "))}</small></span></div>`).join("")}</div></section>`;
}

function runtimeCard(snapshot) {
  const reference = snapshot.reference?.price;
  const openOrders = (snapshot.venues || []).reduce((sum, item) => sum + (item.open_orders || []).length, 0);
  const fills = (snapshot.venues || []).reduce((sum, item) => sum + (item.fills || []).length, 0);
  return `<article class="runtime-card">
    <div class="runtime-card-head"><div class="pair-identity"><span class="token-icon">${esc(snapshot.base_symbol?.slice(0, 1) || "?")}</span><div><h3>${esc(snapshot.base_symbol || snapshot.instrument_id)}/${esc(snapshot.quote_symbol || "")}</h3><p>${esc(snapshot.instrument_id)}</p></div></div>${runtimeBadge(snapshot)}</div>
    <div class="runtime-values"><span><small>指数价</small><b>${reference ? esc(reference) : "—"}</b></span><span><small>总库存</small><b>${snapshot.inventory_available ? esc(snapshot.inventory) : "—"}</b></span><span><small>挂单</small><b>${openOrders}</b></span><span><small>近期成交</small><b>${fills}</b></span></div>
    <div class="connection-strip">${(snapshot.venues || []).map((venue) => `<span><i class="${venue.market_connected ? "ok" : "bad"}"></i>${esc(venue.name)}${venue.account_connected ? " · 账户在线" : " · 仅行情"}</span>`).join("") || `<span><i class="bad"></i>等待首个运行快照</span>`}</div>
    ${snapshot.error ? `<div class="runtime-error">${esc(snapshot.error)}</div>` : ""}
    <div class="card-footer"><span>更新 ${formatRelative(snapshot.updated_at)}</span><button class="btn small" data-open-runtime="${esc(snapshot.instrument_id)}">查看运行详情</button></div>
  </article>`;
}

function runtimeBadge(snapshot) {
  const labels = { running: "运行中", pausing: "撤单中", paused: "已暂停", resuming: "恢复中", degraded: "异常", waiting: "等待运行" };
  const status = snapshot.paused ? "paused" : snapshot.status;
  const kind = ["pausing", "paused", "resuming"].includes(status) ? "warning" : status === "running" ? "success" : status === "degraded" ? "danger-soft" : "neutral";
  return `<span class="state-badge ${kind}">${labels[status] || esc(status || "未知")}</span>`;
}

function pairRow(pair) {
  const issues = pairIssues(pair);
  const markets = marketsForPair(pair.id);
  return `<button class="data-row pair-row" data-open-pair="${esc(pair.id)}">
    <span class="token-icon">${esc(pair.base.symbol?.slice(0, 1) || "?")}</span>
    <span class="row-primary"><strong>${esc(pair.base.symbol || "TOKEN")}/${esc(pair.quote.symbol || "QUOTE")}</strong><small>${esc(pair.id)}</small></span>
    <span class="market-tags">${markets.length ? markets.map((item) => `<i>${esc(item.name)}</i>`).join("") : "未绑定交易所"}</span>
    <span class="state-badge ${issues.length ? "warning" : "success"}">${issues.length ? `${issues.length} 项待完善` : "配置就绪"}</span>
    <span class="chevron">›</span>
  </button>`;
}

function pairsPage() {
  const pairs = (state.draft.instruments || []).filter((pair) => {
    const keyword = state.search.toLowerCase();
    return !keyword || `${pair.id} ${pair.base.symbol} ${pair.quote.symbol}`.toLowerCase().includes(keyword);
  });
  return `
    <section class="panel">
      <div class="toolbar">
        <div class="search"><span>⌕</span><input id="pair-search" value="${esc(state.search)}" placeholder="搜索币对、Token 或内部 ID"></div>
        ${has("instrument:edit") && has("config:edit") ? `<button class="btn primary" data-action="toggle-create-pair">＋ 新增币对</button>` : ""}
      </div>
      ${state.ui.createPair ? createPairForm() : ""}
      ${pairs.length ? `<div class="data-list roomy">${pairs.map(pairRow).join("")}</div>` : emptyState("没有匹配的币对", "调整搜索条件，或新增一个币对。", "新增币对", "toggle-create-pair")}
    </section>`;
}

function createPairForm() {
  return `<form class="inline-create" id="create-pair">
    <div class="form-title"><div><span class="eyebrow">NEW INSTRUMENT</span><h3>新增币对</h3></div><button class="icon-btn" type="button" data-action="toggle-create-pair">×</button></div>
    <div class="form-grid four">
      ${plainField("基础币符号", "base_symbol", "TOKEN", true)}
      ${plainField("报价币符号", "quote_symbol", "USDT", true, "", "USDT")}
      ${plainField("基础币 BSC 合约", "base_address", "0x…", true, "wide")}
      ${plainField("报价币 BSC 合约", "quote_address", "0x…", true, "wide")}
    </div>
    <div class="form-actions"><span>创建后继续配置 Pancake 路径、交易市场与策略。</span><button class="btn primary" type="submit">创建并进入详情</button></div>
  </form>`;
}

function pairPage() {
  const pair = selectedPair();
  if (!pair) return emptyState("币对不存在", "它可能已被删除或超出你的数据权限。", "返回币对列表", "back-pairs");
  const issues = pairIssues(pair);
  const tabs = [["basic", "基础信息"], ["oracle", "链上参考价"], ["markets", "交易市场"], ["strategy", "策略与风控"], ["simulation", "成交量仿真（内部）"]];
  if (has("runtime:view")) tabs.push(["runtime", "运行状态"]);
  return `
    <button class="back-link" data-action="back-pairs">← 返回币对列表</button>
    <section class="pair-summary">
      <div class="pair-identity"><span class="token-icon large">${esc(pair.base.symbol?.slice(0, 1))}</span><div><h2>${esc(pair.base.symbol)}/${esc(pair.quote.symbol)}</h2><p>${esc(pair.id)}</p></div></div>
      <div class="summary-facts"><span><small>配置状态</small><b class="state-text ${issues.length ? "warn" : "ok"}">${issues.length ? "待完善" : "就绪"}</b></span><span><small>价格来源</small><b>Pancake V2</b></span><span><small>交易市场</small><b>${marketsForPair(pair.id).length}</b></span></div>
    </section>
    ${issues.length ? `<div class="validation-card"><strong>完成以下项目后才能保存</strong><div>${issues.map((issue) => `<span>• ${esc(issue)}</span>`).join("")}</div></div>` : ""}
    <div class="tabs">${tabs.map(([id, label]) => `<button data-pair-tab="${id}" class="${state.pairTab === id ? "active" : ""}">${label}</button>`).join("")}</div>
    ${pairTabContent(pair)}
    ${has("instrument:edit") && has("config:edit") ? `<section class="danger-zone"><div><strong>删除币对</strong><p>只会删除草稿中的币对及其交易市场映射，保存后立即从运行引擎移除。</p></div><button class="btn danger" data-action="delete-pair">删除 ${esc(pair.id)}</button></section>` : ""}`;
}

function pairTabContent(pair) {
  const index = pairIndex(pair.id);
  if (state.pairTab === "runtime") return runtimeTab(pair);
  if (state.pairTab === "oracle") return oracleTab(pair, index);
  if (state.pairTab === "markets") return marketsTab(pair);
  if (state.pairTab === "strategy") return strategyTab(pair, index);
  if (state.pairTab === "simulation") return tradeSimulationTab(pair, index);
  return basicTab(pair, index);
}

function runtimeTab(pair) {
  const snapshot = runtimeSnapshot(pair.id) || { instrument_id: pair.id, base_symbol: pair.base.symbol, quote_symbol: pair.quote.symbol, status: "waiting", paused: false, venues: [] };
  const reference = snapshot.reference;
  const pausePending = snapshot.status === "pausing";
  const resumePending = snapshot.status === "resuming";
  const pauseRequested = snapshot.paused || pausePending;
  const controls = `<div class="runtime-actions">
    ${pauseRequested && has("runtime:start") ? `<button class="btn primary" data-runtime-action="resume" data-id="${esc(pair.id)}">开启币对</button>` : ""}
    ${!pauseRequested && !resumePending && has("runtime:stop") ? `<button class="btn" data-runtime-action="pause" data-id="${esc(pair.id)}">关闭币对</button>` : ""}
    ${has("runtime:emergency_cancel") ? `<button class="btn danger" data-runtime-action="emergency-cancel" data-id="${esc(pair.id)}">紧急暂停并撤单</button>` : ""}
    ${has("runtime:start") ? `<button class="btn" data-runtime-action="reconcile" data-id="${esc(pair.id)}">对账并解除阻断</button>` : ""}
  </div>`;
  return `
    <div class="runtime-detail-head"><div><span class="eyebrow">LIVE SNAPSHOT</span><h2>${esc(pair.base.symbol)}/${esc(pair.quote.symbol)} ${runtimeBadge(snapshot)}</h2><p>快照更新于 ${formatRelative(snapshot.updated_at)} · 总耗时 ${snapshot.tick_duration_ms || 0}ms · 链上 ${snapshot.reference_duration_ms || 0}ms · 余额 ${snapshot.balance_duration_ms || 0}ms</p></div>${controls}</div>
    ${snapshot.pause ? `<div class="pause-notice"><span>Ⅱ</span><div><strong>${snapshot.paused ? "币对已暂停" : "暂停撤单指令处理中"}</strong><p>原因：${esc(snapshot.pause.reason)} · 请求时间 ${formatDate(snapshot.pause.requested_at)}</p></div></div>` : resumePending ? `<div class="pause-notice"><span>↻</span><div><strong>正在恢复报价</strong><p>等待引擎加载最新配置并生成新的运行快照。</p></div></div>` : ""}
    ${snapshot.error ? `<div class="runtime-error prominent">${esc(snapshot.error)}</div>` : ""}
    <div class="metrics runtime-metrics">
      ${metric("Pancake 指数价", reference?.price || "—", reference ? `${reference.confidence} · 区块 ${reference.block_number}` : "等待链上价格")}
      ${metric("链上 Spot", reference?.spot || "—", reference?.twap_ready ? "TWAP 已就绪" : "TWAP 暖机中")}
      ${metric("账户总库存", snapshot.inventory_available ? snapshot.inventory : "—", `目标 ${snapshot.target_inventory ?? pair.strategy.target_base}`)}
      ${metric("最大库存偏差", snapshot.max_base_deviation ?? pair.strategy.max_base_deviation, snapshot.mode === "live" ? "实盘风控" : "Shadow 观察")}
    </div>
    ${(snapshot.venues || []).length ? `<div class="runtime-venue-stack">${snapshot.venues.map(runtimeVenuePanel).join("")}</div>` : emptyState("等待首个运行快照", "引擎成功加载最新配置后，会写入指数价、盘口和账户数据。", "刷新", "refresh-runtime")}`;
}

function runtimeVenuePanel(venue) {
  const book = venue.book;
  const budget = venue.budget;
  const rules = venue.rules;
  const bookState = bookDisplayState(book);
  const spread = bookState.twoSided ? spreadBps(book.bid_price, book.ask_price) : null;
  return `<section class="panel runtime-venue">
    <div class="market-head"><div><span class="venue-logo">${esc(venue.type?.slice(0, 1).toUpperCase())}</span><div><h3>${esc(venue.name)} · ${esc(venue.symbol)}</h3><p>行情 ${connectionLabel(venue.market_connected)} · 账户 ${connectionLabel(venue.account_connected)} · 盘口 ${venue.book_duration_ms || 0}ms · 订单 ${venue.orders_duration_ms || 0}ms · 成交 ${venue.fills_duration_ms || 0}ms · OMS ${venue.oms_duration_ms || 0}ms</p></div></div><span class="state-badge ${venue.market_connected ? "success" : "danger-soft"}">${venue.trading_enabled ? "允许实盘" : "Shadow"}</span></div>
    <div class="book-grid"><span><small>买一</small><b class="buy-text">${bookState.hasBid ? esc(book.bid_price) : "—"}</b><i>${bookState.hasBid ? esc(book.bid_qty) : "等待本系统铺单"}</i></span><span><small>卖一</small><b class="sell-text">${bookState.hasAsk ? esc(book.ask_price) : "—"}</b><i>${bookState.hasAsk ? esc(book.ask_qty) : "等待本系统铺单"}</i></span><span><small>盘口点差</small><b>${spread == null ? "—" : `${spread.toFixed(2)} bps`}</b><i>${esc(bookState.label)}</i></span><span><small>${esc(venue.base_balance?.asset || "Base")} 库存</small><b>${venue.base_balance ? decimalAdd(venue.base_balance.free, venue.base_balance.locked) : "—"}</b><i>${venue.base_balance ? `可用 ${venue.base_balance.free}` : "未连接账户"}</i></span><span><small>${esc(venue.quote_balance?.asset || "Quote")} 余额</small><b>${venue.quote_balance ? decimalAdd(venue.quote_balance.free, venue.quote_balance.locked) : "—"}</b><i>${venue.quote_balance ? `可用 ${venue.quote_balance.free}` : "未连接账户"}</i></span>${rules ? `<span><small>交易所实时精度</small><b>${esc(rules.price_tick)} / ${esc(rules.quantity_step)}</b><i>价格 Tick / 数量 Step</i></span><span><small>交易所挂单上限</small><b>${rules.max_open_orders || "未公布"}</b><i>运行时自动同步</i></span>` : ""}${budget ? `<span><small>卖盘资金预算</small><b>${esc(budget.base_budget)}</b><i>目标需要 ${esc(budget.base_required)}${budget.base_limited ? " · 已裁剪" : ""}</i></span><span><small>买盘资金预算</small><b>${esc(budget.quote_budget)}</b><i>目标需要 ${esc(budget.quote_required)}${budget.quote_limited ? " · 已裁剪" : ""}</i></span><span><small>可执行目标订单</small><b>${esc(budget.eligible_orders)} / ${esc(budget.target_orders)}</b><i>余额预留 ${esc(budget.reserve_bps)} bps</i></span>` : ""}</div>
    ${venue.fault && venue.fault.status !== "normal" ? `<div class="runtime-error">故障状态：${esc(venue.fault.status)} · ${esc(venue.fault.stage || "恢复检查")} · 连续失败 ${esc(venue.fault.consecutive_failures)} · 连续恢复 ${esc(venue.fault.consecutive_successes)}${venue.fault.orders_retained ? " · 原订单暂时保留" : ""}</div>` : ""}
    ${venue.error ? `<div class="runtime-error">${esc(venue.error)}</div>` : ""}
    <div class="runtime-tables">
      <div><div class="table-title"><h4>当前挂单</h4><span>${has("orders:view") ? `${(venue.open_orders || []).length} 笔${venue.pending_orders ? ` · ${venue.pending_orders} 笔确认中` : ""}` : "受权限限制"}</span></div>${has("orders:view") ? ordersTable(venue.open_orders || []) : `<div class="mini-empty">无查看挂单权限</div>`}</div>
      <div><div class="table-title"><h4>近期成交</h4><span>${has("fills:view") ? `${(venue.fills || []).length} 笔` : "受权限限制"}</span></div>${has("fills:view") ? fillsTable(venue.fills || []) : `<div class="mini-empty">无查看成交权限</div>`}</div>
    </div>
  </section>`;
}

function ordersTable(orders) {
  if (!orders.length) return `<div class="mini-empty">暂无挂单</div>`;
  const asks = orders.filter((order) => String(order.side).toUpperCase() === "SELL").sort((left, right) => compareDecimalPrices(right.price, left.price));
  const bids = orders.filter((order) => String(order.side).toUpperCase() === "BUY").sort((left, right) => compareDecimalPrices(right.price, left.price));
  const unclassified = orders.filter((order) => !["BUY", "SELL"].includes(String(order.side).toUpperCase()));
  const bestAsk = asks.at(-1)?.price;
  const bestBid = bids[0]?.price;
  const spread = spreadBps(bestBid, bestAsk);
  return `<div class="order-book">
    ${orderBookSide("SELL", asks)}
    <div class="order-book-mid"><span><small>最低卖价</small><b class="sell-text">${bestAsk == null ? "—" : esc(bestAsk)}</b></span><strong>${spread == null ? "买卖分界" : `${spread.toFixed(2)} bps`}</strong><span><small>最高买价</small><b class="buy-text">${bestBid == null ? "—" : esc(bestBid)}</b></span></div>
    ${orderBookSide("BUY", bids)}
    ${unclassified.length ? orderBookSide("OTHER", unclassified) : ""}
  </div>`;
}

function orderBookSide(side, orders) {
  const labels = side === "SELL" ? ["卖盘", "ASK"] : side === "BUY" ? ["买盘", "BID"] : ["其他订单", "OTHER"];
  const tone = side === "SELL" ? "asks" : side === "BUY" ? "bids" : "other";
  return `<section class="order-book-side ${tone}"><div class="order-book-side-head"><span>${labels[0]} <b>${labels[1]}</b></span><small>${orders.length} 笔</small></div>${orders.length ? `<div class="table-wrap"><table class="runtime-table order-book-table"><thead><tr><th>价格</th><th>委托数量</th><th>已成交</th><th>状态</th></tr></thead><tbody>${orders.map((order) => `<tr title="订单 ${esc(order.order_id || order.client_id || "—")}"><td class="${side === "SELL" ? "sell-text" : side === "BUY" ? "buy-text" : ""}">${esc(order.price)}</td><td>${esc(order.quantity)}</td><td>${esc(order.executed_qty)}</td><td>${esc(orderStateLabel(order.state))}</td></tr>`).join("")}</tbody></table></div>` : `<div class="order-book-empty">暂无${labels[0]}挂单</div>`}</section>`;
}

function compareDecimalPrices(left, right) {
  const leftParts = positiveDecimalParts(left);
  const rightParts = positiveDecimalParts(right);
  if (!leftParts || !rightParts) return Number(left || 0) - Number(right || 0);
  if (leftParts.integer.length !== rightParts.integer.length) return leftParts.integer.length < rightParts.integer.length ? -1 : 1;
  if (leftParts.integer !== rightParts.integer) return leftParts.integer < rightParts.integer ? -1 : 1;
  const scale = Math.max(leftParts.fraction.length, rightParts.fraction.length);
  const leftFraction = leftParts.fraction.padEnd(scale, "0");
  const rightFraction = rightParts.fraction.padEnd(scale, "0");
  if (leftFraction === rightFraction) return 0;
  return leftFraction < rightFraction ? -1 : 1;
}

function positiveDecimalParts(value) {
  const match = String(value ?? "").trim().match(/^\+?(\d+)(?:\.(\d*))?$/);
  if (!match) return null;
  return { integer: match[1].replace(/^0+(?=\d)/, ""), fraction: match[2] || "" };
}

function orderStateLabel(value) {
  return ({ NEW: "挂单中", PARTIALLY_FILLED: "部分成交", UNKNOWN: "确认中", FILLED: "已成交", CANCELED: "已撤销", REJECTED: "已拒绝", EXPIRED: "已过期" })[String(value || "").toUpperCase()] || value || "—";
}

function fillsTable(fills) {
  if (!fills.length) return `<div class="mini-empty">暂无近期成交</div>`;
  return `<div class="table-wrap"><table class="runtime-table"><thead><tr><th>时间</th><th>方向</th><th>价格</th><th>数量</th><th>类型</th></tr></thead><tbody>${fills.slice(0, 20).map((fill) => `<tr><td>${formatDate(fill.timestamp)}</td><td class="${fill.side === "BUY" ? "buy-text" : "sell-text"}">${esc(fill.side)}</td><td>${esc(fill.price)}</td><td>${esc(fill.quantity)}</td><td>${fill.aggregate ? "订单汇总" : fill.maker ? "Maker" : "Taker"}</td></tr>`).join("")}</tbody></table></div>`;
}

function simulatedFillsTable(fills) {
  if (!fills.length) return `<div class="mini-empty">等待第一条内部仿真事件</div>`;
  return `<div class="table-wrap"><table class="runtime-table"><thead><tr><th>时间</th><th>方向</th><th>价差内价格</th><th>仿真数量</th><th>标识</th></tr></thead><tbody>${fills.slice(0, 50).map((fill) => `<tr><td>${formatDate(fill.timestamp)}</td><td class="${fill.side === "BUY" ? "buy-text" : "sell-text"}">${esc(fill.side)}</td><td>${esc(fill.price)}</td><td>${esc(fill.quantity)}</td><td>SIMULATED</td></tr>`).join("")}</tbody></table></div>`;
}

function basicTab(pair, index) {
  const editable = has("instrument:edit") && has("config:edit");
  const path = `instruments.${index}`;
  return `<section class="panel form-panel">
    <div class="section-heading"><span>01</span><div><h2>币对与 Token</h2><p>内部 ID 创建后保持不变，避免交易所映射失效。</p></div></div>
    <div class="form-grid">
      ${boundField("内部 ID", `${path}.id`, pair.id, "text", false, true)}
      ${boundField("基础币符号", `${path}.base.symbol`, pair.base.symbol, "text", editable)}
      ${boundField("基础币 Decimals", `${path}.base.decimals`, pair.base.decimals, "number", editable)}
      ${boundField("基础币 BSC 合约", `${path}.base.address`, pair.base.address, "text", editable, false, "span-2")}
      ${boundField("报价币符号", `${path}.quote.symbol`, pair.quote.symbol, "text", editable)}
      ${boundField("报价币 Decimals", `${path}.quote.decimals`, pair.quote.decimals, "number", editable)}
      ${boundField("报价币 BSC 合约", `${path}.quote.address`, pair.quote.address, "text", editable, false, "span-2")}
    </div>
  </section>`;
}

function oracleTab(pair, index) {
  const editable = has("instrument:edit") && has("config:edit");
  const reference = pair.reference || {};
  const path = `instruments.${index}.reference`;
  return `
    <section class="panel form-panel">
      <div class="section-heading"><span>02</span><div><h2>参考价安全参数</h2><p>用 TWAP、现货偏离和区块时效过滤异常链上价格。</p></div></div>
      <div class="form-grid">
        ${boundField("TWAP 窗口（秒）", `${path}.twap_window_seconds`, reference.twap_window_seconds, "number", editable)}
        ${boundField("最大 Spot/TWAP 偏离（bps）", `${path}.max_spot_twap_deviation_bps`, reference.max_spot_twap_deviation_bps, "number", editable)}
        ${boundField("价格过期时间（秒）", `${path}.stale_after_seconds`, reference.stale_after_seconds, "number", editable)}
        ${boundCheck("仅 Shadow 暖机允许 Spot（Live 自动关闭）", `${path}.allow_spot_during_warmup`, reference.allow_spot_during_warmup, editable && state.draft.mode !== "live")}
      </div>
    </section>
    <section class="panel">
      <div class="panel-head"><div><h2>PancakeSwap V2 价格路径</h2><p>TOKEN/USDT 通常一跳；TOKEN/WBNB/USDT 使用两跳。</p></div>${editable ? `<button class="btn" data-action="add-leg">＋ 增加一跳</button>` : ""}</div>
      ${(reference.legs || []).length ? `<div class="route-flow">${reference.legs.map((leg, legIndex) => oracleLeg(leg, index, legIndex, editable)).join("")}</div>` : emptyState("还没有价格路径", "至少添加一个 PancakeSwap V2 Pair 地址。", "增加第一跳", "add-leg")}
    </section>`;
}

function oracleLeg(leg, pairIndexValue, legIndex, editable) {
  const path = `instruments.${pairIndexValue}.reference.legs.${legIndex}`;
  const inspection = state.ui.pairInspection[`${state.selectedPairId}:${legIndex}`];
  return `<article class="route-card">
    <div class="route-number">${legIndex + 1}</div>
    <div class="route-body"><div class="row-head"><div><strong>第 ${legIndex + 1} 跳</strong><small>${esc(shortAddress(leg.pair_address))}</small></div>${editable ? `<button class="text-danger" data-action="remove-leg" data-leg="${legIndex}">移除</button>` : ""}</div>
      <div class="form-grid">
        ${oraclePairField(`${path}.pair_address`, leg.pair_address, legIndex, editable, inspection)}
        ${boundField("Factory 地址", `${path}.expected_factory`, leg.expected_factory, "text", editable, false, "span-2")}
        ${boundField("Base Token（自动识别）", `${path}.base_token`, leg.base_token, "text", false, false, "span-2")}
        ${boundField("Quote Token（自动识别）", `${path}.quote_token`, leg.quote_token, "text", false, false, "span-2")}
        ${boundField("最低 Quote 储备", `${path}.min_quote_reserve`, leg.min_quote_reserve, "text", editable)}
        ${boundField("最大空闲（秒）", `${path}.max_idle_seconds`, leg.max_idle_seconds, "number", editable)}
      </div>
    </div>
  </article>`;
}

function oraclePairField(path, value, legIndex, editable, inspection) {
  const helper = inspection?.error
    ? `<small class="inspect-error">${esc(inspection.error)}</small>`
    : inspection?.loading
      ? `<small class="inspect-loading">正在从 BNB Chain 读取 Pair…</small>`
      : inspection?.factory
        ? `<small class="inspect-success">已验证 Factory ${esc(shortAddress(inspection.factory))} · token0/token1 已识别</small>`
        : `<small>填写 Pair 后自动读取 token0/token1；也可以手动点击识别。</small>`;
  return `<div class="field span-2"><label>Pair 地址</label><div class="pair-inspect-line"><input data-path="${esc(path)}" data-type="text" data-inspect-leg="${legIndex}" type="text" value="${esc(value)}" ${editable ? "" : "disabled"}><button class="btn small" type="button" data-action="inspect-pair" data-leg="${legIndex}" ${editable ? "" : "disabled"}>识别</button></div>${helper}</div>`;
}

function marketsTab(pair) {
  const mappings = marketsForPair(pair.id);
  const editable = has("venue:edit") && has("config:edit");
  return `<section class="panel">
    <div class="panel-head"><div><h2>交易市场</h2><p>先选择交易所，再为这个币对绑定对应交易账号凭证。</p></div>${editable ? `<button class="btn primary" data-action="toggle-add-market">＋ 新增市场</button>` : ""}</div>
    ${state.ui.addMarket ? addMarketForm(pair) : ""}
    ${mappings.length ? `<div class="market-stack">${mappings.map((mapping) => marketCard(pair, mapping, editable)).join("")}</div>` : emptyState("尚未绑定交易市场", "至少配置一家交易所；关闭某家交易所不会影响其他市场。", "新增交易市场", "toggle-add-market")}
  </section>`;
}

function addMarketForm(pair) {
  const available = Object.entries(state.draft.venues || {}).filter(([name, venue]) => venue.enabled && !venue.markets?.[pair.id]);
  if (!available.length) return `<div class="inline-note warning">没有可添加的交易所。请先启用交易所，或当前币对已经绑定了全部交易所。</div>`;
  return `<form class="inline-create" id="add-market">
    <div class="form-title"><div><span class="eyebrow">NEW MARKET</span><h3>绑定交易市场</h3></div><button class="icon-btn" type="button" data-action="toggle-add-market">×</button></div>
    <div class="form-grid four">
      <div class="field"><label>交易所</label><select name="venue_name" id="market-venue" required>${available.map(([name, venue]) => `<option value="${esc(name)}">${esc(name)} · ${esc(venue.type)}</option>`).join("")}</select></div>
      ${plainField("交易所 Symbol", "symbol", `${pair.base.symbol}${pair.quote.symbol}`, true, "", `${pair.base.symbol}${pair.quote.symbol}`)}
      ${plainField("价格精度", "price_tick", "0.000001", true, "", "0.000001")}
      ${plainField("数量精度", "quantity_step", "0.01", true, "", "0.01")}
      ${plainField("最小下单金额", "min_notional", "5", true, "", "5")}
      <div class="field span-2"><label>交易账号凭证</label><select name="credential_id" id="market-credential"><option value="0">Shadow 暂不绑定</option>${credentialOptionHTML(available[0][1].type)}</select></div>
    </div>
    <div class="form-actions"><span>实盘保存前必须绑定启用中的同类型凭证。</span><button class="btn primary" type="submit">添加市场</button></div>
  </form>`;
}

function marketCard(pair, mapping, editable) {
  const path = `venues.${mapping.name}.markets.${pair.id}`;
  const market = mapping.market;
  const credential = state.credentialOptions.find((item) => item.id === Number(market.credential_id));
  return `<article class="market-card">
    <div class="market-head"><div><span class="venue-logo">${esc(mapping.venue.type.slice(0, 1).toUpperCase())}</span><div><h3>${esc(mapping.name)}</h3><p>${esc(mapping.venue.type)} · ${esc(market.symbol || "未填写 Symbol")}</p></div></div><div class="market-status"><span class="state-badge ${mapping.venue.trading_enabled ? "danger-soft" : "neutral"}">${mapping.venue.trading_enabled ? "允许实盘" : "Shadow"}</span>${editable ? `<button class="text-danger" data-remove-market="${esc(mapping.name)}">移除</button>` : ""}</div></div>
    <div class="form-grid compact">
      ${boundField("交易所 Symbol", `${path}.symbol`, market.symbol, "text", editable)}
      ${boundField("基础币代码", `${path}.base_asset`, market.base_asset, "text", editable)}
      ${boundField("报价币代码", `${path}.quote_asset`, market.quote_asset, "text", editable)}
      ${boundField("价格 Tick", `${path}.price_tick`, market.price_tick, "text", editable)}
      ${boundField("数量 Step", `${path}.quantity_step`, market.quantity_step, "text", editable)}
      ${boundField("最小金额", `${path}.min_notional`, market.min_notional, "text", editable)}
      ${boundField("最大 Base 占用（0=不限）", `${path}.max_base_commitment`, market.max_base_commitment ?? "0", "text", editable)}
      ${boundField("最大 Quote 占用（0=不限）", `${path}.max_quote_commitment`, market.max_quote_commitment ?? "0", "text", editable)}
      <div class="field span-2"><label>交易账号凭证</label><select data-path="${path}.credential_id" data-type="number" ${editable ? "" : "disabled"}><option value="0">Shadow 暂不绑定</option>${credentialOptionHTML(mapping.venue.type, market.credential_id)}</select><small>${credential ? `${esc(credential.name)} · ${credential.enabled ? "可用" : "已停用"}` : "实盘前必须绑定"}</small></div>
    </div>
    <p class="muted-line">价格 Tick、数量 Step、最小金额和挂单上限会在保存时从交易所重新读取；后台填写值作为保存前基础校验及交易所未提供字段的兜底。</p>
  </article>`;
}

function strategyTab(pair, index) {
  const editable = has("strategy:edit") && has("config:edit");
  const strategy = pair.strategy || {};
  const path = `instruments.${index}.strategy`;
  const quoteSymbol = pair.quote?.symbol || "报价币";
  const usesNotionalRange = Number(strategy.min_order_notional ?? 0) !== 0 || Number(strategy.max_order_notional ?? 0) !== 0;
  const legacyOrderSize = !usesNotionalRange && Number(strategy.order_size) > 0;
  const levels = Math.max(0, Number(strategy.levels) || 0);
  const enabledMarketCount = marketsForPair(pair.id).filter((item) => item.venue.enabled).length;
  const ordersPerMarket = levels * 2;
  const totalTargetOrders = ordersPerMarket * enabledMarketCount;
  const outermostSpread = Number(strategy.half_spread_bps || 0) + Math.max(0, levels - 1) * Number(strategy.level_spacing_bps || 0);
  return `<div class="content-grid">
    <section class="panel form-panel span-2">
      <div class="section-heading"><span>03</span><div><h2>报价梯度</h2><p>控制初始点差、档位距离、每档金额和重新报价频率。</p></div></div>
      <div class="form-grid">
        ${boundField("初始半点差（bps）", `${path}.half_spread_bps`, strategy.half_spread_bps, "number", editable)}
        ${boundField("档位间距（bps）", `${path}.level_spacing_bps`, strategy.level_spacing_bps, "number", editable)}
        ${boundedNumberField("档数（1–100）", `${path}.levels`, strategy.levels, editable, 1, 100, "每个交易市场的买卖两侧各铺这么多档")}
        ${boundField(`每档最小金额（${quoteSymbol}）`, `${path}.min_order_notional`, usesNotionalRange ? strategy.min_order_notional : "", "text", editable)}
        ${boundField(`每档最大金额（${quoteSymbol}）`, `${path}.max_order_notional`, usesNotionalRange ? strategy.max_order_notional : "", "text", editable)}
        ${boundField("重新报价阈值（bps）", `${path}.reprice_threshold_bps`, strategy.reprice_threshold_bps, "number", editable)}
        ${boundedNumberField("余额预留（bps）", `${path}.balance_reserve_bps`, strategy.balance_reserve_bps ?? 0, editable, 0, 10000, "500 表示保留 5% 可控资金，不用于铺单")}
        ${boundedNumberField("交易所/指数最大偏差（bps）", `${path}.max_venue_reference_deviation_bps`, strategy.max_venue_reference_deviation_bps ?? 0, editable, 0, 10000, "0 使用运行时安全默认值 500 bps")}
        ${boundedNumberField("交易所盘口最大点差（bps）", `${path}.max_venue_spread_bps`, strategy.max_venue_spread_bps ?? 0, editable, 0, 10000, "0 使用运行时安全默认值 1000 bps")}
      </div>
      ${legacyOrderSize ? `<div class="inline-note warning">当前配置仍使用旧版固定数量：每档 ${esc(strategy.order_size)} ${esc(pair.base?.symbol || "Base Token")}。填写上面的最小和最大金额后，才会切换为按 ${esc(quoteSymbol)} 金额自动换算数量。</div>` : `<div class="inline-note">每个买卖档位会在金额范围内生成稳定随机值，再按该档价格和交易所数量精度自动换算，不需要手工计算 Token 数量。交易所最小下单金额高于这里的最小值时，以交易所规则为准。</div>`}
      <div class="section-heading compact-heading"><span>↻</span><div><h2>盘口渐进轮换</h2><p>指数价格不变时，只轮换少量到期档位；真实行情移动仍按重新报价阈值优先处理。</p></div></div>
      <div class="form-grid">
        ${boundedNumberField("轮换间隔（秒）", `${path}.quote_refresh_seconds`, strategy.quote_refresh_seconds ?? 45, editable, 10, 86400, "建议 45 秒；同一时间窗口内目标保持稳定")}
        ${boundedNumberField("每轮轮换比例（bps）", `${path}.quote_refresh_ratio_bps`, strategy.quote_refresh_ratio_bps ?? 1000, editable, 1, 10000, "1000 表示每轮只处理约 10% 当前目标订单")}
        ${boundedNumberField("最短挂单存活（秒）", `${path}.min_order_lifetime_seconds`, strategy.min_order_lifetime_seconds ?? 30, editable, 5, 86400, "新挂订单在此时间内不会因常规轮换再次撤掉")}
        ${boundedNumberField("最长挂单存活（秒）", `${path}.max_order_lifetime_seconds`, strategy.max_order_lifetime_seconds ?? 300, editable, 10, 604800, "超过后按最老优先、分批轮换")}
        ${boundedNumberField("价格扰动（Tick）", `${path}.price_jitter_ticks`, strategy.price_jitter_ticks ?? 2, editable, 1, 100, "只在原策略价格附近变化，并继续受 Post-Only 与价格偏差保护")}
        ${boundedNumberField("最优档数量", `${path}.best_levels`, strategy.best_levels ?? Math.min(3, levels), editable, 1, Math.max(1, levels), "买卖两侧最靠近盘口的档位数量")}
        ${boundedNumberField("最优档轮换间隔（秒）", `${path}.best_level_refresh_seconds`, strategy.best_level_refresh_seconds ?? 90, editable, 10, 86400, "不能短于普通轮换间隔，避免频繁丢失最优价排队位置")}
      </div>
      <div class="inline-note">系统只让到期档位进入撤挂队列，并优先保留其他深度。数量会在 ${esc(strategy.min_order_notional ?? 10)}～${esc(strategy.max_order_notional ?? 20)} ${esc(quoteSymbol)} 内重新生成，价格最多扰动 ${esc(strategy.price_jitter_ticks ?? 2)} 个 Tick；不会因为轮换一次撤空全部盘口。</div>
    </section>
    <aside class="panel guidance"><span class="guide-icon">↕</span><h3>报价说明</h3><p>半点差决定第一档距离指数价的位置；后续档位按档位间距向外展开。</p><code>买一 = 指数价 − 半点差</code><code>卖一 = 指数价 ＋ 半点差</code><code>数量 = 随机${esc(quoteSymbol)}金额 ÷ 该档价格</code><code>最外档距离 = ${esc(outermostSpread)} bps</code><p>同一档位的随机目标金额保持稳定，避免行情轮询导致无意义撤挂。每个市场目标 ${ordersPerMarket} 张订单；当前 ${enabledMarketCount} 个已启用市场合计 ${totalTargetOrders} 张。订单按每轮最多 20 张滚动撤挂，避免限频和整片盘口瞬间消失。</p></aside>
    <section class="panel form-panel span-2">
      <div class="section-heading"><span>04</span><div><h2>库存风控</h2><p>库存偏离目标时，系统只调整报价倾斜，不做价格操纵。</p></div></div>
      <div class="form-grid">
        ${boundField("目标总库存", `${path}.target_base`, strategy.target_base, "text", editable)}
        ${boundField("最大库存偏差", `${path}.max_base_deviation`, strategy.max_base_deviation, "text", editable)}
        ${boundField("库存倾斜（bps）", `${path}.inventory_skew_bps`, strategy.inventory_skew_bps, "number", editable)}
      </div>
    </section>
  </div>`;
}

function tradeSimulationTab(pair, index) {
  const editable = has("strategy:edit") && has("config:edit");
  const cfg = pair.trade_simulation || {};
  const path = `instruments.${index}.trade_simulation`;
  const venueNames = marketsForPair(pair.id).filter((item) => item.venue.enabled).map((item) => item.name);
  const source = cfg.source_venue || venueNames[0] || "";
  const snapshot = runtimeSnapshot(pair.id)?.trade_simulation;
  return `<div class="content-grid">
    <section class="panel form-panel span-2">
      <div class="section-heading"><span>05</span><div><h2>成交量仿真与压测</h2><p>只读取所选交易所的公开买一卖一与精度，生成可供页面、Redis 和下游程序消费的内部事件。</p></div></div>
      <div class="inline-note warning">所有结果只用于内部仿真/压测，带 <code>simulated=true</code> 标记并写入独立 Redis Stream。不会调用 <code>PlaceOrder</code>、<code>CancelOrder</code> 或批量下单接口，也不会写入交易所成交量。</div>
      <div class="form-grid">
        ${boundCheck("启用内部成交量仿真", `${path}.enabled`, Boolean(cfg.enabled), editable)}
        <div class="field"><label>市场数据来源</label><select data-path="${path}.source_venue" data-type="text" ${editable ? "" : "disabled"}>${venueNames.length ? venueNames.map((name) => `<option value="${esc(name)}" ${name === source ? "selected" : ""}>${esc(name)}</option>`).join("") : `<option value="">请先绑定交易市场</option>`}</select><small>只读取公开盘口和精度，不传入账户凭证或下单客户端。</small></div>
        ${boundField("单事件最小数量", `${path}.min_quantity`, cfg.min_quantity ?? "1", "text", editable)}
        ${boundField("单事件最大数量", `${path}.max_quantity`, cfg.max_quantity ?? "10", "text", editable)}
        ${boundedNumberField("事件最短间隔（ms）", `${path}.min_interval_ms`, cfg.min_interval_ms ?? 1000, editable, 100, 3600000, "至少 100ms")}
        ${boundedNumberField("事件最长间隔（ms）", `${path}.max_interval_ms`, cfg.max_interval_ms ?? 3000, editable, 100, 3600000, "每条事件后重新随机")}
        ${boundedNumberField("买方向概率（bps）", `${path}.buy_probability_bps`, cfg.buy_probability_bps ?? 5000, editable, 0, 10000, "5000 表示买卖方向各 50%")}
        ${boundedNumberField("页面保留事件数", `${path}.recent_limit`, cfg.recent_limit ?? 50, editable, 1, 200, "Redis Stream 独立保留最近约 1000 条")}
      </div>
    </section>
    <aside class="panel guidance"><span class="guide-icon">SIM</span><h3>算法扩展点</h3><p>Java 规划器只接收配置、时间和只读市场快照，输出方向、价格、数量三个字段。框架统一补齐 SIM ID 与内部标记。</p><code>bid &lt; 仿真价 &lt; ask</code><code>simulated = true</code><p>默认算法按 Tick/Step 在价差内随机生成；你可以只替换规划器实现自己的压测逻辑。</p></aside>
    <section class="panel span-2">
      <div class="panel-head"><div><h2>近期内部仿真事件</h2><p>${snapshot?.enabled ? `状态 ${esc(snapshot.status || "waiting")} · 数据源 ${esc(snapshot.source_venue || source)} · 规划器 ${esc(snapshot.planner || "InsideSpreadRandomPlanner")}` : "尚未启用"}</p></div></div>
      ${snapshot?.error ? `<div class="runtime-error">${esc(snapshot.error)}</div>` : ""}
      ${simulatedFillsTable(snapshot?.fills || [])}
    </section>
  </div>`;
}

function venuesPage() {
  const entries = Object.entries(state.draft.venues || {});
  const editable = has("venue:edit") && has("config:edit") && state.user.all_instruments;
  return `<section class="panel">
    <div class="panel-head"><div><h2>交易所连接</h2><p>平台配置与交易账号分开；一个交易所可以有多个凭证。</p></div>${editable ? `<button class="btn primary" data-action="toggle-create-venue">＋ 新增交易所</button>` : ""}</div>
    ${state.ui.createVenue ? createVenueForm() : ""}
    ${entries.length ? `<div class="venue-grid">${entries.map(([name, venue]) => venueCard(name, venue, editable)).join("")}</div>` : emptyState("还没有交易所", "第一版建议先添加 Binance 和 MGBX。", "新增交易所", "toggle-create-venue")}
  </section>`;
}

function createVenueForm() {
  const specs = state.venueTypes.length ? state.venueTypes : [{ type: "binance", name: "Binance", production_base_url: "https://api.binance.com", testnet_base_url: "https://testnet.binance.vision", default_self_trade_prevention: "EXPIRE_BOTH" }];
  const first = specs[0];
  return `<form class="inline-create" id="create-venue">
    <div class="form-title"><div><span class="eyebrow">NEW VENUE</span><h3>新增交易所</h3></div><button class="icon-btn" type="button" data-action="toggle-create-venue">×</button></div>
    <div class="form-grid four">
      ${plainField("内部名称", "name", first.type, true, "", first.type)}
      <div class="field"><label>交易所类型</label><select name="type" id="new-venue-type">${specs.map((spec) => `<option value="${esc(spec.type)}">${esc(spec.name)}</option>`).join("")}</select></div>
      <div class="field"><label>运行环境</label><select name="environment"><option value="production">生产环境</option><option value="testnet">测试网</option></select></div>
      ${plainField("REST API 地址", "base_url", first.production_base_url, true, "span-2", first.production_base_url)}
      ${plainField("STP 模式", "self_trade_prevention", first.default_self_trade_prevention || "", false, "", first.default_self_trade_prevention || "")}
    </div>
    <div class="form-actions"><span>交易所创建后，再到“交易所凭证”添加账号。</span><button class="btn primary" type="submit">保存交易所</button></div>
  </form>`;
}

function venueCard(name, venue, editable) {
  const path = `venues.${name}`;
  const markets = Object.keys(venue.markets || {}).length;
  const accounts = state.credentialOptions.filter((item) => item.venue_type === venue.type).length;
  return `<article class="venue-card">
    <div class="venue-card-head"><span class="venue-logo large">${esc(venue.type.slice(0, 1).toUpperCase())}</span><div><h3>${esc(name)}</h3><p>${esc(venue.type)}</p></div><span class="state-badge ${venue.enabled ? "success" : "neutral"}">${venue.enabled ? "已启用" : "已停用"}</span></div>
    <div class="venue-facts"><span><b>${markets}</b><small>币对映射</small></span><span><b>${accounts}</b><small>账号凭证</small></span><span><b>${venue.trading_enabled ? "实盘" : "Shadow"}</b><small>运行权限</small></span></div>
    <div class="form-grid compact">
      ${boundField("API 地址", `${path}.base_url`, venue.base_url, "text", editable, false, "span-2")}
      <div class="field"><label>运行环境</label><select data-path="${path}.environment" data-type="text" ${editable ? "" : "disabled"}><option value="production" ${(venue.environment || "production") === "production" ? "selected" : ""}>生产环境</option><option value="testnet" ${venue.environment === "testnet" ? "selected" : ""}>测试网</option></select><small>${esc(venueSpec(venue.type)?.testnet_base_url ? "切换测试网会自动使用适配器登记地址。" : "没有登记固定测试网地址时保留手工 API 地址。")}</small></div>
      ${boundField("STP 模式", `${path}.self_trade_prevention`, venue.self_trade_prevention || "", "text", editable)}
      ${boundCheck("启用交易所", `${path}.enabled`, venue.enabled, editable)}
      ${boundCheck("允许实盘交易", `${path}.trading_enabled`, venue.trading_enabled, editable)}
      ${boundCheck("专用账户", `${path}.dedicated_account`, venue.dedicated_account, editable)}
    </div>
    ${editable ? `<div class="card-footer"><span>删除前需要先解除全部币对映射。</span><button class="text-danger" data-remove-venue="${esc(name)}">删除交易所</button></div>` : ""}
  </article>`;
}

function credentialsPage() {
  const editing = state.credentials.find((item) => item.id === state.ui.editCredentialId);
  return `<section class="panel">
    <div class="panel-head"><div><h2>交易账号</h2><p>密钥使用 AES-256-GCM 加密，配置快照和 Redis 只保存凭证 ID。</p></div><button class="btn primary" data-action="toggle-create-credential">＋ 新增凭证</button></div>
    ${state.ui.createCredential ? createCredentialForm() : ""}
    ${editing ? updateCredentialForm(editing) : ""}
    ${state.credentials.length ? `<div class="credential-grid">${state.credentials.map(credentialCard).join("")}</div>` : emptyState("还没有交易所凭证", "先维护交易所，再新增对应的交易账号。", "新增凭证", "toggle-create-credential")}
  </section>`;
}

function createCredentialForm() {
  const types = [...new Set(Object.values(state.draft.venues || {}).map((venue) => venue.type))];
  return `<form class="inline-create" id="create-credential">
    <div class="form-title"><div><span class="eyebrow">NEW ACCOUNT</span><h3>新增交易所凭证</h3></div><button class="icon-btn" type="button" data-action="toggle-create-credential">×</button></div>
    <div class="form-grid four">
      ${plainField("凭证名称", "name", "TOKEN-Binance-主账号", true)}
      <div class="field"><label>所属交易所</label><select name="venue_type" required>${types.length ? types.map((type) => `<option value="${esc(type)}">${esc(type)}</option>`).join("") : state.venueTypes.map((spec) => `<option value="${esc(spec.type)}">${esc(spec.name)}</option>`).join("")}</select></div>
      ${plainPasswordField("API Key", "api_key")}
      ${plainPasswordField("API Secret", "api_secret")}
    </div>
    <div class="security-note"><span>◉</span><p><b>提交后不会再次显示明文</b><br>请确认关闭提现权限，并限制服务器出口 IP。</p></div>
    <div class="form-actions"><span></span><button class="btn primary" type="submit">加密保存</button></div>
  </form>`;
}

function updateCredentialForm(item) {
  return `<form class="inline-create" id="update-credential" data-id="${item.id}">
    <div class="form-title"><div><span class="eyebrow">ROTATE SECRET</span><h3>轮换 ${esc(item.name)}</h3></div><button class="icon-btn" type="button" data-action="cancel-credential-edit">×</button></div>
    <div class="form-grid">
      ${plainField("凭证名称", "name", item.name, true, "", item.name)}
      ${plainPasswordField("新 API Key（留空不修改）", "api_key", false)}
      ${plainPasswordField("新 API Secret（留空不修改）", "api_secret", false)}
    </div>
    <div class="form-actions"><span>只填写需要轮换的字段。</span><button class="btn primary" type="submit">保存更新</button></div>
  </form>`;
}

function credentialCard(item) {
  const bindings = credentialBindings(item.id);
  return `<article class="credential-card">
    <div class="credential-head"><span class="venue-logo">${esc(item.venue_type.slice(0, 1).toUpperCase())}</span><div><h3>${esc(item.name)}</h3><p>${esc(item.venue_type)} · Key ****${esc(item.api_key_last4)}</p></div><span class="state-badge ${item.enabled ? "success" : "neutral"}">${item.enabled ? "启用" : "停用"}</span></div>
    <div class="credential-meta"><span>指纹 <code>${esc(item.fingerprint)}</code></span><span>更新于 ${formatDate(item.updated_at)}</span></div>
    <div class="binding-line"><span>绑定币对</span><div>${bindings.length ? bindings.map((id) => `<i>${esc(id)}</i>`).join("") : "尚未绑定"}</div></div>
    <div class="card-actions"><button class="btn small" data-edit-credential="${item.id}">轮换密钥</button><button class="btn small ${item.enabled ? "danger" : ""}" data-toggle-credential="${item.id}">${item.enabled ? "停用" : "启用"}</button></div>
  </article>`;
}

function settingsPage() {
  const editable = has("config:edit") && state.user.all_instruments;
  const config = state.draft;
  return `<div class="content-grid">
    <section class="config-flow-note span-3"><span class="guide-icon">↻</span><div><strong>改完点“保存”即刻生效</strong><p>保存会先校验再直接应用到正在运行的引擎：策略参数热更新、不重启；交易所/凭证等结构性改动只重建受影响的部分，其他币对照常报价。</p></div><div class="flow-steps"><span><b>1</b>编辑</span><i>→</i><span><b>2</b>保存</span><i>→</i><span><b>3</b>生效</span></div></section>
    <section class="panel form-panel span-2">
      <div class="section-heading"><span>01</span><div><h2>运行安全开关</h2><p>Shadow 不发送订单；实盘还需要服务器环境变量二次确认。</p></div></div>
      <div class="mode-selector">
        <label class="mode-card ${config.mode === "shadow" ? "selected" : ""}"><input type="radio" name="mode" data-path="mode" value="shadow" ${config.mode === "shadow" ? "checked" : ""} ${editable ? "" : "disabled"}><span>Shadow</span><small>只计算和记录报价计划</small></label>
        <label class="mode-card danger-mode ${config.mode === "live" ? "selected" : ""}"><input type="radio" name="mode" data-path="mode" value="live" ${config.mode === "live" ? "checked" : ""} ${editable ? "" : "disabled"}><span>Live</span><small>允许已启用市场发送订单</small></label>
      </div>
      <div class="form-grid">
        ${boundField("轮询间隔（ms）", "poll_interval_ms", config.poll_interval_ms, "number", editable)}
		${boundedNumberField("交易规则刷新（秒）", "rules_refresh_seconds", config.rules_refresh_seconds || 300, editable, 30, 86400, "定时同步价格精度、数量精度、最小金额与挂单限制；变化时告警")}
        ${boundedNumberField("币对最大并发数", "max_concurrent_instruments", config.max_concurrent_instruments || 4, editable, 1, 32, "不同交易账户可并行；共享同一凭证的币对仍自动串行")}
        ${boundField("Watchdog 超时（秒）", "watchdog_timeout_seconds", config.watchdog_timeout_seconds, "number", editable)}
        ${boundedNumberField("连续失败撤单阈值", "market_failure_threshold", config.market_failure_threshold ?? 3, editable, 1, 20, "达到次数后只撤对应币对和交易所")}
        ${boundedNumberField("连续恢复成功阈值", "market_recovery_threshold", config.market_recovery_threshold ?? 3, editable, 1, 20, "连续成功后才恢复铺单")}
        ${boundedNumberField("故障安全宽限（秒）", "market_error_grace_seconds", config.market_error_grace_seconds ?? 15, editable, 1, 300, "价格或行情超过宽限时间即撤单")}
        ${boundedNumberField("交易循环卡死超时（秒）", "trading_progress_timeout_seconds", config.trading_progress_timeout_seconds ?? 120, editable, 30, 3600, "进程在线但长期无交易进度时由 Watchdog 撤单")}
        ${boundField("审计文件", "audit_path", config.audit_path, "text", editable, false, "span-2")}
        ${boundMiBField("审计单文件上限（MiB）", "audit_max_bytes", config.audit_max_bytes || 104857600, editable, 1, 10240, "默认 100MiB，达到上限后轮转")}
        ${boundedNumberField("审计备份份数", "audit_backups", config.audit_backups || 7, editable, 1, 30, "保留 events.jsonl.1 到指定份数")}
        ${boundField("心跳文件", "heartbeat_path", config.heartbeat_path, "text", editable, false, "span-2")}
      </div>
    </section>
    <aside class="panel guidance"><span class="guide-icon">!</span><h3>实盘双重开关</h3><p>后台选择 Live 仍不会自动开启交易。服务器还必须设置：</p><code>FLUXMAKER_ENABLE_LIVE_TRADING=I_UNDERSTAND</code></aside>
    <section class="panel form-panel span-2">
      <div class="section-heading"><span>02</span><div><h2>BNB Chain RPC</h2><p>建议至少配置两个独立服务商节点，按顺序自动回退。</p></div></div>
      <div class="form-grid">
        ${boundField("Chain ID", "rpc.chain_id", config.rpc.chain_id, "number", editable)}
        ${boundField("请求超时（ms）", "rpc.request_timeout_ms", config.rpc.request_timeout_ms, "number", editable)}
        ${boundTextarea("RPC 地址（每行一个）", "rpc.urls", (config.rpc.urls || []).join("\n"), editable, "span-4")}
      </div>
    </section>
  </div>`;
}

function usersPage() {
  return `<section class="panel">
    <div class="panel-head"><div><h2>后台账户</h2><p>停用代替删除以保留审计记录；角色、密码或状态变更会立即使旧会话失效。</p></div><button class="btn" data-action="reload-users">刷新</button></div>
    ${state.users.length ? `<div class="user-list">${state.users.map(userCard).join("")}</div>` : `<div class="loading-state">正在加载账户…</div>`}
    <form class="inline-create always" id="create-user">
      <div class="form-title"><div><span class="eyebrow">NEW USER</span><h3>新增账户</h3></div></div>
      <div class="form-grid"><div class="field"><label>邮箱</label><input name="email" type="email" required></div><div class="field"><label>初始密码</label><input name="password" type="password" minlength="12" required autocomplete="new-password"></div><div class="field span-2"><label>分配角色</label><div class="choice-grid">${state.userRoleOptions.map((role) => checkboxChoice("roles", role.code, role.name, role.code)).join("") || "请先加载角色"}</div></div></div>
      <div class="form-actions"><span>密码至少 12 位；账户创建、修改和强制下线均写入审计日志。</span><button class="btn primary" type="submit">创建账户</button></div>
    </form>
  </section>`;
}

function userCard(user) {
  const editing = state.ui.editUserId === user.id;
  const current = Number(state.user?.user_id) === Number(user.id);
  return `<article class="user-card">
    <div class="user-row"><span class="avatar">${esc(user.email.slice(0, 1).toUpperCase())}</span><div><strong>${esc(user.email)}${current ? " · 当前账户" : ""}</strong><small>${esc((user.roles || []).join(" · ") || "未分配角色")} · 最近登录 ${esc(formatDateTime(user.last_login_at))}</small></div><span class="state-badge ${user.enabled ? "success" : "neutral"}">${user.enabled ? "启用" : "停用"}</span><div class="user-actions"><button class="btn small" data-edit-user="${user.id}">${editing ? "收起" : "管理"}</button>${current ? "" : `<button class="btn small ${user.enabled ? "danger" : ""}" data-toggle-user="${user.id}">${user.enabled ? "停用" : "启用"}</button>`}<button class="btn small" data-revoke-user="${user.id}">强制下线</button></div></div>
    ${editing ? userEditor(user, current) : ""}
  </article>`;
}

function userEditor(user, current) {
  return `<div class="user-editor">
    <form data-update-user="${user.id}">
      <div class="form-grid"><div class="field span-2"><label>登录邮箱</label><input name="email" type="email" value="${esc(user.email)}" required></div><label class="switch-line span-2"><input name="enabled" type="checkbox" ${user.enabled ? "checked" : ""} ${current ? "disabled" : ""}><span><b>允许登录</b><small>${current ? "不能在当前会话中停用自己" : "停用后立即拒绝现有会话和新登录"}</small></span></label><div class="field span-4"><label>角色</label><div class="choice-grid">${state.userRoleOptions.map((role) => checkboxChoice("roles", role.code, role.name, role.code, user.roles?.includes(role.code))).join("")}</div></div></div>
      <div class="form-actions"><span>保存后该账户必须重新登录。</span><button class="btn primary small" type="submit">保存账户</button></div>
    </form>
    <form class="password-reset" data-reset-user-password="${user.id}"><div class="field"><label>重置密码</label><input name="password" type="password" minlength="12" required autocomplete="new-password" placeholder="输入至少 12 位新密码"></div><button class="btn small" type="submit">重置并下线</button></form>
  </div>`;
}

function formatDateTime(value) {
  if (!value) return "从未";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "未知" : date.toLocaleString("zh-CN", { hour12: false });
}

function rolesPage() {
  const selected = state.roles.find((role) => role.id === state.selectedRoleId) || state.roles[0];
  return `<section class="panel">
    <div class="panel-head"><div><h2>角色授权</h2><p>不再手填权限代码和币对 ID，直接勾选需要的范围。</p></div><button class="btn" data-action="reload-roles">刷新</button></div>
    ${state.roles.length ? `<div class="role-layout"><aside class="role-list">${state.roles.map((role) => `<button data-select-role="${role.id}" class="${selected?.id === role.id ? "active" : ""}"><span class="role-icon">${role.code === "super_admin" ? "★" : "R"}</span><span><b>${esc(role.name)}</b><small>${esc(role.code)}</small></span><i>›</i></button>`).join("")}</aside><div class="role-editor">${selected ? roleEditor(selected) : ""}</div></div>` : `<div class="loading-state">正在加载角色…</div>`}
  </section>`;
}

function roleEditor(role) {
  if (role.code === "super_admin") return `<article class="role-card locked"><div><span class="role-icon">★</span><div><h3>${esc(role.name)}</h3><p>${esc(role.code)} · 全部权限和全部币对</p></div></div><span class="state-badge success">系统内置</span></article>`;
  const groups = permissionGroups();
  return `<form class="role-card role-form" data-role="${role.id}">
    <div class="role-title"><div><span class="role-icon">R</span><div><h3>${esc(role.name)}</h3><p>${esc(role.code)}</p></div></div><button class="btn primary small" type="submit">保存角色</button></div>
    <div class="form-grid"><div class="field"><label>显示名称</label><input name="name" value="${esc(role.name)}" required></div><label class="switch-line"><input name="all_instruments" type="checkbox" ${role.all_instruments ? "checked" : ""}><span><b>访问全部币对</b><small>关闭后只允许访问下方勾选的币对</small></span></label></div>
    <div class="permission-sections">${groups.map((group) => `<div><h4>${esc(group.label)}</h4><div class="choice-grid">${group.items.map((permission) => checkboxChoice("permissions", permission.code, permission.name, permission.code, role.permissions?.includes(permission.code))).join("")}</div></div>`).join("")}</div>
    <div class="scope-section"><h4>币对数据范围</h4><div class="choice-grid">${state.draft.instruments.map((pair) => checkboxChoice("instruments", pair.id, `${pair.base.symbol}/${pair.quote.symbol}`, pair.id, role.instruments?.includes(pair.id))).join("") || `<span class="muted">暂无币对</span>`}</div></div>
  </form>`;
}

function permissionGroups() {
  const definitions = [
    ["配置与币对", ["config", "token", "instrument", "strategy"]],
    ["交易所与运行", ["venue", "runtime", "orders", "fills", "secrets"]],
    ["系统管理", ["dashboard", "audit", "users", "roles"]],
  ];
  return definitions.map(([label, prefixes]) => ({ label, items: state.permissions.filter((item) => prefixes.includes(item.code.split(":")[0])) }));
}

function checkboxChoice(name, value, label, helper, checked = false) {
  return `<label class="choice"><input type="checkbox" name="${esc(name)}" value="${esc(value)}" ${checked ? "checked" : ""}><span><b>${esc(label)}</b><small>${esc(helper)}</small></span></label>`;
}

function boundField(label, path, value, type = "text", editable = true, required = false, className = "") {
  return `<div class="field ${className}"><label>${esc(label)}</label><input data-path="${esc(path)}" data-type="${esc(type)}" type="${type === "number" ? "number" : "text"}" value="${esc(value)}" ${editable ? "" : "disabled"} ${required ? "required" : ""}></div>`;
}

function boundedNumberField(label, path, value, editable, min, max, helper = "") {
  return `<div class="field"><label>${esc(label)}</label><input data-path="${esc(path)}" data-type="number" type="number" value="${esc(value)}" min="${esc(min)}" max="${esc(max)}" step="1" ${editable ? "" : "disabled"}>${helper ? `<small>${esc(helper)}</small>` : ""}</div>`;
}

function boundMiBField(label, path, bytes, editable, min, max, helper = "") {
  const value = Math.max(1, Math.round(Number(bytes || 0) / 1048576));
  return `<div class="field"><label>${esc(label)}</label><input data-path="${esc(path)}" data-type="bytes-mib" type="number" value="${esc(value)}" min="${esc(min)}" max="${esc(max)}" step="1" ${editable ? "" : "disabled"}>${helper ? `<small>${esc(helper)}</small>` : ""}</div>`;
}

function boundTextarea(label, path, value, editable = true, className = "") {
  return `<div class="field ${className}"><label>${esc(label)}</label><textarea data-path="${esc(path)}" data-type="lines" ${editable ? "" : "disabled"}>${esc(value)}</textarea></div>`;
}

function boundCheck(label, path, value, editable = true) {
  return `<label class="switch-line"><input data-path="${esc(path)}" data-type="bool" type="checkbox" ${value ? "checked" : ""} ${editable ? "" : "disabled"}><span><b>${esc(label)}</b></span></label>`;
}

function plainField(label, name, placeholder, required = false, className = "", value = "") {
  return `<div class="field ${className}"><label>${esc(label)}</label><input name="${esc(name)}" placeholder="${esc(placeholder)}" value="${esc(value)}" ${required ? "required" : ""}></div>`;
}

function plainPasswordField(label, name, required = true) {
  return `<div class="field"><label>${esc(label)}</label><input name="${esc(name)}" type="password" ${required ? "required" : ""} autocomplete="new-password"></div>`;
}

function emptyState(title, description, actionLabel, action) {
  return `<div class="empty-state"><span>◇</span><h3>${esc(title)}</h3><p>${esc(description)}</p>${actionLabel ? `<button class="btn" data-action="${esc(action)}">${esc(actionLabel)}</button>` : ""}</div>`;
}

function bind() {
  document.querySelectorAll("[data-page]").forEach((button) => { button.onclick = () => navigate(button.dataset.page); });
  document.querySelectorAll("[data-open-pair]").forEach((button) => { button.onclick = () => openPair(button.dataset.openPair); });
  document.querySelectorAll("[data-open-runtime]").forEach((button) => { button.onclick = () => { openPair(button.dataset.openRuntime); state.pairTab = "runtime"; render(); }; });
  document.querySelectorAll("[data-runtime-action]").forEach((button) => { button.onclick = () => controlRuntime(button.dataset.runtimeAction, button.dataset.id); });
  document.querySelectorAll("[data-pair-tab]").forEach((button) => { button.onclick = () => { state.pairTab = button.dataset.pairTab; render(); }; });
  document.querySelectorAll("[data-path]").forEach((element) => { element.onchange = () => updateBoundInput(element); });
  document.querySelectorAll("[data-inspect-leg]").forEach((element) => { element.onchange = () => updatePairAddress(element); });
  document.querySelectorAll("[data-action]").forEach((button) => { button.onclick = () => handleAction(button.dataset.action, button); });
  document.querySelectorAll("[data-remove-market]").forEach((button) => { button.onclick = () => removeMarket(button.dataset.removeMarket); });
  document.querySelectorAll("[data-remove-venue]").forEach((button) => { button.onclick = () => removeVenue(button.dataset.removeVenue); });
  document.querySelectorAll("[data-edit-credential]").forEach((button) => { button.onclick = () => { state.ui.editCredentialId = Number(button.dataset.editCredential); state.ui.createCredential = false; render(); }; });
  document.querySelectorAll("[data-toggle-credential]").forEach((button) => { button.onclick = () => toggleCredential(Number(button.dataset.toggleCredential)); });
  document.querySelectorAll("[data-select-role]").forEach((button) => { button.onclick = () => { state.selectedRoleId = Number(button.dataset.selectRole); render(); }; });
  document.querySelectorAll("[data-edit-user]").forEach((button) => { button.onclick = () => { const id = Number(button.dataset.editUser); state.ui.editUserId = state.ui.editUserId === id ? null : id; render(); }; });
  document.querySelectorAll("[data-toggle-user]").forEach((button) => { button.onclick = () => toggleUser(Number(button.dataset.toggleUser)); });
  document.querySelectorAll("[data-revoke-user]").forEach((button) => { button.onclick = () => revokeUserSessions(Number(button.dataset.revokeUser)); });
  document.querySelectorAll("[data-validation-issue]").forEach((button) => { button.onclick = () => navigateValidationIssue(Number(button.dataset.validationIssue)); });
  $("#logout").onclick = logout;
  $("#change-password").onclick = () => { state.ui.passwordModal = true; render(); };
  $("#password-cancel") && ($("#password-cancel").onclick = closeOwnPasswordModal);
  $("#password-cancel-x") && ($("#password-cancel-x").onclick = closeOwnPasswordModal);
  $("#password-modal") && ($("#password-modal").onclick = (event) => { if (event.target.id === "password-modal") closeOwnPasswordModal(); });
  $("#change-own-password") && ($("#change-own-password").onsubmit = changeOwnPassword);
  $("#save") && ($("#save").onclick = saveDraft);
  $("#confirm-ok") && ($("#confirm-ok").onclick = runControl);
  $("#confirm-cancel") && ($("#confirm-cancel").onclick = () => { state.ui.confirm = null; render(); });
  $("#confirm-modal") && ($("#confirm-modal").onclick = (event) => { if (event.target.id === "confirm-modal") { state.ui.confirm = null; render(); } });
  document.onkeydown = (event) => {
    if (event.key !== "Escape") return;
    if (state.ui.passwordModal) closeOwnPasswordModal();
    else if (state.ui.confirm) { state.ui.confirm = null; render(); }
  };
  $("#discard") && ($("#discard").onclick = () => loadConfig());
  $("#validation-close") && ($("#validation-close").onclick = () => { state.ui.validationIssues = []; render(); });
  $("#pair-search") && ($("#pair-search").oninput = (event) => { state.search = event.target.value; render(); $("#pair-search")?.focus(); });
  $("#create-pair") && ($("#create-pair").onsubmit = createPair);
  $("#create-venue") && ($("#create-venue").onsubmit = createVenue);
  $("#new-venue-type") && ($("#new-venue-type").onchange = updateVenueDefaults);
  $("#add-market") && ($("#add-market").onsubmit = addMarket);
  $("#market-venue") && ($("#market-venue").onchange = updateMarketCredentials);
  $("#create-credential") && ($("#create-credential").onsubmit = createCredential);
  $("#update-credential") && ($("#update-credential").onsubmit = updateCredential);
  $("#create-user") && ($("#create-user").onsubmit = createUser);
  document.querySelectorAll("[data-update-user]").forEach((form) => { form.onsubmit = updateUser; });
  document.querySelectorAll("[data-reset-user-password]").forEach((form) => { form.onsubmit = resetUserPassword; });
  document.querySelectorAll(".role-form").forEach((form) => { form.onsubmit = saveRole; });
}

async function navigate(page) {
  state.page = page;
  state.selectedPairId = null;
  if (page === "credentials" && !state.credentials.length) await loadCredentials(false);
  if (page === "runtime") await loadRuntime(false);
  if (page === "users") await Promise.all([loadUsers(false), loadUserRoleOptions(false)]);
  if (page === "roles") {
    await Promise.all([loadRoles(false), loadPermissions(false)]);
    if (!state.selectedRoleId) state.selectedRoleId = state.roles[0]?.id || null;
  }
  render();
}

function openPair(id) {
  state.selectedPairId = id;
  state.page = "pair";
  state.pairTab = "basic";
  state.ui.addMarket = false;
  render();
}

function selectedPair() {
  return state.draft.instruments.find((pair) => pair.id === state.selectedPairId);
}

function pairIndex(id) {
  return state.draft.instruments.findIndex((pair) => pair.id === id);
}

function updateBoundInput(element) {
  let value = element.value;
  if (element.type === "radio") value = element.value;
  if (element.dataset.type === "bool") value = element.checked;
  if (element.dataset.type === "number") value = Number(element.value);
  if (element.dataset.type === "bytes-mib") value = Math.round(Number(element.value) * 1048576);
  if (element.dataset.type === "lines") value = element.value.split("\n").map((line) => line.trim()).filter(Boolean);
  setPath(element.dataset.path, value);
  if (element.dataset.path === "mode" && value === "live") {
    state.ui.safetyAdjustments = applyLiveSafetyDefaults(state.draft);
    if (state.ui.safetyAdjustments) toast(`已自动关闭 ${state.ui.safetyAdjustments} 个币对的暖机 Spot`);
  } else if (element.dataset.path === "mode") {
    state.ui.safetyAdjustments = 0;
  }
  if (element.dataset.path.endsWith(".environment")) {
    const venueName = element.dataset.path.split(".")[1];
    const configuredVenue = state.draft.venues?.[venueName];
    const spec = venueSpec(configuredVenue?.type);
    if (configuredVenue && spec) configuredVenue.base_url = value === "testnet" && spec.testnet_base_url ? spec.testnet_base_url : spec.production_base_url || configuredVenue.base_url;
  }
  if (element.dataset.path.includes(".trade_simulation.")) ensureTradeSimulationDefaults(Number(element.dataset.path.split(".")[1]));
  state.ui.validationIssues = [];
  state.dirty = true;
  render();
}

function applyLiveSafetyDefaults(config) {
  if (config?.mode !== "live") return 0;
  let adjusted = 0;
  (config.instruments || []).forEach((pair) => {
    if (pair.reference?.allow_spot_during_warmup) {
      pair.reference.allow_spot_during_warmup = false;
      adjusted += 1;
    }
  });
  return adjusted;
}

function navigateValidationIssue(index) {
  const issue = state.ui.validationIssues[index] || "";
  if (issue.startsWith("系统：")) return navigate("settings");
  const pair = (state.draft.instruments || []).find((item) => issue.startsWith(`${item.base.symbol || item.id}/${item.quote.symbol || "?"}：`));
  if (pair) {
    state.selectedPairId = pair.id;
    state.page = "pair";
    state.pairTab = /暖机|TWAP|价格路径|第\s*\d+\s*跳/.test(issue) ? "oracle" : /市场|凭证|精度/.test(issue) ? "markets" : /成交量仿真/.test(issue) ? "simulation" : /档|点差|数量|库存|资金|余额/.test(issue) ? "strategy" : "basic";
    return render();
  }
  return navigate("venues");
}

function ensureTradeSimulationDefaults(index) {
  const pair = state.draft.instruments?.[index];
  if (!pair) return;
  const source = marketsForPair(pair.id).find((item) => item.venue.enabled)?.name || "";
  pair.trade_simulation = {
    enabled: false,
    source_venue: source,
    min_quantity: "1",
    max_quantity: "10",
    min_interval_ms: 1000,
    max_interval_ms: 3000,
    buy_probability_bps: 5000,
    recent_limit: 50,
    ...(pair.trade_simulation || {}),
  };
  if (!pair.trade_simulation.source_venue) pair.trade_simulation.source_venue = source;
}

function setPath(path, value) {
  const parts = path.split(".");
  let target = state.draft;
  for (let index = 0; index < parts.length - 1; index += 1) {
    const key = parts[index];
    if (target[key] == null) target[key] = /^\d+$/.test(parts[index + 1]) ? [] : {};
    target = target[key];
  }
  target[parts.at(-1)] = value;
}

function handleAction(action, button) {
  if (action === "toggle-create-pair" || action === "show-create-pair") { state.ui.createPair = !state.ui.createPair; state.page = "pairs"; return render(); }
  if (action === "back-pairs") return navigate("pairs");
  if (action === "delete-pair") return deletePair();
  if (action === "add-leg") return addLeg();
  if (action === "remove-leg") return removeLeg(Number(button.dataset.leg));
  if (action === "inspect-pair") return inspectPair(Number(button.dataset.leg));
  if (action === "toggle-add-market") { state.ui.addMarket = !state.ui.addMarket; return render(); }
  if (action === "toggle-create-venue") { state.ui.createVenue = !state.ui.createVenue; return render(); }
  if (action === "toggle-create-credential") { state.ui.createCredential = !state.ui.createCredential; state.ui.editCredentialId = null; return render(); }
  if (action === "cancel-credential-edit") { state.ui.editCredentialId = null; return render(); }
  if (action === "reload-users") return Promise.all([loadUsers(false), loadUserRoleOptions(false)]).then(render);
  if (action === "reload-roles") return Promise.all([loadRoles(false), loadPermissions(false)]).then(render);
  if (action === "refresh-runtime") return loadRuntime();
}

function addLeg() {
  const pair = selectedPair();
  if (!pair) return;
  pair.reference.legs.push({ pair_address: "", expected_factory: "0xcA143Ce32Fe78f1f7019d7d551a6402fC5350c73", base_token: "", quote_token: "", min_quote_reserve: "0", max_idle_seconds: 300 });
  state.dirty = true;
  render();
}

function updatePairAddress(element) {
  const pair = selectedPair();
  if (!pair) return;
  const legIndex = Number(element.dataset.inspectLeg);
  const value = element.value.trim();
  pair.reference.legs[legIndex].pair_address = value;
  for (let index = legIndex; index < pair.reference.legs.length; index += 1) {
    pair.reference.legs[index].base_token = "";
    pair.reference.legs[index].quote_token = "";
    delete state.ui.pairInspection[`${pair.id}:${index}`];
  }
  state.dirty = true;
  render();
  if (isAddress(value)) inspectPair(legIndex);
}

async function inspectPair(legIndex, cascade = true) {
  const pair = selectedPair();
  const leg = pair?.reference?.legs?.[legIndex];
  if (!pair || !leg) return;
  const inputToken = legIndex === 0 ? pair.base.address : pair.reference.legs[legIndex - 1]?.quote_token;
  const key = `${pair.id}:${legIndex}`;
  if (!isAddress(leg.pair_address)) {
    state.ui.pairInspection[key] = { error: "请先填写有效的 Pair 地址。" };
    return render();
  }
  if (!isAddress(inputToken)) {
    state.ui.pairInspection[key] = { error: legIndex === 0 ? "请先填写币对的基础币 BSC 合约。" : "请先识别上一跳 Pair。" };
    return render();
  }
  state.ui.pairInspection[key] = { loading: true };
  render();
  try {
    const result = await api("/api/oracle/pancake-v2/inspect-pair", {
      method: "POST",
      body: JSON.stringify({ pair_address: leg.pair_address, input_token: inputToken, expected_factory: leg.expected_factory || "" }),
    });
    leg.base_token = result.base_token;
    leg.quote_token = result.quote_token;
    state.ui.pairInspection[key] = { factory: result.factory, token0: result.token0, token1: result.token1 };
    state.dirty = true;
    render();
    toast(`第 ${legIndex + 1} 跳识别成功`);
    const next = pair.reference.legs[legIndex + 1];
    if (cascade && next && isAddress(next.pair_address)) await inspectPair(legIndex + 1, true);
  } catch (error) {
    state.ui.pairInspection[key] = { error: error.message };
    render();
    toast(error.message, true);
  }
}

function removeLeg(index) {
  const pair = selectedPair();
  if (!pair) return;
  pair.reference.legs.splice(index, 1);
  state.ui.pairInspection = {};
  state.dirty = true;
  render();
}

async function createPair(event) {
  event.preventDefault();
  const body = Object.fromEntries(new FormData(event.currentTarget));
  const base = body.base_symbol.trim().toUpperCase();
  const quote = body.quote_symbol.trim().toUpperCase();
  const id = `${base}_${quote}`.toLowerCase();
  if (state.draft.instruments.some((pair) => pair.id === id)) return toast("这个币对已经存在", true);
  const pair = {
    id,
    base: { symbol: base, address: body.base_address.trim(), decimals: 18 },
    quote: { symbol: quote, address: body.quote_address.trim(), decimals: 18 },
    reference: { type: "pancake_v2", legs: [], twap_window_seconds: 60, max_spot_twap_deviation_bps: 200, stale_after_seconds: 20, allow_spot_during_warmup: state.draft.mode !== "live" },
    strategy: { half_spread_bps: 50, level_spacing_bps: 25, levels: 20, min_order_notional: "10", max_order_notional: "20", reprice_threshold_bps: 10, balance_reserve_bps: 500, max_venue_reference_deviation_bps: 500, max_venue_spread_bps: 1000, target_base: "0", max_base_deviation: "100", inventory_skew_bps: 30, quote_refresh_seconds: 45, quote_refresh_ratio_bps: 1000, min_order_lifetime_seconds: 30, max_order_lifetime_seconds: 300, price_jitter_ticks: 2, best_levels: 3, best_level_refresh_seconds: 90 },
    trade_simulation: { enabled: false, source_venue: "", min_quantity: "1", max_quantity: "10", min_interval_ms: 1000, max_interval_ms: 3000, buy_probability_bps: 5000, recent_limit: 50 },
  };
  state.draft.instruments.push(pair);
  state.ui.createPair = false;
  state.dirty = true;
  openPair(id);
}

function deletePair() {
  const pair = selectedPair();
  if (!pair || !confirm(`确定从草稿删除 ${pair.id}？相关交易市场映射也会删除。`)) return;
  state.draft.instruments = state.draft.instruments.filter((item) => item.id !== pair.id);
  Object.values(state.draft.venues || {}).forEach((venue) => { if (venue.markets) delete venue.markets[pair.id]; });
  state.dirty = true;
  navigate("pairs");
}

function addMarket(event) {
  event.preventDefault();
  const pair = selectedPair();
  const body = Object.fromEntries(new FormData(event.currentTarget));
  const venue = state.draft.venues[body.venue_name];
  if (!pair || !venue) return toast("交易所不存在", true);
  venue.markets ||= {};
  venue.markets[pair.id] = {
    symbol: body.symbol.trim(),
    base_asset: pair.base.symbol,
    quote_asset: pair.quote.symbol,
    price_tick: body.price_tick.trim(),
    quantity_step: body.quantity_step.trim(),
    min_notional: body.min_notional.trim(),
    credential_id: Number(body.credential_id || 0),
  };
  if (pair.trade_simulation && !pair.trade_simulation.source_venue) pair.trade_simulation.source_venue = body.venue_name;
  state.ui.addMarket = false;
  state.dirty = true;
  render();
}

function removeMarket(venueName) {
  const pair = selectedPair();
  if (!pair || !confirm(`确定解除 ${venueName} 与 ${pair.id} 的市场绑定？`)) return;
  delete state.draft.venues[venueName].markets[pair.id];
  state.dirty = true;
  render();
}

function updateMarketCredentials() {
  const venue = state.draft.venues[$("#market-venue").value];
  const select = $("#market-credential");
  select.innerHTML = `<option value="0">Shadow 暂不绑定</option>${credentialOptionHTML(venue.type)}`;
}

function credentialOptionHTML(venueType, current = 0) {
  return state.credentialOptions.filter((item) => item.venue_type === venueType).map((item) => `<option value="${item.id}" ${Number(current) === item.id ? "selected" : ""} ${item.enabled ? "" : "disabled"}>${esc(item.name)}${item.enabled ? "" : "（已停用）"}</option>`).join("");
}

function createVenue(event) {
  event.preventDefault();
  const body = Object.fromEntries(new FormData(event.currentTarget));
  const name = body.name.trim().toLowerCase();
  if (state.draft.venues[name]) return toast("交易所内部名称已存在", true);
  const spec = venueSpec(body.type) || {};
  state.draft.venues[name] = {
    type: body.type,
    environment: body.environment || "production",
    enabled: true,
    trading_enabled: false,
    dedicated_account: Boolean(spec.requires_dedicated_account),
    base_url: body.environment === "testnet" && spec.testnet_base_url ? spec.testnet_base_url : body.base_url.trim(),
    self_trade_prevention: body.self_trade_prevention.trim(),
    markets: {},
  };
  state.ui.createVenue = false;
  state.dirty = true;
  render();
}

function updateVenueDefaults() {
  const type = $("#new-venue-type").value;
  const form = $("#create-venue");
  const spec = venueSpec(type) || {};
  form.elements.name.value = type;
  form.elements.base_url.value = spec.production_base_url || "";
  form.elements.self_trade_prevention.value = spec.default_self_trade_prevention || "";
}

function venueSpec(type) {
  return state.venueTypes.find((item) => item.type === type);
}

function removeVenue(name) {
  const venue = state.draft.venues[name];
  if (Object.keys(venue.markets || {}).length) return toast("请先在币对详情中解除全部市场映射", true);
  if (!confirm(`确定删除交易所 ${name}？`)) return;
  delete state.draft.venues[name];
  state.dirty = true;
  render();
}

async function saveDraft() {
  const issues = configIssues();
  if (issues.length) {
    state.ui.validationIssues = issues;
    render();
    return toast(`还有 ${issues.length} 项需要完善，已列出具体原因`, true);
  }
  try {
    // Saving now takes effect immediately (the backend validates and activates
    // atomically); refresh the live config so the UI reflects what is running.
    await api("/api/config/draft", { method: "PUT", body: JSON.stringify(state.draft) });
    state.draft = await api("/api/config/draft");
    state.active = await api("/api/config/active").catch(() => state.active);
    state.dirty = false;
    state.ui.safetyAdjustments = 0;
    state.ui.validationIssues = [];
    toast("已保存，立即生效");
    render();
  } catch (error) {
    toast(error.message, true);
  }
}

async function logout() {
  await api("/api/logout", { method: "POST" });
  state.user = null;
  renderLogin();
}

function closeOwnPasswordModal() {
  state.ui.passwordModal = false;
  render();
}

async function changeOwnPassword(event) {
  event.preventDefault();
  const body = Object.fromEntries(new FormData(event.currentTarget));
  try {
    await api("/api/me/password", { method: "PUT", body: JSON.stringify(body) });
    state.ui.passwordModal = false;
    requireFreshLogin("密码已修改，请使用新密码重新登录");
  } catch (error) { toast(error.message, true); }
}

async function loadCredentials(redraw = true) {
  state.credentials = (await api("/api/credentials")) || [];
  if (redraw) render();
}

async function createCredential(event) {
  event.preventDefault();
  try {
    await api("/api/credentials", { method: "POST", body: JSON.stringify(Object.fromEntries(new FormData(event.currentTarget))) });
    state.ui.createCredential = false;
    await Promise.all([loadCredentials(false), loadCredentialOptions(false)]);
    toast("凭证已加密保存");
    render();
  } catch (error) { toast(error.message, true); }
}

async function updateCredential(event) {
  event.preventDefault();
  try {
    await api(`/api/credentials/${event.currentTarget.dataset.id}`, { method: "PUT", body: JSON.stringify(Object.fromEntries(new FormData(event.currentTarget))) });
    state.ui.editCredentialId = null;
    await Promise.all([loadCredentials(false), loadCredentialOptions(false)]);
    toast("凭证已更新");
    render();
  } catch (error) { toast(error.message, true); }
}

async function toggleCredential(id) {
  const item = state.credentials.find((credential) => credential.id === id);
  if (!item || !confirm(`确定${item.enabled ? "停用" : "启用"}凭证 ${item.name}？`)) return;
  try {
    await api(`/api/credentials/${id}`, { method: "PUT", body: JSON.stringify({ name: item.name, api_key: "", api_secret: "", enabled: !item.enabled }) });
    await Promise.all([loadCredentials(false), loadCredentialOptions(false)]);
    toast("凭证状态已更新");
    render();
  } catch (error) { toast(error.message, true); }
}

async function loadUsers(redraw = true) {
  state.users = (await api("/api/users")) || [];
  if (redraw) render();
}

async function loadUserRoleOptions(redraw = true) {
  state.userRoleOptions = (await api("/api/user-role-options")) || [];
  if (redraw) render();
}

async function loadRoles(redraw = true) {
  state.roles = (await api("/api/roles")) || [];
  if (redraw) render();
}

async function loadPermissions(redraw = true) {
  state.permissions = (await api("/api/permissions")) || [];
  if (redraw) render();
}

async function loadRuntime(redraw = true) {
  state.runtime = (await api("/api/runtime")) || { engine: { online: false, ready: false, version: 0, desired_version: 0 }, instruments: [] };
  state.runtime.instruments ||= [];
  if (redraw) render();
}

function controlRuntime(action, instrumentID) {
  const meta = {
    pause: { title: `关闭 ${instrumentID}`, body: "停止铺新单；已挂出的订单继续留在盘口。重启后保持关闭。", label: "关闭币对", danger: false },
    resume: { title: `开启 ${instrumentID}`, body: "引擎将在下一轮继续铺单。", label: "开启币对", danger: false },
    "emergency-cancel": { title: `紧急暂停并撤单 ${instrumentID}`, body: "立即停止铺单，并撤销该币对的全部受管挂单。", label: "紧急撤单", danger: true },
    reconcile: { title: `对账 ${instrumentID}`, body: "执行安全撤单、订单对账，并解除 OMS 阻断。", label: "开始对账", danger: false },
  }[action];
  if (!meta) return;
  state.ui.confirm = { ...meta, action, instrumentID };
  render();
}

async function runControl() {
  const request = state.ui.confirm;
  if (!request) return;
  state.ui.confirm = null;
  render();
  try {
    await api(`/api/runtime/${encodeURIComponent(request.instrumentID)}/${request.action}`, { method: "POST" });
    const done = {
      resume: "已开启，正在恢复铺单",
      reconcile: "对账解除指令已提交",
      pause: "已关闭：停止铺单，挂单保留",
      "emergency-cancel": "紧急撤单指令已提交，等待引擎确认",
    };
    toast(done[request.action] || "指令已提交");
    await loadRuntime();
  } catch (error) {
    toast(error.message, true);
  }
}

async function createUser(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const data = new FormData(form);
  const body = { email: data.get("email"), password: data.get("password"), roles: data.getAll("roles") };
  try {
    await api("/api/users", { method: "POST", body: JSON.stringify(body) });
    form.reset();
    toast("账户已创建");
    await loadUsers();
  } catch (error) { toast(error.message, true); }
}

async function updateUser(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const id = Number(form.dataset.updateUser);
  const data = new FormData(form);
  const current = Number(state.user?.user_id) === id;
  const body = { email: data.get("email"), enabled: current ? true : data.get("enabled") === "on", roles: data.getAll("roles") };
  try {
    await api(`/api/users/${id}`, { method: "PUT", body: JSON.stringify(body) });
    if (current) return requireFreshLogin("当前账户已更新，请使用新资料重新登录");
    state.ui.editUserId = null;
    await loadUsers(false);
    toast("账户资料和角色已更新，旧会话已失效");
    render();
  } catch (error) { toast(error.message, true); }
}

async function toggleUser(id) {
  const user = state.users.find((item) => Number(item.id) === id);
  if (!user || !confirm(`确定${user.enabled ? "停用" : "启用"}账户 ${user.email}？`)) return;
  try {
    await api(`/api/users/${id}`, { method: "PUT", body: JSON.stringify({ email: user.email, enabled: !user.enabled, roles: user.roles || [] }) });
    await loadUsers(false);
    toast(user.enabled ? "账户已停用，全部旧会话已失效" : "账户已启用");
    render();
  } catch (error) { toast(error.message, true); }
}

async function resetUserPassword(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const id = Number(form.dataset.resetUserPassword);
  const current = Number(state.user?.user_id) === id;
  const password = new FormData(form).get("password");
  if (!confirm("确定重置密码并使该账户全部旧会话失效？")) return;
  try {
    await api(`/api/users/${id}/password`, { method: "PUT", body: JSON.stringify({ password }) });
    if (current) return requireFreshLogin("密码已重置，请使用新密码重新登录");
    form.reset();
    await loadUsers(false);
    toast("密码已重置，旧会话已失效");
    render();
  } catch (error) { toast(error.message, true); }
}

async function revokeUserSessions(id) {
  const user = state.users.find((item) => Number(item.id) === id);
  if (!user || !confirm(`确定强制下线 ${user.email} 的全部会话？`)) return;
  const current = Number(state.user?.user_id) === id;
  try {
    await api(`/api/users/${id}/revoke-sessions`, { method: "POST" });
    if (current) return requireFreshLogin("当前账户已强制下线，请重新登录");
    toast("该账户的全部旧会话已失效");
  } catch (error) { toast(error.message, true); }
}

function requireFreshLogin(message) {
  state.user = null;
  if (runtimePollTimer) clearInterval(runtimePollTimer);
  renderLogin();
  toast(message);
}

async function saveRole(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const data = new FormData(form);
  const body = {
    name: data.get("name"),
    all_instruments: data.get("all_instruments") === "on",
    permissions: data.getAll("permissions"),
    instruments: data.getAll("instruments"),
  };
  try {
    await api(`/api/roles/${form.dataset.role}`, { method: "PUT", body: JSON.stringify(body) });
    toast("角色权限已保存");
    await loadRoles();
  } catch (error) { toast(error.message, true); }
}

function marketsForPair(pairId) {
  return Object.entries(state.draft.venues || {}).flatMap(([name, venue]) => (
    venue.markets?.[pairId] ? [{ name, venue, market: venue.markets[pairId] }] : []
  ));
}

function credentialBindings(credentialId) {
  return (state.draft.instruments || []).filter((pair) => marketsForPair(pair.id).some((item) => Number(item.market.credential_id) === credentialId)).map((pair) => pair.id);
}

function pairIssues(pair) {
  const issues = [];
  if (!pair.base?.symbol || !pair.quote?.symbol) issues.push("补充基础币和报价币符号");
  if (!isAddress(pair.base?.address) || !isAddress(pair.quote?.address)) issues.push("检查 Token 合约地址");
  if (!pair.reference?.legs?.length) issues.push("添加 Pancake V2 价格路径");
  if (!(Number(pair.reference?.twap_window_seconds) > 0)) issues.push("TWAP 窗口必须大于 0");
  if (state.draft.mode === "live" && pair.reference?.allow_spot_during_warmup) issues.push("实盘必须关闭暖机 Spot");
  let expectedPathToken = pair.base?.address;
  (pair.reference?.legs || []).forEach((leg, index) => {
    if (!isAddress(leg.pair_address) || !isAddress(leg.base_token) || !isAddress(leg.quote_token)) issues.push(`完善第 ${index + 1} 跳地址`);
    if (isAddress(expectedPathToken) && isAddress(leg.base_token) && expectedPathToken.toLowerCase() !== leg.base_token.toLowerCase()) issues.push(`第 ${index + 1} 跳没有承接上一跳 Token`);
    expectedPathToken = leg.quote_token;
  });
  if ((pair.reference?.legs || []).length && isAddress(expectedPathToken) && isAddress(pair.quote?.address) && expectedPathToken.toLowerCase() !== pair.quote.address.toLowerCase()) issues.push("价格路径最终没有到达币对报价币");
  const levels = Number(pair.strategy?.levels);
  if (!(Number.isInteger(levels) && levels >= 1 && levels <= 100)) issues.push("档数必须为 1–100 的整数");
  const outermostSpread = Number(pair.strategy?.half_spread_bps) + Math.max(0, levels - 1) * Number(pair.strategy?.level_spacing_bps);
  if (!(outermostSpread >= 0 && outermostSpread < 10000)) issues.push("最外档距离必须小于 10000 bps（100%）");
  const sizingIssue = strategyOrderSizingIssue(pair.strategy);
  if (sizingIssue) issues.push(sizingIssue);
  const balanceReserve = Number(pair.strategy?.balance_reserve_bps ?? 0);
  if (!(Number.isInteger(balanceReserve) && balanceReserve >= 0 && balanceReserve <= 10000)) issues.push("余额预留必须为 0–10000 bps 的整数");
  if (!(Number(pair.strategy?.max_venue_reference_deviation_bps ?? 0) >= 0)) issues.push("交易所/指数最大偏差不能为负数");
  if (!(Number(pair.strategy?.max_venue_spread_bps ?? 0) >= 0)) issues.push("交易所盘口最大点差不能为负数");
  const refreshSeconds = Number(pair.strategy?.quote_refresh_seconds ?? 45);
  const refreshRatio = Number(pair.strategy?.quote_refresh_ratio_bps ?? 1000);
  const minLifetime = Number(pair.strategy?.min_order_lifetime_seconds ?? 30);
  const maxLifetime = Number(pair.strategy?.max_order_lifetime_seconds ?? 300);
  const jitterTicks = Number(pair.strategy?.price_jitter_ticks ?? 2);
  const bestLevels = Number(pair.strategy?.best_levels ?? Math.min(3, levels));
  const bestRefreshSeconds = Number(pair.strategy?.best_level_refresh_seconds ?? 90);
  if (!(Number.isInteger(refreshSeconds) && refreshSeconds >= 10)) issues.push("盘口轮换间隔不能小于 10 秒");
  if (!(Number.isInteger(refreshRatio) && refreshRatio >= 1 && refreshRatio <= 10000)) issues.push("每轮轮换比例必须为 1–10000 bps");
  if (!(Number.isInteger(minLifetime) && minLifetime >= 5)) issues.push("最短挂单存活不能小于 5 秒");
  if (!(Number.isInteger(maxLifetime) && maxLifetime >= minLifetime)) issues.push("最长挂单存活不能小于最短挂单存活");
  if (!(Number.isInteger(jitterTicks) && jitterTicks >= 1 && jitterTicks <= 100)) issues.push("价格扰动必须为 1–100 Tick");
  if (!(Number.isInteger(bestLevels) && bestLevels >= 1 && bestLevels <= levels)) issues.push("最优档数量必须在总档数范围内");
  if (!(Number.isInteger(bestRefreshSeconds) && bestRefreshSeconds >= refreshSeconds)) issues.push("最优档轮换间隔不能短于普通轮换间隔");
  const markets = marketsForPair(pair.id);
  const enabledMarkets = markets.filter((item) => item.venue.enabled);
  if (!enabledMarkets.length) issues.push("至少绑定一个已启用的交易市场");
  markets.forEach((item) => {
    const market = item.market;
    if (!market.symbol || !market.base_asset || !market.quote_asset || !(Number(market.price_tick) > 0) || !(Number(market.quantity_step) > 0) || !(Number(market.min_notional) > 0)) issues.push(`完善 ${item.name} 市场精度`);
    if (Number(market.max_base_commitment ?? 0) < 0 || Number(market.max_quote_commitment ?? 0) < 0) issues.push(`${item.name} 资金占用上限不能为负数`);
    if (state.draft.mode === "live" && item.venue.trading_enabled && !Number(market.credential_id)) issues.push(`${item.name} 实盘需要凭证`);
  });
  const simulation = pair.trade_simulation || {};
  if (simulation.enabled) {
    if (!enabledMarkets.some((item) => item.name === simulation.source_venue)) issues.push("成交量仿真需要选择已启用且已绑定的市场数据来源");
    if (!(Number(simulation.min_quantity) > 0) || Number(simulation.max_quantity) < Number(simulation.min_quantity)) issues.push("成交量仿真的单事件数量范围无效");
    if (!(Number(simulation.min_interval_ms) >= 100) || Number(simulation.max_interval_ms) < Number(simulation.min_interval_ms)) issues.push("成交量仿真间隔必须至少 100ms，且最长不能小于最短");
    if (!(Number(simulation.buy_probability_bps) >= 0 && Number(simulation.buy_probability_bps) <= 10000)) issues.push("成交量仿真买方向概率必须为 0–10000 bps");
    if (!(Number(simulation.recent_limit) >= 1 && Number(simulation.recent_limit) <= 200)) issues.push("成交量仿真页面保留事件数必须为 1–200");
  }
  return [...new Set(issues)];
}

function strategyOrderSizingIssue(strategy = {}) {
  const minimum = Number(strategy.min_order_notional ?? 0);
  const maximum = Number(strategy.max_order_notional ?? 0);
  const usesNotionalRange = minimum !== 0 || maximum !== 0;
  if (usesNotionalRange) {
    if (!(minimum > 0) || !(maximum > 0)) return "每档最小和最大金额必须都大于 0";
    if (maximum < minimum) return "每档最大金额不能小于最小金额";
    return "";
  }
  if (!(Number(strategy.order_size) > 0)) return "请填写每档最小和最大金额";
  return "";
}

function configIssues() {
  const issues = [];
  if (!["shadow", "live"].includes(state.draft.mode)) issues.push("系统：运行模式必须是 Shadow 或 Live");
  if (!(Number(state.draft.poll_interval_ms) >= 250)) issues.push("系统：轮询间隔不能小于 250ms");
  if (!(Number(state.draft.rules_refresh_seconds ?? 0) === 0 || Number(state.draft.rules_refresh_seconds) >= 30)) issues.push("系统：交易规则刷新间隔不能小于 30 秒（0 使用默认值 300 秒）");
  if (!(Number(state.draft.max_concurrent_instruments ?? 0) >= 0 && Number(state.draft.max_concurrent_instruments ?? 0) <= 32)) issues.push("系统：币对最大并发数必须为 1–32（0 使用默认值 4）");
  if (!(Number(state.draft.audit_max_bytes ?? 0) === 0 || Number(state.draft.audit_max_bytes) >= 1048576)) issues.push("系统：审计单文件上限不能小于 1MiB");
  if (!(Number(state.draft.audit_backups ?? 0) >= 0 && Number(state.draft.audit_backups ?? 0) <= 30)) issues.push("系统：审计备份份数必须为 1–30（0 使用默认值 7）");
  if (!(Number(state.draft.market_failure_threshold ?? 3) >= 1)) issues.push("系统：连续失败撤单阈值必须大于 0");
  if (!(Number(state.draft.market_recovery_threshold ?? 3) >= 1)) issues.push("系统：连续恢复阈值必须大于 0");
  if (!(Number(state.draft.market_error_grace_seconds ?? 15) >= 1)) issues.push("系统：故障安全宽限必须大于 0");
  if (!(Number(state.draft.trading_progress_timeout_seconds ?? 120) >= 30)) issues.push("系统：交易循环卡死超时不能小于 30 秒");
  if (state.draft.mode === "live" && (!state.draft.heartbeat_path || !(Number(state.draft.watchdog_timeout_seconds) > 0))) issues.push("系统：实盘需要心跳文件和 Watchdog 超时");
  if (!(state.draft.rpc?.urls || []).length) issues.push("系统：至少配置一个 BNB Chain RPC");
  if (!(Number(state.draft.rpc?.chain_id) > 0)) issues.push("系统：Chain ID 必须大于 0");
  if (!(Number(state.draft.rpc?.request_timeout_ms) > 0)) issues.push("系统：RPC 请求超时必须大于 0");
  if (!(state.draft.instruments || []).length) issues.push("币对：至少创建一个币对");
  (state.draft.instruments || []).forEach((pair) => pairIssues(pair).forEach((issue) => issues.push(`${pair.base.symbol || pair.id}/${pair.quote.symbol || "?"}：${issue}`)));
  Object.entries(state.draft.venues || {}).forEach(([name, venue]) => {
    if (!venue.enabled) return;
    const spec = venueSpec(venue.type);
    if (!spec) issues.push(`${name}：不支持的交易所类型`);
    if (!venue.base_url) issues.push(`${name}：API 地址不能为空`);
    if (!["production", "testnet"].includes(venue.environment || "production")) issues.push(`${name}：运行环境无效`);
    if (spec?.testnet_base_url && venue.environment === "testnet" && venue.base_url !== spec.testnet_base_url) issues.push(`${name}：${spec.name} 测试网地址必须为 ${spec.testnet_base_url}`);
    if (spec?.requires_self_trade_prevention && venue.trading_enabled && (!venue.self_trade_prevention || venue.self_trade_prevention.toUpperCase() === "NONE")) issues.push(`${name}：允许下单时必须启用 STP`);
    if (state.draft.mode === "live" && spec?.requires_dedicated_account && venue.trading_enabled && !venue.dedicated_account) issues.push(`${name}：${spec.name} 实盘必须使用专用账户`);
  });
  return issues;
}

function readinessPercent() {
  const total = 4 + (state.draft.instruments || []).length * 6;
  return Math.max(0, Math.round(((total - Math.min(configIssues().length, total)) / total) * 100));
}

function isAddress(value) {
  return /^0x[0-9a-fA-F]{40}$/.test(String(value || ""));
}

function shortAddress(value) {
  if (!value) return "地址未填写";
  return value.length > 16 ? `${value.slice(0, 8)}…${value.slice(-6)}` : value;
}

function formatDate(value) {
  if (!value) return "—";
  return new Intl.DateTimeFormat("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }).format(new Date(value));
}

function runtimeSnapshot(instrumentID) {
  return (state.runtime.instruments || []).find((item) => item.instrument_id === instrumentID);
}

function formatRelative(value) {
  if (!value) return "尚未更新";
  const seconds = Math.max(0, Math.round((Date.now() - new Date(value).getTime()) / 1000));
  if (seconds < 2) return "刚刚";
  if (seconds < 60) return `${seconds} 秒前`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟前`;
  return formatDate(value);
}

function connectionLabel(value) {
  return value ? "在线" : "离线";
}

function spreadBps(bid, ask) {
  const bidValue = Number(bid);
  const askValue = Number(ask);
  if (!(bidValue > 0) || !(askValue > bidValue)) return null;
  return ((askValue - bidValue) / ((askValue + bidValue) / 2)) * 10000;
}

function bookDisplayState(book) {
  const hasBid = isPositiveDecimal(book?.bid_price);
  const hasAsk = isPositiveDecimal(book?.ask_price);
  if (!book) return { hasBid, hasAsk, twoSided: false, label: "盘口接口不可用 · 按指数价铺单" };
  if (!hasBid && !hasAsk) return { hasBid, hasAsk, twoSided: false, label: "空盘口 · 按指数价铺单" };
  if (!hasBid || !hasAsk) return { hasBid, hasAsk, twoSided: false, label: "单边盘口 · 按指数价补单" };
  return { hasBid, hasAsk, twoSided: true, label: formatRelative(book.timestamp) };
}

function isPositiveDecimal(value) {
  const parts = positiveDecimalParts(value);
  return Boolean(parts && /[1-9]/.test(parts.integer + parts.fraction));
}

function decimalAdd(left, right) {
  const value = Number(left || 0) + Number(right || 0);
  return Number.isFinite(value) ? value.toLocaleString("en-US", { maximumFractionDigits: 12 }) : "—";
}

boot();
