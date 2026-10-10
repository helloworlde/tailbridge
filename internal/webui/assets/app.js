const $ = (id) => document.getElementById(id);
let lastStatus = null;
const probeResults = new Map();

const escapeHTML = (value) => String(value ?? "").replace(/[&<>'"]/g, (char) => ({
  "&": "&amp;", "<": "&lt;", ">": "&gt;", "'": "&#39;", '"': "&quot;"
}[char]));

const stateClass = (value) => value === "running" || value === "available" || value === "healthy" ? "state-good" : value === "failed" || value === "stale" ? "state-bad" : "";

function activePath(network = {}) {
  if (network.peer_relay_path_count) return "peer relay";
  if (network.direct_path_count) return "direct";
  if (network.derp_path_count) return "DERP";
  return network.active_peer_count ? "unknown" : "—";
}

async function loadMeta() {
  const response = await fetch("/api/v1/meta", { cache: "no-store" });
  if (!response.ok) throw new Error("无法读取配置元数据");
  const meta = await response.json();
  $("route-profile").innerHTML = meta.profiles.map((id) => `<option value="${escapeHTML(id)}">${escapeHTML(id)}</option>`).join("");
}

async function loadStatus() {
  try {
    const response = await fetch("/api/v1/status", { cache: "no-store" });
    if (!response.ok) throw new Error("状态不可用");
    renderStatus(await response.json());
  } catch (error) {
    if (lastStatus) renderStatus({ ...lastStatus, state: "unavailable", alive: false });
    $("runtime-state").textContent = "状态不可用";
    $("runtime-dot").className = "status-dot bad";
  }
}

function renderStatus(status) {
  if (lastStatus && lastStatus.started_at !== status.started_at) probeResults.clear();
  lastStatus = status;
  const profiles = status.profiles || [];
  const routes = status.routes || [];
  const running = status.state === "running" && status.alive !== false;
  $("runtime-state").textContent = running ? "运行中" : status.state || "未知";
  $("runtime-dot").className = `status-dot ${running ? "good" : status.state === "failed" ? "bad" : "neutral"}`;
  $("summary-state").textContent = running ? "运行中" : status.state || "—";
  $("summary-state").className = running ? "state-good" : stateClass(status.state);
  $("summary-profiles").textContent = profiles.length;
  $("summary-routes").textContent = routes.length;
  $("summary-path").textContent = profiles.map((p) => activePath(p.network)).find((p) => p !== "—") || "—";

  $("profiles-body").innerHTML = profiles.length ? profiles.map((profile) => {
    const n = profile.network || {};
    const available = running && n.state === "available";
    const health = !available ? "未知" : n.health_issue_count ? `${n.health_issue_count} 个问题` : "正常";
    return `<tr><td>${escapeHTML(profile.id)}</td><td class="${running ? stateClass(profile.state) : ""}">${escapeHTML(running ? profile.state : "未知")}</td><td>${n.online_peer_count || 0} / ${n.peer_count || 0}</td><td>${escapeHTML(activePath(n))}</td><td>${n.primary_subnet_route_count || 0}</td><td class="${!available ? "" : n.health_issue_count ? "state-bad" : "state-good"}">${health}</td></tr>`;
  }).join("") : '<tr><td colspan="6" class="empty">没有 profile</td></tr>';

  $("routes-body").innerHTML = routes.length ? routes.map((route) => `<tr><td>${escapeHTML(route.id)}</td><td>${escapeHTML(String(route.kind || "").toUpperCase())}</td><td class="mono">${escapeHTML(route.listen)}</td><td class="${!running ? "" : route.accepting ? "state-good" : "state-bad"}">${!running ? "未知" : route.accepting ? "监听中" : "已停止"}</td><td>${route.active_connections || 0}</td><td>${route.connections_total || 0}</td><td>${renderProbe(route.id, running)}</td></tr>`).join("") : '<tr><td colspan="7" class="empty">尚未配置路由</td></tr>';
}

function renderProbe(id, running) {
  const result = probeResults.get(id);
  let label = result?.pending ? "检查中…" : "尚未检查";
  let state = "";
  let age = "";
  if (result && !result.pending) {
    const errors = { target_timeout: "连接超时", target_refused: "连接被拒绝", magicdns_failed: "DNS 解析失败", tls_failed: "TLS 验证失败", invalid_configuration: "配置不可用", diagnostics_unavailable: "诊断暂不可用" };
    label = result.reachable ? result.http_status ? `可达 · HTTP ${result.http_status}` : "TCP 可达" : errors[result.error_class] || "连接失败";
    state = result.reachable && !(result.http_status >= 500) ? "state-good" : "state-bad";
    const seconds = Math.max(0, Math.floor((Date.now() - result.finished) / 1000));
    age = `${seconds < 60 ? `${seconds} 秒前` : `${Math.floor(seconds / 60)} 分钟前`} · ${result.duration_ms || 0} ms`;
  }
  return `<div class="probe-controls"><button class="button secondary" type="button" data-probe="${escapeHTML(id)}" aria-label="检查 ${escapeHTML(id)}" ${!running || result?.pending ? "disabled" : ""}>检查</button><span class="probe-result ${state}" role="status">${escapeHTML(label)}<small class="probe-age">${escapeHTML(age)}</small></span></div>`;
}

async function probeRoute(id) {
  if (!lastStatus || lastStatus.state !== "running" || probeResults.get(id)?.pending) return;
  const pending = { pending: true };
  probeResults.set(id, pending);
  renderStatus(lastStatus);
  const controller = new AbortController();
  const timeout = window.setTimeout(() => controller.abort(), 6500);
  let result;
  try {
    const response = await fetch("/api/v1/routes/probe", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ id }), signal: controller.signal });
    if (!response.ok) throw new Error("probe unavailable");
    result = await response.json();
  } catch (error) {
    result = { reachable: false, error_class: "diagnostics_unavailable" };
  } finally {
    window.clearTimeout(timeout);
  }
  if (probeResults.get(id) !== pending) return;
  probeResults.set(id, { ...result, finished: Date.now() });
  renderStatus(lastStatus);
}

function routeInput() {
  return {
    id: $("route-id").value.trim(),
    profile: $("route-profile").value,
    kind: $("route-kind").value,
    local_port: Number($("route-port").value),
    upstream: $("route-upstream").value.trim()
  };
}

function yamlQuote(value) {
  return JSON.stringify(String(value));
}

function renderYAML() {
  const route = routeInput();
  const lines = [
    `- id: ${yamlQuote(route.id || "route-name")}`,
    `  profile: ${yamlQuote(route.profile || "home")}`,
    `  kind: ${route.kind}`,
    `  local_port: ${route.local_port || 18080}`,
    `  upstream: ${yamlQuote(route.upstream || (route.kind === "http" ? "http://service:3000" : "service:22"))}`
  ];
  $("yaml-preview").textContent = lines.join("\n");
  $("copy-yaml").disabled = true;
  $("validation-state").textContent = "尚未校验";
  $("validation-state").className = "";
}

async function validateRoute(event) {
  event.preventDefault();
  if (!event.currentTarget.reportValidity()) return;
  const response = await fetch("/api/v1/routes/validate", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(routeInput())
  });
  const result = await response.json();
  if (response.ok && result.valid) {
    $("validation-state").textContent = "校验通过";
    $("validation-state").className = "good";
    $("copy-yaml").disabled = false;
  } else {
    $("validation-state").textContent = result.error || "校验失败";
    $("validation-state").className = "bad";
    $("copy-yaml").disabled = true;
  }
}

async function copyYAML() {
  await navigator.clipboard.writeText($("yaml-preview").textContent);
  $("copy-yaml").textContent = "已复制";
  window.setTimeout(() => { $("copy-yaml").textContent = "复制 YAML"; }, 1400);
}

function updateKindHelp() {
  const http = $("route-kind").value === "http";
  $("route-upstream").placeholder = http ? "http://service:3000" : "service:22";
  $("upstream-help").textContent = http ? "HTTP(S) URL，例如 http://service:3000" : "host:port，例如 service:22";
  renderYAML();
}

document.addEventListener("DOMContentLoaded", async () => {
  try { await loadMeta(); } catch (error) { $("validation-state").textContent = error.message; }
  await loadStatus();
  renderYAML();
  $("refresh").addEventListener("click", loadStatus);
  $("routes-body").addEventListener("click", (event) => { const button = event.target.closest("button[data-probe]"); if (button) probeRoute(button.dataset.probe); });
  $("new-route").addEventListener("click", () => { $("route-id").focus(); location.hash = "configuration"; });
  $("route-form").addEventListener("input", renderYAML);
  $("route-form").addEventListener("submit", validateRoute);
  $("route-kind").addEventListener("change", updateKindHelp);
  $("copy-yaml").addEventListener("click", copyYAML);
  window.setInterval(loadStatus, 3000);
});
