// fluid-config console. Plain JS, no build step. All DOM is built with
// textContent / createElement, never innerHTML, so payload data cannot inject markup.
"use strict";

// ---------------------------------------------------------------- helpers

const $app = document.getElementById("app");
const MASK = "********";
// External path prefix (e.g. "/fluid-admin"), filled in by the server.
const BASE = (() => {
  const b = document.querySelector('meta[name="fluid-base"]')?.content || "";
  return b.includes("{{") ? "" : b.replace(/\/$/, "");
})();
// appPath is the route inside the app, without the base prefix.
function appPath() {
  const p = location.pathname;
  return BASE && (p === BASE || p.startsWith(BASE + "/")) ? p.slice(BASE.length) || "/" : p;
}

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "value") el.value = v;
    else if (k === "checked") el.checked = !!v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === undefined || c === null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

// fill replaces an element's children, skipping null/false (replaceChildren would print "null").
function fill(el, ...children) {
  el.replaceChildren(...children.flat(Infinity).filter((c) => c !== null && c !== undefined && c !== false));
  return el;
}

const SVGNS = "http://www.w3.org/2000/svg";
const ICONS = {
  home: "M3 10.5 12 3l9 7.5V21h-6v-6H9v6H3z",
  inbox: "M3 13h5l2 3h4l2-3h5M5 5h14l2 8v6H3v-6z",
  send: "M3 11 21 3l-8 18-2-8z",
  board: "M4 4h4v16H4zM10 4h4v10h-4zM16 4h4v13h-4z",
  list: "M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01",
  hook: "M9 7a3 3 0 1 1 6 0c0 2-3 3-3 6v2a3 3 0 1 0 3 3",
  route: "M6 3v12a3 3 0 0 0 3 3h9M15 15l3 3-3 3M6 3l-3 3M6 3l3 3",
  key: "M15 7a4 4 0 1 1-4 4M11 11 3 19v2h3l1-1v-2h2v-2h2l1-1",
  shield: "M12 3 4 6v6c0 5 4 8 8 9 4-1 8-4 8-9V6z",
  gear: "M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM19 12l2-1-1-3-2 .5-1.5-1.5L17 5l-3-1-1 2h-2L10 4 7 5l.5 2L6 8.5 4 8l-1 3 2 1v0l-2 1 1 3 2-.5L7.5 17 7 19l3 1 1-2h2l1 2 3-1-.5-2 1.5-1.5 2 .5 1-3z",
  plus: "M12 5v14M5 12h14",
  refresh: "M20 11a8 8 0 1 0-2.3 5.7M20 4v7h-7",
  close: "M6 6l12 12M18 6 6 18",
  retry: "M4 4v6h6M20 20v-6h-6M5 15a7 7 0 0 0 12 3M19 9A7 7 0 0 0 7 6",
  edit: "M4 20h4L20 8l-4-4L4 16zM14 6l4 4",
  trash: "M4 7h16M10 11v6M14 11v6M6 7l1 13h10l1-13M9 7V4h6v3",
  logout: "M15 4h4v16h-4M10 8l-4 4 4 4M6 12h10",
  menu: "M4 6h16M4 12h16M4 18h16",
  down: "M12 5v14M6 13l6 6 6-6",
  up: "M12 19V5M6 11l6-6 6 6",
  logs: "M5 4h14v16H5zM8 8h8M8 12h8M8 16h5",
  download: "M12 4v11M7 10l5 5 5-5M5 20h14",
  lock: "M6 11h12v10H6zM8 11V7a4 4 0 0 1 8 0v4",
};
function icon(name) {
  const s = document.createElementNS(SVGNS, "svg");
  s.setAttribute("viewBox", "0 0 24 24");
  s.setAttribute("fill", "none");
  s.setAttribute("stroke", "currentColor");
  s.setAttribute("stroke-width", "2");
  s.setAttribute("stroke-linecap", "round");
  s.setAttribute("stroke-linejoin", "round");
  s.setAttribute("aria-hidden", "true");
  const p = document.createElementNS(SVGNS, "path");
  p.setAttribute("d", ICONS[name] || "");
  s.append(p);
  return s;
}

const COLORS = ["#0c66e4", "#6e5dc6", "#22a06b", "#c9372c", "#e56910", "#1d7f8c", "#943d73", "#5b7f24"];
function avatar(name, small) {
  name = name || "?";
  let n = 0;
  for (const ch of name) n = (n * 31 + ch.charCodeAt(0)) >>> 0;
  const el = h("span", { class: "avatar" + (small ? " sm" : ""), title: name }, name.slice(0, 2).toUpperCase());
  el.style.background = COLORS[n % COLORS.length];
  return el;
}

function lozenge(text, kind) { return h("span", { class: "lz " + (kind || "") }, text); }
const STATUS_KIND = { delivered: "success", ok: "success", pending: "info", inflight: "purple", failed: "danger",
  dead: "danger", partial: "warn", error: "danger", warn: "warn", info: "", debug: "" };
function statusLozenge(s) { return lozenge(s === "inflight" ? "in flight" : s, STATUS_KIND[s] || ""); }
function httpLozenge(code) {
  if (!code) return h("span", { class: "muted" }, "—");
  const kind = code >= 500 ? "danger" : code >= 400 ? "warn" : code >= 300 ? "info" : "success";
  return lozenge(String(code), kind);
}
function dirTag(d) {
  if (d === "inbound") return h("span", { class: "dir in", title: "Incoming call" }, icon("down"), "IN");
  if (d === "outbound") return h("span", { class: "dir out", title: "Outgoing call" }, icon("up"), "OUT");
  return h("span", { class: "muted" }, "—");
}

function ago(ts) {
  const d = (Date.now() - new Date(ts).getTime()) / 1000;
  if (d < 5) return "just now";
  if (d < 60) return Math.floor(d) + "s ago";
  if (d < 3600) return Math.floor(d / 60) + "m ago";
  if (d < 86400) return Math.floor(d / 3600) + "h ago";
  return Math.floor(d / 86400) + "d ago";
}
// ---- date & time: always shown in the viewer's local time zone ----
const TZ = Intl.DateTimeFormat().resolvedOptions().timeZone || "local";
const TZ_SHORT = (() => {
  try { return new Intl.DateTimeFormat(undefined, { timeZoneName: "short" }).formatToParts(new Date()).find((x) => x.type === "timeZoneName").value; }
  catch { return TZ; }
})();
const DT_FMT = new Intl.DateTimeFormat(undefined, { year: "numeric", month: "short", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23" });
function validTime(ts) { if (!ts) return null; const d = new Date(ts); return isNaN(d) || d.getFullYear() < 2000 ? null : d; }
function fmtDT(ts) { const d = validTime(ts); return d ? DT_FMT.format(d) + " " + TZ_SHORT : ""; }
// when renders a local date-time; the tooltip adds "5m ago" and UTC.
function when(ts) {
  const d = validTime(ts);
  if (!d) return h("span", { class: "muted" }, "—");
  return h("span", { class: "dt", title: ago(ts) + " · " + d.toISOString().replace("T", " ").replace(/\.\d+Z$/, " UTC") }, DT_FMT.format(d));
}
function bytes(n) {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i ? n.toFixed(1) : n) + " " + u[i];
}
function eventKey(ev) { return (ev.route || "ev").toUpperCase().slice(0, 10) + "-" + ev.id.slice(-6).toUpperCase(); }
function pretty(s) {
  try { return JSON.stringify(JSON.parse(s), null, 2); } catch { return s; }
}

function toast(msg, err) {
  const t = h("div", { class: "toast" + (err ? " err" : "") }, msg);
  document.getElementById("toasts").append(t);
  setTimeout(() => t.remove(), err ? 7000 : 3500);
}

// ---------------------------------------------------------------- API

const state = { token: null, me: null, live: false, stream: null, page: null, status: null };

try { state.token = sessionStorage.getItem("fluid-token"); } catch { /* storage blocked */ }

class ApiError extends Error {
  constructor(status, msg) { super(msg); this.status = status; }
}

async function api(method, path, body, headers) {
  const opts = { method, headers: { Authorization: "Bearer " + state.token, ...(headers || {}) } };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(BASE + path, opts);
  if (resp.status === 401) { logout(); throw new ApiError(401, "Session expired"); }
  if (resp.status === 204) return null;
  const text = await resp.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { error: text }; }
  if (!resp.ok) throw new ApiError(resp.status, (data && data.error) || resp.statusText);
  return data;
}

// download fetches a file with the session token and saves it.
async function download(path, fallbackName) {
  const resp = await fetch(BASE + path, { headers: { Authorization: "Bearer " + state.token } });
  if (resp.status === 401) { logout(); throw new ApiError(401, "Session expired"); }
  if (!resp.ok) { let m = resp.statusText; try { m = (await resp.json()).error || m; } catch { /* not JSON */ } throw new ApiError(resp.status, m); }
  const name = (/filename="([^"]+)"/.exec(resp.headers.get("Content-Disposition") || "") || [])[1] || fallbackName;
  saveBlob(await resp.blob(), name);
}
function saveBlob(blob, name) {
  const u = URL.createObjectURL(blob);
  const a = h("a", { href: u, download: name });
  document.body.appendChild(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(u), 10000);
}
// csvCell quotes a value and neutralises spreadsheet formulas (= + - @).
function csvCell(v) {
  let s = v === null || v === undefined ? "" : Array.isArray(v) ? v.join(" ") : String(v);
  if (typeof v === "string" && /^[=+\-@\t\r]/.test(s)) s = "'" + s;
  return /[",\n\r]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s;
}
// isoLocal formats a time with the viewer's UTC offset: 2026-11-17T20:31:43+05:30
function isoLocal(ts) {
  const d = validTime(ts); if (!d) return "";
  const p = (n) => String(Math.abs(n)).padStart(2, "0"), off = -d.getTimezoneOffset();
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}${off >= 0 ? "+" : "-"}${p(Math.trunc(off / 60))}:${p(off % 60)}`;
}
function stamp() { return isoLocal(new Date()).slice(0, 19).replace(/[-:]/g, "").replace("T", "-"); }

const isEditor = () => state.me && state.me.role === "editor";

function logout() {
  try { sessionStorage.removeItem("fluid-token"); } catch { /* ignore */ }
  state.token = null;
  state.me = null;
  stopLive();
  renderLogin();
}

// ---------------------------------------------------------------- live stream

// The stream is read with fetch() so the token stays in a header.
async function startLive() {
  if (state.stream) return;
  const ctrl = new AbortController();
  state.stream = ctrl;
  setLiveIndicator(true);
  try {
    const resp = await fetch(BASE + "/api/stream?topics=logs,audit", {
      headers: { Authorization: "Bearer " + state.token }, signal: ctrl.signal });
    const reader = resp.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf("\n\n")) >= 0) {
        const chunk = buf.slice(0, i);
        buf = buf.slice(i + 2);
        let ev = "message", data = "";
        for (const line of chunk.split("\n")) {
          if (line.startsWith("event: ")) ev = line.slice(7);
          else if (line.startsWith("data: ")) data += line.slice(6);
        }
        if (data && state.page && state.page.onLive) {
          try { state.page.onLive(ev, JSON.parse(data)); } catch (e) { console.error(e); }
        }
      }
    }
  } catch (e) {
    if (e.name !== "AbortError") console.warn("stream ended", e);
  }
  if (state.stream === ctrl) {
    state.stream = null;
    if (state.live) setTimeout(startLive, 3000); // reconnect
  }
  setLiveIndicator(false);
}
function stopLive() {
  if (state.stream) state.stream.abort();
  state.stream = null;
  setLiveIndicator(false);
}
function setLiveIndicator(on) {
  const el = document.getElementById("live");
  if (el) {
    el.classList.toggle("on", on && state.live);
    el.lastChild.textContent = state.live ? (on ? "Live" : "Reconnecting…") : "Paused";
  }
}
function toggleLive() {
  state.live = !state.live;
  try { localStorage.setItem("fluid-live", state.live ? "1" : "0"); } catch { /* ignore */ }
  state.live ? startLive() : stopLive();
  setLiveIndicator(!!state.stream);
}


// ---------------------------------------------------------------- shell & router

ICONS.api = "M4 6h16v4H4zM4 14h16v4H4zM7 8h.01M7 16h.01";
ICONS.history = "M3 12a9 9 0 1 0 3-6.7M3 4v5h5M12 7v5l3 2";
ICONS.bolt = "M13 2 4 14h7l-1 8 9-12h-7z";
ICONS.upload = "M12 20V9M7 14l5-5 5 5M5 4h14";

const NAV = [
  { group: "Data", items: [
    { path: "/", label: "Overview", icon: "home" },
    { path: "/apis", label: "APIs", icon: "api", badge: "config" },
    { path: "/audit", label: "History", icon: "history" },
    { path: "/logs", label: "Request logs", icon: "logs" },
  ] },
  { group: "Settings", items: [
    { path: "/access", label: "Access (IP allowlists)", icon: "lock" },
    { path: "/system", label: "Status", icon: "gear" },
  ] },
];

const ROUTES = [
  [/^\/$/, () => pageOverview()],
  [/^\/apis$/, () => pageAPIs()],
  [/^\/apis\/new$/, () => pageNewAPI()],
  [/^\/apis\/([^/]+)\/([^/]+)$/, (m) => pageAPI(decodeURIComponent(m[1]), decodeURIComponent(m[2]))],
  [/^\/audit$/, () => pageAudit()],
  [/^\/logs$/, () => pageLogs()],
  [/^\/access$/, () => pageAccess()],
  [/^\/system$/, () => pageSystem()],
];

function navigate(path, replace) {
  if (replace) history.replaceState(null, "", BASE + path);
  else history.pushState(null, "", BASE + path);
  route();
}
window.addEventListener("popstate", () => route());
document.addEventListener("click", (e) => {
  const a = e.target.closest("a[data-link]");
  if (!a || e.metaKey || e.ctrlKey || e.shiftKey) return;
  e.preventDefault();
  navigate(a.dataset.link);
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") {
    const ov = document.querySelector(".overlay");
    if (ov) ov.remove(); else closeDetail();
  }
});

function link(path, ...children) { return h("a", { href: BASE + path, "data-link": path }, ...children); }

function renderShell() {
  const role = state.me.role;
  fill($app,
    h("div", { class: "shell", id: "shell" },
      h("header", { class: "topbar" },
        h("button", { class: "btn subtle menu-btn", "aria-label": "Menu",
          onclick: () => document.getElementById("shell").classList.toggle("nav-open") }, icon("menu")),
        link("/", h("span", { class: "logo" }, h("span", { class: "logo-mark" }, "F"), "fluid-config")),
        h("span", { class: "spacer" }),
        h("input", { class: "global-search", type: "search", placeholder: "Find an API…", "aria-label": "Find an API",
          onkeydown: (e) => { if (e.key === "Enter") navigate("/apis?f.key=" + encodeURIComponent(e.target.value)); } }),
        h("button", { class: "live", id: "live", title: "Toggle live updates", onclick: toggleLive },
          h("span", { class: "live-dot" }), h("span", {}, "Paused")),
        lozenge(role === "editor" ? "Editor" : "Viewer", role === "editor" ? "info" : ""),
        avatar(state.me.name),
        h("button", { class: "btn subtle", title: "Sign out", "aria-label": "Sign out", onclick: logout }, icon("logout"))),
      h("nav", { class: "sidebar", "aria-label": "Main" },
        h("div", { class: "project" }, h("div", { class: "project-icon" }),
          h("div", {}, h("div", { class: "project-name" }, "Config APIs"),
            h("div", { class: "project-sub" }, "Files · Records · History"))),
        NAV.map((g) => h("div", { class: "nav-group" }, h("div", { class: "nav-title" }, g.group),
          h("div", { class: "nav" }, g.items.map((it) => link(it.path, icon(it.icon), it.label,
            it.badge ? h("span", { class: "count", "data-badge": it.badge }) : null))))),
        h("div", { class: "nav-group", id: "nav-apis" })),
      h("main", { class: "main", id: "main" }),
      h("aside", { class: "detail", id: "detail", "aria-label": "Details" })));
  setLiveIndicator(!!state.stream);
}

async function route() {
  if (!state.me) return;
  closeDetail();
  closeModal();
  document.getElementById("shell")?.classList.remove("nav-open");
  const path = appPath();
  for (const a of document.querySelectorAll(".nav a")) {
    const p = a.dataset.link;
    a.classList.toggle("active", p === "/" ? path === "/" : p === "/apis" ? path === "/apis" || path === "/apis/new" : path.startsWith(p));
  }
  let m = null;
  const match = ROUTES.find(([re]) => (m = re.exec(path)));
  const main = document.getElementById("main");
  state.page = null;
  fill(main, h("div", { class: "page" }, h("div", { class: "muted" }, "Loading…")));
  try {
    const p = await (match ? match[1](m) : pageNotFound());
    state.page = p || null;
  } catch (e) {
    fill(main, h("div", { class: "page" }, banner("danger", "Could not load this page", e.message)));
  }
  refreshBadges();
}

async function refreshBadges() {
  try {
    const [st, apis] = await Promise.all([api("GET", "/api/status"), api("GET", "/api/apis")]);
    state.status = st;
    const el = document.querySelector('[data-badge="config"]');
    const n = Object.keys(st.config.errors || {}).length;
    if (el) fill(el, n ? h("span", { class: "badge danger", title: n + " invalid file(s)" }, String(n)) : h("span", { class: "badge" }, String(apis.length)));
    const nav = document.getElementById("nav-apis");
    const path = appPath();
    if (nav) fill(nav, apis.length ? [h("div", { class: "nav-title" }, "Your APIs"),
      h("div", { class: "nav" }, apis.slice(0, 30).map((a) => {
        const p = "/apis/" + a.version + "/" + a.name;
        const el = link(p, h("span", { class: "nav-api" + (a.enabled ? "" : " off") }, a.key),
          a.error ? h("span", { class: "count" }, h("span", { class: "badge danger" }, "!")) : null);
        if (path === p) el.classList.add("active");
        return el;
      }))] : null);
  } catch { /* ignore */ }
}

function page(title, opts, ...body) {
  const main = document.getElementById("main");
  const el = h("div", { class: "page" },
    h("div", { class: "crumbs" }, link("/apis", "APIs"), opts.crumb ? [" / ", opts.crumb] : null),
    h("div", { class: "page-head" }, h("h1", {}, title), opts.actions || null),
    opts.desc ? h("p", { class: "page-desc" }, opts.desc) : null,
    ...body);
  fill(main, el);
  document.title = title + " · fluid-config";
  return el;
}

function banner(kind, title, detail, prose) {
  return h("div", { class: "banner " + kind }, h("div", {}, h("strong", {}, title),
    detail ? (prose ? h("div", {}, detail) : h("pre", {}, detail)) : null));
}

function emptyState(title, text, action) {
  return h("div", { class: "empty" }, h("h3", {}, title), h("div", {}, text), action ? h("div", { class: "more" }, action) : null);
}

function viewerNote() {
  return isEditor() ? null : banner("info", "View only", "You are signed in with a viewer token. Secrets are hidden and changes are disabled; ask for an editor token to change configuration.", true);
}


// ---------------------------------------------------------------- detail panel

function openDetail(keyText, title, ...body) {
  const d = document.getElementById("detail");
  fill(d, 
    h("div", { class: "detail-head" }, h("span", { class: "key" }, keyText), h("span", { class: "spacer" }),
      h("button", { class: "btn subtle", "aria-label": "Close", onclick: closeDetail }, icon("close"))),
    h("div", { class: "detail-body" }, h("h2", {}, title), ...body));
  d.classList.add("open");
  d.querySelector("button").focus();
}
function closeDetail() {
  const d = document.getElementById("detail");
  if (d) d.classList.remove("open");
  for (const r of document.querySelectorAll("tr.selected")) r.classList.remove("selected");
}
function props(pairs) {
  return h("dl", { class: "props" }, pairs.filter(Boolean).map(([k, v]) => [h("dt", {}, k), h("dd", {}, v ?? "—")]));
}

// ---------------------------------------------------------------- data table
//
// dataTable({ id, cols, fetch | rows, onRow, empty, pageSize, defaultSort })
//   col: { key, label, render, v (value for client-side sort/filter), sort, filter, num, time }
//   filter: "text" | "num" | "time" | [[value, label], ...] (exact match)
//   fetch({offset, limit, sort, filters}) -> {items, total}   (server-side paging)
//   rows: [...]                                               (client-side paging)
// Click a header to sort, Shift+click to add a secondary sort. State lives in the URL.

const PAGE_SIZES = [10, 25, 50, 100, 200];
const TIME_PRESETS = [["", "Any time"], ["15m..", "Last 15 min"], ["1h..", "Last hour"], ["24h..", "Last 24 h"], ["7d..", "Last 7 days"], ["30d..", "Last 30 days"], ["custom", "Custom range…"]];
const DUR = { s: 1e3, m: 6e4, h: 36e5, d: 864e5 };

function parseInstantJS(s) {
  const m = /^(\d+)([smhd])$/.exec(s);
  if (m) return Date.now() - m[1] * DUR[m[2]];
  const t = Date.parse(s);
  return isNaN(t) ? null : t;
}
function matchTime(ts, f) {
  const t = new Date(ts).getTime();
  if (!validTime(ts)) return false;
  const [a, b] = f.includes("..") ? f.split("..") : [f, ""];
  const lo = a ? parseInstantJS(a) : null, hi = b ? parseInstantJS(b) : null;
  return (lo === null || t >= lo) && (hi === null || t < hi);
}
function matchNum(v, f) {
  if (v === "" || v === null || v === undefined) return false;
  v = Number(v); f = f.replace(/\s/g, "");
  let m;
  if ((m = /^([1-9])xx$/i.exec(f))) return v >= m[1] * 100 && v < m[1] * 100 + 100;
  if ((m = /^(>=|<=|>|<|=)(-?\d+(?:\.\d+)?)$/.exec(f))) {
    const n = Number(m[2]);
    return { ">=": v >= n, "<=": v <= n, ">": v > n, "<": v < n, "=": v === n }[m[1]];
  }
  if ((m = /^(\d+)-(\d+)$/.exec(f))) return v >= +m[1] && v <= +m[2];
  return String(v) === f;
}
function cellValue(c, r) { const v = c.v ? c.v(r) : r[c.key]; return v === undefined || v === null ? "" : v; }
function cmp(a, b) {
  if (typeof a === "number" && typeof b === "number") return a - b;
  const ta = Date.parse(a), tb = Date.parse(b);
  if (typeof a === "string" && typeof b === "string" && /^\d{4}-\d\d-\d\dT/.test(a) && /^\d{4}-\d\d-\d\dT/.test(b)) return ta - tb;
  return String(a).localeCompare(String(b), undefined, { numeric: true, sensitivity: "base" });
}
// datetime-local input value <-> ISO (UTC) string
function localInputToISO(v) { return v ? new Date(v).toISOString() : ""; }
function isoToLocalInput(s) {
  const d = validTime(s); if (!d) return "";
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

function dataTable(o) {
  const P = o.id ? o.id + "." : "";
  const url = new URLSearchParams(location.search);
  const st = {
    page: Math.max(1, parseInt(url.get(P + "page"), 10) || 1),
    size: PAGE_SIZES.includes(+url.get(P + "size")) ? +url.get(P + "size") : (o.pageSize || 25),
    sort: (url.get(P + "sort") ?? (o.defaultSort || []).join(",")).split(",").filter(Boolean)
      .map((x) => { const [k, d] = x.split(":"); return { k, d: d === "desc" ? "desc" : "asc" }; }),
    filters: { ...(o.initialFilters || {}) },
  };
  for (const c of o.cols) if (c.filter) { const v = url.get(P + "f." + c.key); if (v) st.filters[c.key] = v; }
  let data = { items: [], total: 0 }, seq = 0;

  const save = () => {
    const q = new URLSearchParams(location.search);
    for (const k of [...q.keys()]) if (k.startsWith(P + "f.") || [P + "page", P + "size", P + "sort"].includes(k)) q.delete(k);
    if (st.page > 1) q.set(P + "page", st.page);
    if (st.size !== (o.pageSize || 25)) q.set(P + "size", st.size);
    const srt = st.sort.map((x) => x.k + ":" + x.d).join(",");
    if (srt !== (o.defaultSort || []).join(",")) q.set(P + "sort", srt);
    for (const [k, v] of Object.entries(st.filters)) if (v) q.set(P + "f." + k, v);
    const qs = q.toString();
    history.replaceState(null, "", location.pathname + (qs ? "?" + qs : ""));
  };

  // header: labels (sortable) + one filter control per column, built once
  const thFor = new Map();
  const headRow = h("tr", {}, o.cols.map((c) => {
    const label = [c.label, c.time ? h("span", { class: "tz", title: "Shown in your time zone: " + TZ }, TZ_SHORT) : null];
    if (!c.sort) return h("th", { class: c.num ? "num" : "" }, label);
    const ind = h("span", { class: "sort-ind" });
    const th = h("th", { class: "sortable" + (c.num ? " num" : ""), tabindex: "0", "aria-sort": "none",
      title: "Click to sort, Shift+click to add a secondary sort" }, label, ind);
    const toggle = (multi) => {
      const i = st.sort.findIndex((x) => x.k === c.key);
      if (!multi) {
        const cur = i >= 0 ? st.sort[i].d : null;
        st.sort = cur === null ? [{ k: c.key, d: c.time || c.num ? "desc" : "asc" }] : cur === (c.time || c.num ? "desc" : "asc")
          ? [{ k: c.key, d: c.time || c.num ? "asc" : "desc" }] : [];
      } else if (i < 0) st.sort.push({ k: c.key, d: c.time || c.num ? "desc" : "asc" });
      else if (st.sort[i].d === (c.time || c.num ? "desc" : "asc")) st.sort[i].d = st.sort[i].d === "asc" ? "desc" : "asc";
      else st.sort.splice(i, 1);
      st.page = 1; load();
    };
    th.addEventListener("click", (e) => toggle(e.shiftKey));
    th.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggle(e.shiftKey); } });
    thFor.set(c.key, { th, ind });
    return th;
  }));
  let timer;
  const setFilter = (k, v, now) => {
    clearTimeout(timer);
    const apply = () => { if ((st.filters[k] || "") === v) return; if (v) st.filters[k] = v; else delete st.filters[k]; st.page = 1; load(); };
    if (now) apply(); else timer = setTimeout(apply, 350);
  };
  const filterCell = (c) => {
    if (!c.filter) return h("th", {});
    const cur = st.filters[c.key] || "";
    if (Array.isArray(c.filter)) {
      return h("th", {}, select([["", "All"], ...c.filter.map(([v, t]) => ["=" + v, t])], cur, (v) => setFilter(c.key, v, true), "Filter " + c.label));
    }
    if (c.filter === "time") {
      const from = h("input", { type: "datetime-local", "aria-label": c.label + " from" });
      const to = h("input", { type: "datetime-local", "aria-label": c.label + " to" });
      const custom = h("div", { class: "dt-range hidden" }, from, h("span", { class: "muted" }, "to"), to);
      const isPreset = TIME_PRESETS.some(([v]) => v === cur);
      const sel = select(TIME_PRESETS, isPreset ? cur : cur ? "custom" : "", (v) => {
        custom.classList.toggle("hidden", v !== "custom");
        if (v !== "custom") setFilter(c.key, v, true); else from.focus();
      }, "Filter " + c.label);
      if (cur && !isPreset) {
        const [a, b] = cur.split("..");
        from.value = isoToLocalInput(a); to.value = isoToLocalInput(b);
        custom.classList.remove("hidden");
      }
      const applyRange = () => {
        const a = localInputToISO(from.value), b = localInputToISO(to.value);
        setFilter(c.key, a || b ? a + ".." + b : "", true);
      };
      from.addEventListener("change", applyRange); to.addEventListener("change", applyRange);
      return h("th", { class: "f-time" }, sel, custom);
    }
    const inp = h("input", { type: "search", value: cur, "aria-label": "Filter " + c.label,
      placeholder: c.filter === "num" ? "200, 5xx, >=400" : "Filter…",
      title: c.filter === "num" ? "A number, 5xx, >=400, <100 or 200-299" : "Contains (case-insensitive); start with = for an exact match" });
    inp.addEventListener("input", () => setFilter(c.key, inp.value.trim()));
    inp.addEventListener("keydown", (e) => { if (e.key === "Enter") setFilter(c.key, inp.value.trim(), true); });
    return h("th", {}, inp);
  };
  const hasFilters = o.cols.some((c) => c.filter);
  const tbody = h("tbody", {});
  const tableEl = h("table", { class: "issues" }, h("thead", {}, headRow, hasFilters ? h("tr", { class: "filters" }, o.cols.map(filterCell)) : null), tbody);
  const pager = h("div", { class: "pager" });
  const chips = h("div", { class: "chips" });
  const exportBtn = h("button", { class: "btn sm", title: "Download every row matching the current search, filters and sort (not just this page)" }, icon("download"), "Export CSV");
  exportBtn.addEventListener("click", async () => {
    exportBtn.disabled = true;
    try {
      if (o.fetch) {
        const q = o.exportParams ? o.exportParams() : new URLSearchParams();
        const srt = st.sort.map((x) => x.k + ":" + x.d).join(",");
        if (srt) q.set("sort", srt);
        for (const [k, v] of Object.entries(st.filters)) if (v) q.set("f." + k, v);
        q.set("format", "csv"); q.set("tz", TZ);
        await download(o.exportPath + "?" + q, (o.exportName || "export") + ".csv");
      } else {
        const cols = o.cols.filter((c) => c.key && c.label);
        const rows = clientRows();
        const lines = [cols.map((c) => csvCell(c.label)).join(",")].concat(rows.map((r) => cols.map((c) => {
          const v = c.csv ? c.csv(r) : cellValue(c, r);
          return csvCell(c.time ? isoLocal(v) : v);
        }).join(",")));
        saveBlob(new Blob(["\ufeff" + lines.join("\r\n") + "\r\n"], { type: "text/csv;charset=utf-8" }), `fluid-${o.exportName || "export"}-${stamp()}.csv`);
      }
      toast("Exported " + data.total.toLocaleString() + " row" + (data.total === 1 ? "" : "s"));
    } catch (e) { toast(e.message, true); }
    finally { exportBtn.disabled = false; }
  });
  const el = h("div", { class: "data-table" }, h("div", { class: "dt-bar" }, chips, exportBtn), h("div", { class: "table-wrap" }, tableEl), pager);

  const clientPage = () => {
    const rows = clientRows();
    return { items: rows.slice((st.page - 1) * st.size, st.page * st.size), total: rows.length };
  };
  function clientRows() {
    let rows = o.rows.slice();
    for (const [k, f] of Object.entries(st.filters)) {
      const c = o.cols.find((x) => x.key === k); if (!c) continue;
      rows = rows.filter((r) => {
        const v = cellValue(c, r);
        if (c.filter === "time") return matchTime(v, f);
        if (c.filter === "num") return matchNum(v, f);
        if (f.startsWith("=")) return String(v) === f.slice(1);
        return String(Array.isArray(v) ? v.join(" ") : v).toLowerCase().includes(f.toLowerCase());
      });
    }
    if (st.sort.length) {
      const cs = st.sort.map((x) => [o.cols.find((c) => c.key === x.k), x.d === "desc" ? -1 : 1]).filter(([c]) => c);
      rows.sort((a, b) => { for (const [c, dir] of cs) { const r = cmp(cellValue(c, a), cellValue(c, b)); if (r) return r * dir; } return 0; });
    }
    return rows;
  }

  const load = async () => {
    save();
    const my = ++seq;
    try {
      data = o.fetch
        ? await o.fetch({ offset: (st.page - 1) * st.size, limit: st.size, sort: st.sort.map((x) => x.k + ":" + x.d).join(","), filters: st.filters })
        : clientPage();
    } catch (e) { if (my === seq) { fill(tbody, h("tr", {}, h("td", { colspan: String(o.cols.length) }, banner("danger", "Could not load", e.message)))); } return; }
    if (my !== seq) return;
    const pages = Math.max(1, Math.ceil(data.total / st.size));
    if (st.page > pages && data.total > 0) { st.page = pages; return load(); }
    draw();
  };

  const draw = () => {
    for (const [k, { th, ind }] of thFor) {
      const i = st.sort.findIndex((x) => x.k === k);
      th.classList.toggle("sorted", i >= 0);
      th.setAttribute("aria-sort", i < 0 ? "none" : st.sort[i].d === "asc" ? "ascending" : "descending");
      fill(ind, i < 0 ? "↕" : [st.sort[i].d === "asc" ? "▲" : "▼", st.sort.length > 1 ? h("sup", {}, String(i + 1)) : null]);
    }
    const active = Object.entries(st.filters).filter(([, v]) => v);
    fill(chips, active.length || st.sort.length ? [
      active.map(([k, v]) => {
        const c = o.cols.find((x) => x.key === k);
        return h("span", { class: "chip" }, h("strong", {}, (c ? c.label : k) + ": "), chipText(c, v),
          h("button", { class: "chip-x", "aria-label": "Remove filter", onclick: () => { delete st.filters[k]; st.page = 1; resetFilterInputs(); load(); } }, "×"));
      }),
      st.sort.length ? h("span", { class: "chip muted" }, "Sorted by ", st.sort.map((x, i) => {
        const c = o.cols.find((y) => y.key === x.k);
        return (i ? ", " : "") + (c ? c.label : x.k) + (x.d === "asc" ? " ↑" : " ↓");
      })) : null,
      h("button", { class: "btn subtle sm", onclick: () => { st.filters = {}; st.sort = (o.defaultSort || []).map((x) => { const [k, d] = x.split(":"); return { k, d }; }); st.page = 1; resetFilterInputs(); load(); } }, "Clear all"),
    ] : null);
    fill(tbody, data.items.length ? data.items.map((r) => tableRow(o.cols, r, o.onRow))
      : h("tr", {}, h("td", { colspan: String(o.cols.length), class: "empty-cell" },
        emptyState(active.length ? "Nothing matches these filters" : (o.empty || ["Nothing here yet", ""])[0],
          active.length ? "Change or clear the column filters." : (o.empty || ["", ""])[1]))));
    drawPager();
    o.onDraw && o.onDraw(data);
  };
  const chipText = (c, v) => {
    if (c && c.filter === "time") {
      const p = TIME_PRESETS.find(([x]) => x === v); if (p) return p[1];
      const [a, b] = v.split("..");
      return (a ? fmtDT(a) : "…") + " → " + (b ? fmtDT(b) : "now");
    }
    if (c && Array.isArray(c.filter)) { const f = c.filter.find(([x]) => "=" + x === v); if (f) return f[1]; }
    return v;
  };
  const resetFilterInputs = () => {
    const row = tableEl.querySelector("tr.filters");
    if (row) row.replaceWith(h("tr", { class: "filters" }, o.cols.map(filterCell)));
  };

  const drawPager = () => {
    const total = data.total, pages = Math.max(1, Math.ceil(total / st.size));
    const from = total ? (st.page - 1) * st.size + 1 : 0, to = Math.min(total, st.page * st.size);
    const go = (p) => { p = Math.min(pages, Math.max(1, p)); if (p !== st.page) { st.page = p; load(); } };
    const btn = (label, p, disabled, cls, aria) => h("button", { class: "btn sm " + (cls || ""), disabled: disabled ? true : undefined,
      "aria-label": aria || label, "aria-current": cls === "on" ? "page" : undefined, onclick: () => go(p) }, label);
    const nums = [];
    const win = new Set([1, pages, st.page - 2, st.page - 1, st.page, st.page + 1, st.page + 2].filter((p) => p >= 1 && p <= pages));
    let prev = 0;
    for (const p of [...win].sort((a, b) => a - b)) {
      if (p - prev > 1) nums.push(h("span", { class: "gap" }, "…"));
      nums.push(btn(String(p), p, false, p === st.page ? "on" : "", "Page " + p));
      prev = p;
    }
    const jump = h("input", { type: "number", min: "1", max: String(pages), value: String(st.page), class: "jump", "aria-label": "Go to page" });
    jump.addEventListener("keydown", (e) => { if (e.key === "Enter") go(parseInt(jump.value, 10) || 1); });
    fill(pager,
      h("div", { class: "pager-info" }, total ? `Showing ${from.toLocaleString()}–${to.toLocaleString()} of ${total.toLocaleString()}` : "No results",
        h("span", { class: "muted" }, " · times in " + TZ)),
      h("div", { class: "pager-ctl" },
        h("label", { class: "muted" }, "Rows ", select(PAGE_SIZES.map((n) => [String(n), String(n)]), String(st.size),
          (v) => { st.size = +v; st.page = 1; load(); }, "Rows per page")),
        btn("«", 1, st.page <= 1, "", "First page"), btn("‹", st.page - 1, st.page <= 1, "", "Previous page"),
        nums,
        btn("›", st.page + 1, st.page >= pages, "", "Next page"), btn("»", pages, st.page >= pages, "", "Last page"),
        h("label", { class: "muted" }, "Go to ", jump, " / " + pages)));
  };

  load();
  return {
    el,
    reload: load,
    setRows(rows) { o.rows = rows; load(); },
    get page() { return st.page; },
    get filtered() { return Object.values(st.filters).some(Boolean); },
    get sorted() { return st.sort.map((x) => x.k + ":" + x.d).join(",") !== (o.defaultSort || []).join(","); },
    query() { // the table's filters as API params (e.g. for a summary of the same rows)
      const q = new URLSearchParams();
      for (const [k, v] of Object.entries(st.filters)) if (v) q.set("f." + k, v);
      return q;
    },
  };
}

// serverList pages an /api list endpoint; extra() adds the page's own params.
function serverList(path, extra) {
  return async ({ offset, limit, sort, filters }) => {
    const q = extra ? extra() : new URLSearchParams();
    q.set("offset", String(offset)); q.set("limit", String(limit));
    if (sort) q.set("sort", sort);
    for (const [k, v] of Object.entries(filters)) if (v) q.set("f." + k, v);
    return api("GET", path + "?" + q);
  };
}

// ---------------------------------------------------------------- table

function table(cols, rows, onRow) {
  if (!rows.length) return null;
  return h("div", { class: "table-wrap" }, h("table", { class: "issues" },
    h("thead", {}, h("tr", {}, cols.map((c) => h("th", { class: c.num ? "num" : "" }, c.label)))),
    h("tbody", {}, rows.map((r) => tableRow(cols, r, onRow)))));
}
function tableRow(cols, r, onRow) {
  const tr = h("tr", { tabindex: "0" }, cols.map((c) => h("td", { class: c.num ? "num" : "" }, c.render(r))));
  if (onRow) {
    const open = () => {
      for (const s of document.querySelectorAll("tr.selected")) s.classList.remove("selected");
      tr.classList.add("selected");
      onRow(r);
    };
    tr.addEventListener("click", open);
    tr.addEventListener("keydown", (e) => { if (e.key === "Enter") open(); });
  }
  return tr;
}

function select(options, value, onchange, label) {
  return h("select", { "aria-label": label, onchange: (e) => onchange(e.target.value) },
    options.map(([v, t]) => h("option", { value: v, selected: v === value ? true : undefined }, t)));
}
function seg(options, value, onchange) {
  return h("div", { class: "seg", role: "group" }, options.map(([v, t]) =>
    h("button", { class: v === value ? "on" : "", "aria-pressed": v === value ? "true" : "false", onclick: () => onchange(v) }, t)));
}

function queryState(defaults) {
  const q = new URLSearchParams(location.search);
  const s = { ...defaults };
  for (const k of Object.keys(defaults)) if (q.has(k)) s[k] = q.get(k);
  return s;
}
function pushQuery(s, defaults) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(s)) if (v && v !== defaults[k]) q.set(k, v);
  const qs = q.toString();
  history.replaceState(null, "", location.pathname + (qs ? "?" + qs : ""));
}
const SINCE = [["15m", "Last 15 min"], ["1h", "Last hour"], ["24h", "Last 24 hours"], ["7d", "Last 7 days"], ["", "All time"]];


// payloadView shows a captured exchange: RQH, RQB, RSH, RSB.
function payloadView(p, masked, l) {
  if (!p) return null;
  const headerText = (hs) => Object.keys(hs || {}).sort((a, b) => (a[0] === ":") === (b[0] === ":") ? a.localeCompare(b) : a[0] === ":" ? -1 : 1)
    .map((k) => hs[k].map((v) => k + ": " + v).join("\n")).join("\n");
  const bodyText = (part) => part.body ? pretty(part.body) : part.body_base64 ? "(binary, base64)\n" + part.body_base64 : "";
  const sizeNote = (part) => {
    if (!part.size) return "empty";
    return bytes(part.size) + (part.truncated ? ` — first ${bytes(part.stored)} stored (capture_max_body_bytes)` : "");
  };
  const block = (code, title, text, note) => {
    const pre = h("pre", { class: "code payload" }, text || "(none)");
    return h("section", { class: "payload-part" },
      h("div", { class: "payload-head" }, h("span", { class: "pl-code" }, code), h("strong", {}, title),
        note ? h("span", { class: "muted" }, note) : null, h("span", { class: "spacer" }),
        text ? h("button", { class: "btn sm subtle", onclick: () => navigator.clipboard?.writeText(text).then(() => toast("Copied " + code)) }, "Copy") : null),
      pre);
  };
  const out = [h("h3", {}, "Request received and response sent")];
  if (masked) out.push(banner("info", "Credentials are masked for viewer tokens", "Authorization, cookies, tokens and other known secrets show as ********. Editors see the exact values.", true));
  if (p.request) {
    out.push(block("RQH", "Request headers", headerText(p.request.headers)));
    out.push(block("RQB", "Request body", bodyText(p.request), sizeNote(p.request)));
  }
  if (p.response) {
    out.push(block("RSH", "Response headers", headerText(p.response.headers)));
    out.push(block("RSB", "Response body", bodyText(p.response), sizeNote(p.response)));
  } else if (p.request) {
    out.push(h("p", { class: "muted" }, "No response was received (see the error above)."));
  }
  return out;
}

// legacyFilters turns old-style links (?route=x&status=y) into column filters.
function legacyFilters(map) {
  const q = new URLSearchParams(location.search);
  const out = {};
  for (const [k, fn] of Object.entries(map)) {
    const v = q.get(k);
    if (v) { const [col, val] = fn(v); out[col] = val; q.delete(k); }
  }
  if (Object.keys(out).length) { const qs = q.toString(); history.replaceState(null, "", location.pathname + (qs ? "?" + qs : "")); }
  return out;
}
const exact = (col) => (v) => [col, "=" + v];


async function showLog(l) {
  const meta = props([["Time", fmtDT(l.ts)], ["Credential", l.client || null], ["API", l.route || null], ["Method", l.method || null],
    ["URL", l.url ? h("span", { class: "mono" }, l.url) : null], ["Status", l.status ? httpLozenge(l.status) : null],
    ["Duration", l.duration_ms ? l.duration_ms + " ms" : null], ["Bytes in / out", (l.bytes_in || l.bytes_out) ? bytes(l.bytes_in) + " / " + bytes(l.bytes_out) : null],
    ["Source IP", l.source_ip || null]]);
  const payloadEl = h("div", {}, l.has_payload ? h("div", { class: "muted" }, "Loading request and response…") : null);
  openDetail("#" + l.id, l.msg,
    h("div", { class: "toolbar" }, lozenge(l.level, STATUS_KIND[l.level]), h("span", { class: "mono" }, l.kind)),
    meta,
    l.route && l.kind === "api" ? h("p", {}, link(uiPath(l.route), "Open API " + l.route),
      l.attrs && l.attrs.request_id && /^(POST|PUT|PATCH|DELETE)$/.test(l.method) ? [" · ", link("/audit?f.request_id==" + encodeURIComponent(l.attrs.request_id), "Changes made by this request")] : null) : null,
    payloadEl,
    l.attrs ? [h("h3", {}, "Attributes"), h("pre", { class: "code" }, JSON.stringify(l.attrs, null, 2))] : null);
  if (!l.has_payload) return;
  try {
    const d = await api("GET", "/api/logs/" + l.id);
    fill(payloadEl, payloadView(d.payload, d.masked, l));
  } catch (e) { fill(payloadEl, banner("danger", "Could not load the captured request", e.message)); }
}

// ---------------------------------------------------------------- logs

const KINDS = ["api", "access", "admin", "config", "deploy", "system", "app"];

async function pageLogs() {
  const initialFilters = legacyFilters({ kind: exact("kind"), route: exact("route"), client: exact("client"),
    source_ip: exact("source_ip"), status: (v) => ["status", v === "error" ? ">=400" : v], since: (v) => ["ts", v + ".."] });
  if (!Object.keys(initialFilters).length && !location.search.includes("f.ts")) initialFilters.ts = "1h..";
  const defaults = { view: "list", level: "", q: "", by: "route" };
  const f = queryState(defaults);
  const cols = [
    { key: "ts", label: "Time", time: true, sort: true, filter: "time", render: (l) => when(l.ts) },
    { key: "level", label: "Level", sort: true, filter: [["debug", "debug"], ["info", "info"], ["warn", "warn"], ["error", "error"]], render: (l) => lozenge(l.level, STATUS_KIND[l.level]) },
    { key: "kind", label: "Kind", sort: true, filter: KINDS.map((k) => [k, k]), render: (l) => h("span", { class: "mono" }, l.kind) },
    { key: "route", label: "API", sort: true, filter: "text", render: (l) => l.route || h("span", { class: "muted" }, "—") },
    { key: "client", label: "Who", sort: true, filter: "text", render: (l) => l.client ? h("span", { class: "cell-flex" }, avatar(l.client, true), h("span", {}, l.client)) : h("span", { class: "muted" }, "—") },
    { key: "method", label: "Method", sort: true, filter: "text", render: (l) => l.method || "" },
    { key: "status", label: "Status", num: true, sort: true, filter: "num", render: (l) => httpLozenge(l.status) },
    { key: "duration_ms", label: "ms", num: true, sort: true, filter: "num", render: (l) => l.duration_ms ? String(l.duration_ms) : "" },
    { key: "source_ip", label: "Source IP", sort: true, filter: "text", render: (l) => h("span", { class: "mono" }, l.source_ip || "") },
    { key: "msg", label: "URL / message", sort: true, filter: "text", render: (l) => h("span", { class: "cell-flex", title: l.url || l.msg },
      l.has_payload ? h("span", { class: "pl-flag", title: "Full request and response captured" }, "RQ/RS") : null, h("span", {}, l.url || l.msg)) },
  ];
  const pageParams = () => {
    const q = new URLSearchParams();
    for (const k of ["level", "q"]) if (f[k]) q.set(k, f[k]);
    return q;
  };
  const body = h("div", {});
  let t = null;
  const drawSummary = async () => {
    const q = pageParams();
    for (const [k, v] of new URLSearchParams(location.search)) if (k.startsWith("f.")) q.set(k, v);
    for (const [k, v] of Object.entries(initialFilters)) if (!q.has("f." + k)) q.set("f." + k, v);
    q.set("by", f.by);
    const r = await api("GET", "/api/logs/summary?" + q);
    const rows = r.items, max = rows.length ? rows[0].count : 1;
    const st = dataTable({
      rows, pageSize: 25, defaultSort: ["count:desc"], exportName: "logs-summary-by-" + f.by,
      cols: [
        { key: "key", label: f.by, sort: true, filter: "text", render: (x) => h("div", {}, h("div", {}, x.key || "—"),
          (() => { const b = h("i", {}); b.style.width = Math.max(2, (x.count / max) * 100) + "%"; return h("div", { class: "bar" }, b); })()) },
        { key: "count", label: "Count", num: true, sort: true, filter: "num", render: (x) => String(x.count) },
        { key: "errors", label: "Errors", num: true, sort: true, filter: "num", render: (x) => x.errors ? lozenge(String(x.errors), "danger") : "0" },
        { key: "avg_ms", label: "Avg ms", num: true, sort: true, filter: "num", render: (x) => x.avg_ms.toFixed(0) },
        { key: "bytes_out", label: "Bytes out", num: true, sort: true, filter: "num", render: (x) => bytes(x.bytes_out) },
        { key: "last_seen", label: "Last seen", time: true, sort: true, filter: "time", render: (x) => when(x.last_seen) },
      ],
      onRow: (x) => {
        const col = cols.find((c) => c.key === f.by);
        if (!col || !col.filter) return;
        const q = new URLSearchParams(location.search);
        q.set("f." + f.by, col.filter === "num" ? String(x.key) : "=" + x.key); q.delete("view"); q.delete("by");
        history.replaceState(null, "", location.pathname + "?" + q); route();
      },
      empty: ["Nothing to summarize", "No log entries match these filters."],
    });
    fill(body, banner("info", "Summary of the rows matching the list's filters", "Switch to List to change filters; click a row to list its entries.", true), st.el);
  };
  if (f.view === "summary") await drawSummary();
  else {
    t = dataTable({ cols, initialFilters, defaultSort: ["ts:desc"], pageSize: 50, fetch: serverList("/api/logs", pageParams), onRow: showLog, exportPath: "/api/logs", exportName: "logs", exportParams: pageParams,
      empty: ["No log entries", "Try a wider time range or fewer filters."] });
    fill(body, t.el);
  }
  const reload = () => (t ? t.reload() : drawSummary());
  const setPage = (k, v) => { f[k] = v; pushQuery(f, defaults); reload(); };
  page("Logs", {
    desc: "Every API call with its full request and response, plus console, config and system events, stored in SQLite. Filter any column; Shift+click headers to sort by several.",
    actions: h("button", { class: "btn", onclick: reload }, icon("refresh"), "Refresh"),
  },
    h("div", { class: "toolbar" },
      seg([["list", "List"], ["summary", "Summary"]], f.view, (v) => { f.view = v; pushQuery(f, defaults); route(); }),
      select([["", "Any level"], ["info", "Info+"], ["warn", "Warn+"], ["error", "Error"]], f.level, (v) => setPage("level", v), "Minimum level"),
      h("input", { type: "search", placeholder: "Search message, URL, attributes…", value: f.q, "aria-label": "Search", onchange: (e) => setPage("q", e.target.value) }),
      f.view === "summary" ? select([["route", "Group by API"], ["kind", "Group by kind"],
        ["client", "Group by credential"], ["status", "Group by status"], ["source_ip", "Group by source IP"],
        ["level", "Group by level"], ["method", "Group by method"]], f.by, (v) => setPage("by", v), "Group by") : null),
    body);
  return {
    onLive(topic) {
      if (topic !== "logs" || !t || t.page !== 1) return;
      clearTimeout(this.timer); this.timer = setTimeout(reload, 1000);
    },
  };
}


// ---------------- generic form <-> object editing

function getPath(o, path) { return path.split(".").reduce((a, k) => (a == null ? undefined : a[k]), o); }
function setPath(o, path, v) {
  const ks = path.split(".");
  let cur = o;
  for (const k of ks.slice(0, -1)) {
    if (typeof cur[k] !== "object" || cur[k] === null) cur[k] = {};
    cur = cur[k];
  }
  cur[ks[ks.length - 1]] = v;
}
// prune drops empty strings, arrays, objects and false, so unset form fields mean "default".
function prune(v) {
  if (Array.isArray(v)) return v.length ? v : undefined;
  if (v && typeof v === "object") {
    const out = {};
    for (const [k, x] of Object.entries(v)) {
      const p = prune(x);
      if (p !== undefined) out[k] = p;
    }
    return Object.keys(out).length ? out : undefined;
  }
  if (v === "" || v === false || v === null || (typeof v === "number" && isNaN(v))) return undefined;
  return v;
}

function fieldInput(f, obj, isNew) {
  const v = getPath(obj, f.k);
  const set = (x) => setPath(obj, f.k, x);
  const disabled = f.create && !isNew;
  let input;
  switch (f.type) {
    case "bool":
      return h("label", { class: "check" }, h("input", { type: "checkbox", checked: !!v, onchange: (e) => set(e.target.checked) }), f.label);
    case "number":
      input = h("input", { type: "number", value: v ?? "", onchange: (e) => set(e.target.value === "" ? "" : Number(e.target.value)) });
      break;
    case "list":
      input = h("textarea", { rows: "3", onchange: (e) => set(e.target.value.split("\n").map((s) => s.trim()).filter(Boolean)) });
      input.value = (v || []).join("\n");
      break;
    case "kv":
      input = h("textarea", { rows: "3", onchange: (e) => {
        const m = {};
        for (const line of e.target.value.split("\n")) {
          const i = line.indexOf(":");
          if (i > 0) m[line.slice(0, i).trim()] = line.slice(i + 1).trim();
        }
        set(m);
      } });
      input.value = Object.entries(v || {}).map(([k, x]) => k + ": " + x).join("\n");
      break;
    case "textarea":
      input = h("textarea", { rows: "3", onchange: (e) => set(e.target.value) });
      input.value = v ?? "";
      break;
    case "select":
      input = select(f.options, v ?? "", set, f.label);
      break;
    case "secret":
      input = h("input", { type: "password", autocomplete: "new-password", value: v ?? "", placeholder: v === MASK ? "unchanged" : "",
        onchange: (e) => set(e.target.value) });
      break;
    default:
      input = h("input", { type: "text", value: v ?? "", placeholder: f.placeholder || "", disabled: disabled || undefined,
        onchange: (e) => set(e.target.value) });
  }
  return h("label", { class: "field" }, h("span", {}, f.label), input,
    f.help ? h("div", { class: "help" }, f.help) : null,
    f.type === "secret" && v === MASK ? h("div", { class: "secret-note" }, "Stored secret is hidden. Leave as is to keep it.") : null);
}

function modal(title, body, foot) {
  closeModal();
  const ov = h("div", { class: "overlay", role: "dialog", "aria-modal": "true", "aria-label": title },
    h("div", { class: "modal" },
      h("div", { class: "modal-head" }, h("h2", {}, title), h("button", { class: "btn subtle", "aria-label": "Close", onclick: closeModal }, icon("close"))),
      h("div", { class: "modal-body" }, body),
      h("div", { class: "modal-foot" }, foot)));
  ov.addEventListener("mousedown", (e) => { if (e.target === ov) closeModal(); });
  document.body.append(ov);
  const first = ov.querySelector("input:not([disabled]), textarea, select");
  (first || ov.querySelector("button")).focus();
}
function closeModal() { document.querySelector(".overlay")?.remove(); }


// ---------------------------------------------------------------- fluid-config helpers

const apiPath = (key) => "/api/apis/" + key.split("/").map(encodeURIComponent).join("/");
const uiPath = (key) => "/apis/" + key;
const ACTION_KIND = { create: "success", import: "info", replace: "info", patch: "info", delete: "danger", revert: "purple",
  api_create: "success", api_update: "warn", api_delete: "danger" };
function actionLozenge(a) { return lozenge(a.replace("api_", "api "), ACTION_KIND[a] || ""); }
function viaTag(v) { return lozenge(v, v === "file" ? "warn" : v === "api" ? "info" : "purple"); }
function shortJSON(v, n) {
  const s = typeof v === "string" ? v : JSON.stringify(v);
  return s && s.length > (n || 80) ? s.slice(0, n || 80) + "…" : s;
}
function valueCell(v, col) {
  if (v === undefined || v === null) return h("span", { class: "muted" }, "—");
  if (typeof v === "boolean") return lozenge(String(v), v ? "success" : "");
  if (typeof v === "object") return h("span", { class: "mono", title: JSON.stringify(v, null, 2) }, shortJSON(v, 60));
  if (col && col.enum) return lozenge(String(v), "info");
  return h("span", { title: String(v) }, shortJSON(String(v), 80));
}
function ipsCell(a) {
  if (a.default) return lozenge("private only", "warn");
  if (a.allowed_ips.includes("*")) return lozenge("anyone", a.auth === "none" && a.methods.some((m) => m !== "GET") ? "danger" : "");
  const l = a.allowed_ips;
  return h("span", { class: "mono", title: l.join("\n") }, l.length > 2 ? l.slice(0, 2).join(", ") + " +" + (l.length - 2) : l.join(", "));
}
function authCell(a) {
  if (a.auth === "none") return lozenge("none", a.methods.some((m) => m !== "GET") ? "danger" : "");
  return h("span", {}, lozenge(a.auth, "success"), a.read_public ? [" ", lozenge("public reads", "")] : null);
}
function cacheCell(a) {
  if (!a.cache.enabled) return h("span", { class: "muted" }, "off");
  const total = a.cache.hits + a.cache.misses;
  return h("span", { title: `${a.cache.entries} entries, ${bytes(a.cache.bytes)}, ttl ${a.cache.ttl_seconds}s` },
    lozenge("on", "purple"), " ", total ? Math.round((a.cache.hits / total) * 100) + "% hits" : "");
}
async function purgeCache(key, after) {
  try {
    const r = await api("POST", apiPath(key) + "/cache/purge");
    toast(`Cache purged: ${r.purged} entr${r.purged === 1 ? "y" : "ies"} dropped`);
    if (after) after();
  } catch (e) { toast(e.message, true); }
}
function confirmModal(title, text, label, fn, danger) {
  modal(title, h("p", {}, text), [h("button", { class: "btn subtle", onclick: closeModal }, "Cancel"),
    h("button", { class: "btn " + (danger === false ? "primary" : "danger"), onclick: async () => {
      try { await fn(); closeModal(); } catch (e) { toast(e.message, true); }
    } }, label)]);
}

// diffView shows a record (or contract) before and after a change.
function diffView(before, after) {
  if (!before && !after) return h("div", { class: "muted" }, "No data.");
  const b = before || {}, a = after || {};
  const keys = [...new Set([...Object.keys(b), ...Object.keys(a)])].sort();
  const fmt = (o, k) => (k in o ? (typeof o[k] === "object" ? JSON.stringify(o[k], null, 2) : String(o[k])) : "");
  return h("div", { class: "table-wrap" }, h("table", { class: "issues diff" },
    h("thead", {}, h("tr", {}, h("th", {}, "Field"), h("th", {}, before ? "Before" : ""), h("th", {}, after ? "After" : ""))),
    h("tbody", {}, keys.map((k) => {
      const x = fmt(b, k), y = fmt(a, k), changed = x !== y;
      return h("tr", { class: changed ? "changed" : "" }, h("td", { class: "mono" }, k),
        h("td", {}, before ? h("pre", { class: "cell-pre" + (changed && k in b ? " del" : "") }, x || "—") : ""),
        h("td", {}, after ? h("pre", { class: "cell-pre" + (changed && k in a ? " ins" : "") }, y || "—") : ""));
    }))));
}

// showAudit opens one history entry with its diff and a revert button.
function showAudit(e, after) {
  const canRevert = isEditor() && !["api_create", "api_delete"].includes(e.action);
  openDetail("#" + e.id, e.action.replace("api_", "API ") + (e.record_id ? " " + e.record_id : " contract"),
    h("div", { class: "toolbar" }, actionLozenge(e.action), viaTag(e.via),
      link(uiPath(e.api), h("span", { class: "mono" }, e.api)),
      canRevert ? h("button", { class: "btn", onclick: () => confirmModal("Revert this change?",
        e.record_id ? (e.before ? `Record ${e.record_id} goes back to how it was before this change.` : `Record ${e.record_id} is deleted again (this change created it).`)
          : "The API's contract goes back to how it was before this change. Records stay as they are.",
        "Revert", async () => { await api("POST", "/api/audit/" + e.id + "/revert"); toast("Reverted"); closeDetail(); if (after) after(); }, false) }, icon("retry"), "Revert") : null),
    props([["When", fmtDT(e.ts)], ["Who", e.actor], ["Via", e.via === "file" ? "hand edit of the file (detected on reload)" : e.via],
      ["Source IP", e.source_ip || null], ["Request ID", e.request_id ? link("/logs?q=" + encodeURIComponent(e.request_id), h("span", { class: "mono" }, e.request_id)) : null],
      ["Record", e.record_id || null]]),
    h("h3", {}, "Change"), diffView(e.before, e.after));
}

function auditCols(withAPI) {
  return [
    { key: "ts", label: "When", time: true, sort: true, filter: "time", render: (e) => when(e.ts) },
    withAPI ? { key: "api", label: "API", sort: true, filter: "text", render: (e) => h("span", { class: "key" }, e.api) } : null,
    { key: "action", label: "Action", sort: true, filter: ["create", "replace", "patch", "delete", "import", "revert", "api_create", "api_update", "api_delete"].map((a) => [a, a]), render: (e) => actionLozenge(e.action) },
    { key: "record_id", label: "Record", sort: true, filter: "text", render: (e) => e.record_id ? h("span", { class: "mono" }, e.record_id) : h("span", { class: "muted" }, "contract") },
    { key: "actor", label: "Who", sort: true, filter: "text", render: (e) => h("span", { class: "cell-flex" }, avatar(e.actor, true), h("span", {}, e.actor)) },
    { key: "via", label: "Via", sort: true, filter: [["api", "api"], ["console", "console"], ["file", "file"]], render: (e) => viaTag(e.via) },
    { label: "Change", render: (e) => h("span", { class: "mono muted" }, shortJSON(e.after || e.before || "", 70)) },
    { key: "source_ip", label: "Source IP", sort: true, filter: "text", render: (e) => h("span", { class: "mono" }, e.source_ip || "") },
    { key: "request_id", label: "Request", sort: true, filter: "text", render: (e) => h("span", { class: "mono muted" }, e.request_id || "") },
  ].filter(Boolean);
}

// ---------------------------------------------------------------- overview

async function pageOverview() {
  const [st, apis, reqs, byApi, recent] = await Promise.all([
    api("GET", "/api/status"),
    api("GET", "/api/apis"),
    api("GET", "/api/logs/summary?since=24h&kind=api&by=status"),
    api("GET", "/api/logs/summary?since=24h&kind=api&by=route"),
    api("GET", "/api/audit?limit=8"),
  ]);
  const total = reqs.items.reduce((a, r) => a + r.count, 0);
  const errs = reqs.items.filter((r) => +r.key >= 400).reduce((a, r) => a + r.count, 0);
  const changes = apis.reduce((a, x) => a + x.changes_24h, 0);
  const cfgErrs = Object.entries(st.config.errors || {});
  const card = (label, value, sub, href) => {
    const c = h("div", { class: "card" }, h("div", { class: "label" }, label), h("div", { class: "value" }, value), h("div", { class: "sub" }, sub));
    return href ? link(href, c) : c;
  };
  const bars = (title, items, empty, href) => h("div", { class: "panel" }, h("h2", {}, title),
    items.length ? items.slice(0, 8).map((r) => {
      const max = items[0].count || 1;
      const b = h("i", { class: r.errors > r.count / 2 ? "err" : "" });
      b.style.width = Math.max(2, (r.count / max) * 100) + "%";
      return h("div", { class: "bar-row" }, href ? link(href(r), h("span", { class: "name", title: r.key }, r.key || "—")) : h("span", { class: "name" }, r.key || "—"),
        h("div", { class: "bar" }, b), h("span", { class: "num muted" }, String(r.count)));
    }) : h("div", { class: "muted" }, empty));
  const p = page("Overview", { desc: "Your file-backed APIs and what happened to them in the last 24 hours." },
    cfgErrs.length ? banner("danger", `${cfgErrs.length} config file(s) failed validation — the previous version is still served`,
      cfgErrs.map(([f, e]) => f + ": " + e).join("\n")) : null,
    h("div", { class: "cards" },
      card("APIs", String(apis.length), apis.filter((a) => !a.enabled).length + " disabled", "/apis"),
      card("Records", st.config.records.toLocaleString(), "across all APIs", "/apis"),
      card("Requests", total.toLocaleString(), "last 24 h", "/logs?kind=api&since=24h"),
      card("Errors", String(errs), "status ≥ 400, 24 h", "/logs?kind=api&status=error&since=24h"),
      card("Changes", String(changes), "records & contracts, 24 h", "/audit?f.ts=24h.."),
      card("Cached APIs", String(st.config.cached_apis), bytes(st.cache.bytes) + " in memory", "/apis")),
    h("div", { class: "grid-2" },
      bars("Busiest APIs", byApi.items, "No API calls yet.", (r) => uiPath(r.key)),
      h("div", { class: "panel" }, h("h2", {}, "Recent changes"),
        recent.items.length ? recent.items.map((e) => h("div", { class: "bar-row event-row" },
          actionLozenge(e.action), link(uiPath(e.api), h("span", { class: "key" }, e.api)),
          h("span", { class: "name" }, (e.record_id || "contract") + " · " + e.actor), h("span", { class: "muted" }, ago(e.ts))))
          : h("div", { class: "muted" }, "Nothing changed yet."))),
    apis.length ? null : emptyState("No APIs yet", "Create one here, or drop a JSON file in " + st.config.dir + "/apis.",
      isEditor() ? link("/apis/new", h("span", { class: "btn primary" }, icon("plus"), "Create API")) : null));
  return { el: p, onLive(topic) { if (topic === "audit") { clearTimeout(this.t); this.t = setTimeout(() => route(), 1500); } } };
}

// ---------------------------------------------------------------- APIs

async function pageAPIs() {
  const apis = await api("GET", "/api/apis");
  const cols = [
    { key: "key", label: "API", sort: true, filter: "text", render: (a) => h("span", { class: "cell-flex" }, avatar(a.name, true), h("span", { class: "key" }, a.key)) },
    { key: "status", label: "Status", v: (a) => a.error ? "invalid file" : a.enabled ? "enabled" : "disabled", sort: true,
      filter: [["enabled", "enabled"], ["disabled", "disabled"], ["invalid file", "invalid file"]],
      render: (a) => a.error ? lozenge("invalid file", "danger") : a.enabled ? lozenge("enabled", "success") : lozenge("disabled") },
    { key: "description", label: "Description", sort: true, filter: "text", render: (a) => a.description || "" },
    { key: "records", label: "Records", num: true, sort: true, filter: "num", render: (a) => a.records.toLocaleString() },
    { key: "methods", label: "Methods", v: (a) => a.methods.join(" "), sort: true, filter: "text", render: (a) => h("span", { class: "mono" }, a.methods.join(" ")) },
    { key: "auth", label: "Auth", sort: true, filter: [["none", "none"], ["bearer", "bearer"], ["api_key", "api_key"], ["basic", "basic"]], render: authCell },
    { key: "allowed_ips", label: "Allowed IPs", v: (a) => a.default ? "private only" : a.allowed_ips.join(", "), sort: true, filter: "text", render: ipsCell },
    { key: "cache", label: "Cache", v: (a) => a.cache.enabled ? "on" : "off", sort: true, filter: [["on", "on"], ["off", "off"]], render: cacheCell },
    { key: "changes_24h", label: "Changes 24h", num: true, sort: true, filter: "num", render: (a) => a.changes_24h ? String(a.changes_24h) : h("span", { class: "muted" }, "0") },
    { key: "file", label: "File", sort: true, filter: "text", render: (a) => h("span", { class: "mono" }, a.file) },
    { key: "mod_time", label: "Modified", time: true, sort: true, filter: "time", render: (a) => when(a.mod_time) },
  ];
  const p = page("APIs", {
    desc: "Each API is one JSON file in the apis/ folder: its contract, who may call it, and its records. Changes made here are written back to the file.",
    actions: isEditor() ? link("/apis/new", h("span", { class: "btn primary" }, icon("plus"), "Create API")) : null,
  }, viewerNote(),
    dataTable({ cols, rows: apis, onRow: (a) => navigate(uiPath(a.key)), defaultSort: ["key:asc"], exportName: "apis",
      empty: ["No APIs yet", "Create one, or drop a JSON file in the apis/ folder: it is picked up within seconds."] }).el);
  return { el: p };
}

// ---------------------------------------------------------------- one API

async function pageAPI(version, name) {
  const key = version + "/" + name;
  const d = await api("GET", apiPath(key));
  const s = d.summary, c = d.contract;
  const tabs = [["records", "Records"], ["contract", "Contract"], ["history", "History"], ["activity", "Requests"], ["cache", "Cache"], ["usage", "Use it"]];
  const f = queryState({ tab: "records" });
  const body = h("div", {});
  const tabBar = h("div", { class: "tabs page-tabs", role: "tablist" }, tabs.map(([t, label]) =>
    h("button", { class: t === f.tab ? "on" : "", role: "tab", onclick: () => {
      const q = new URLSearchParams(); if (t !== "records") q.set("tab", t);
      history.replaceState(null, "", location.pathname + (q.toString() ? "?" + q : "")); route();
    } }, label)));
  const actions = [];
  if (s.cache.enabled && isEditor()) actions.push(h("button", { class: "btn", title: "Drop every cached response of this API", onclick: () => purgeCache(key, () => route()) }, icon("bolt"), "Purge cache"));
  if (isEditor()) {
    actions.push(h("button", { class: "btn", onclick: () => contractEditor(s, c) }, icon("edit"), "Edit contract"));
    actions.push(h("button", { class: "btn subtle", onclick: () => confirmModal("Delete API " + key + "?",
      `Its file ${s.file} is moved to apis/.deleted/ (restore it by moving it back). The API answers 404 at once; its history and logs are kept.`,
      "Delete API", async () => { const r = await api("DELETE", apiPath(key)); toast("Deleted; file kept at " + r.moved_to); navigate("/apis"); }) }, icon("trash"), "Delete"));
  }
  const el = page(key, {
    crumb: h("span", { class: "mono" }, key),
    desc: s.description || "",
    actions,
  },
    s.error ? banner("danger", "The file is invalid — the previous version is still served", s.error) : null,
    h("div", { class: "toolbar api-meta" },
      s.enabled ? lozenge("enabled", "success") : lozenge("disabled"), authCell(s), ipsCell(s),
      h("span", { class: "mono" }, s.methods.join(" ")), h("span", { class: "muted" }, s.records.toLocaleString() + " records"),
      s.cache.enabled ? lozenge("cache on", "purple") : null,
      h("span", { class: "mono muted", title: "File inside the dynamic config directory" }, s.file)),
    tabBar, body);
  const render = { records: tabRecords, contract: tabContract, history: tabHistory, activity: tabActivity, cache: tabCache, usage: tabUsage }[f.tab] || tabRecords;
  const pg = await render(body, key, d);
  return { el, onLive: pg && pg.onLive };
}

// columns -> dataTable columns
function recordCols(d) {
  return d.columns.map((col) => ({
    key: col.name, label: col.name + (col.required ? " *" : ""), sort: true,
    filter: col.enum && col.enum.length ? col.enum.map((x) => [String(x), String(x)]) : "text",
    render: (r) => col.id ? h("span", { class: "key" }, String(r[col.name] ?? "")) : valueCell(r[col.name], col),
  }));
}

async function tabRecords(body, key, d) {
  const s = d.summary;
  const search = h("input", { type: "search", placeholder: "Search all fields…", "aria-label": "Search records", onchange: () => t.reload() });
  const t = dataTable({
    id: "rec", cols: recordCols(d), defaultSort: [], pageSize: 25,
    fetch: serverList(apiPath(key) + "/records", () => { const q = new URLSearchParams(); if (search.value) q.set("q", search.value); return q; }),
    exportPath: apiPath(key) + "/records", exportName: key.replace("/", "-"),
    exportParams: () => { const q = new URLSearchParams(); if (search.value) q.set("q", search.value); return q; },
    onRow: (r) => showRecord(key, d, String(r[s.id_field]), () => route()),
    empty: ["No records", isEditor() ? "Add one, or import a JSON array." : "This API holds no records yet."],
  });
  fill(body, h("div", { class: "toolbar" }, search,
    h("span", { class: "spacer" }),
    h("button", { class: "btn", title: "Download every record as a JSON array (the file's data)", onclick: () => download(apiPath(key) + "/export", key.replace("/", "-") + ".json").catch((e) => toast(e.message, true)) }, icon("download"), "JSON"),
    isEditor() ? h("button", { class: "btn", onclick: () => importModal(key, () => route()) }, icon("upload"), "Import") : null,
    isEditor() ? h("button", { class: "btn primary", onclick: () => recordEditor(key, d, null, () => route()) }, icon("plus"), "Add record") : null),
    h("p", { class: "help" }, "Filter any column: text matches anywhere, =x is exact, !x excludes, >=10 / <2026-06-01 compare, a..b is a range. Shift+click headers to sort by several."),
    t.el);
  return { onLive(topic, e) { if (topic === "audit" && e.api === key && t.page === 1) { clearTimeout(this.timer); this.timer = setTimeout(() => t.reload(), 600); } } };
}

async function showRecord(key, d, id, after) {
  let r;
  try { r = await api("GET", apiPath(key) + "/records/" + encodeURIComponent(id)); } catch (e) { toast(e.message, true); return; }
  const hist = h("div", {}, h("div", { class: "muted" }, "Loading history…"));
  openDetail(key + " / " + id, id,
    h("div", { class: "toolbar" },
      isEditor() ? h("button", { class: "btn primary", onclick: () => recordEditor(key, d, r, after) }, icon("edit"), "Edit") : null,
      isEditor() ? h("button", { class: "btn subtle", onclick: () => confirmModal("Delete record " + id + "?",
        "It is removed from the file. The History tab keeps a copy, and the change can be reverted.", "Delete", async () => {
          await api("DELETE", apiPath(key) + "/records/" + encodeURIComponent(id), undefined, { "If-Match": r.etag });
          toast("Deleted " + id); closeDetail(); after && after();
        }) }, icon("trash"), "Delete") : null),
    props(d.columns.map((c) => [c.name, valueCell(r.data[c.name], c)])),
    h("h3", {}, "JSON"), h("pre", { class: "code" }, JSON.stringify(r.data, null, 2)),
    h("h3", {}, "History"), hist);
  try {
    const a = await api("GET", "/api/audit?api=" + encodeURIComponent(key) + "&record_id=" + encodeURIComponent(id) + "&limit=20");
    fill(hist, a.items.length ? h("ul", { class: "timeline" }, a.items.map((e) => h("li", {},
      actionLozenge(e.action), h("div", {}, h("a", { href: "#", onclick: (ev) => { ev.preventDefault(); showAudit(e, after); } }, fmtDT(e.ts)),
        " by ", h("strong", {}, e.actor), " via ", e.via)))) : h("div", { class: "muted" }, "No recorded changes (history starts when fluid-config first loads a file)."));
  } catch (e) { fill(hist, banner("danger", "Could not load history", e.message)); }
}

// recordInput builds one form control from a column of the contract.
function recordInput(col, obj, isNew) {
  const v = obj[col.name];
  const set = (x) => { if (x === undefined || x === "") delete obj[col.name]; else obj[col.name] = x; };
  const label = col.name + (col.required ? " *" : "") + (col.type && col.type !== "any" ? "  (" + col.type + (col.format ? ", " + col.format : "") + ")" : "");
  let input;
  if (col.id && !isNew) {
    input = h("input", { type: "text", value: String(v ?? ""), disabled: true });
  } else if (col.enum && col.enum.length) {
    input = select([["", "—"], ...col.enum.map((x) => [String(x), String(x)])], v === undefined ? "" : String(v), (x) => {
      const e = col.enum.find((y) => String(y) === x); set(x === "" ? undefined : e); }, col.name);
  } else if (col.type === "boolean") {
    input = select([["", "—"], ["true", "true"], ["false", "false"]], v === undefined ? "" : String(v), (x) => set(x === "" ? undefined : x === "true"), col.name);
  } else if (col.type === "number" || col.type === "integer") {
    input = h("input", { type: "number", step: col.type === "integer" ? "1" : "any", value: v ?? "", onchange: (e) => set(e.target.value === "" ? undefined : Number(e.target.value)) });
  } else if (["object", "array", "any"].includes(col.type) || (v !== undefined && typeof v === "object")) {
    input = h("textarea", { rows: "4", spellcheck: "false", class: "mono", onchange: (e) => {
      try { set(e.target.value.trim() === "" ? undefined : JSON.parse(e.target.value)); e.target.classList.remove("bad"); }
      catch { e.target.classList.add("bad"); obj.__bad = col.name; } } });
    input.value = v === undefined ? "" : JSON.stringify(v, null, 2);
  } else if (col.format === "date") {
    input = h("input", { type: "date", value: v ?? "", onchange: (e) => set(e.target.value) });
  } else {
    input = h("input", { type: "text", value: v ?? "", placeholder: col.format === "date-time" ? "2026-11-26T17:00:00+05:30" : col.id && isNew ? "leave empty to generate" : "",
      onchange: (e) => set(e.target.value) });
  }
  const rules = [col.min !== undefined && "min " + col.min, col.max !== undefined && "max " + col.max, col.pattern && "pattern " + col.pattern].filter(Boolean).join(" · ");
  return h("label", { class: "field" }, h("span", {}, label), input,
    col.description || rules ? h("div", { class: "help" }, [col.description, rules].filter(Boolean).join(" — ")) : null,
    !col.declared && !col.id ? h("div", { class: "help" }, "Not declared in the contract's fields.") : null);
}

function recordEditor(key, d, existing, after) {
  const isNew = !existing;
  let obj = JSON.parse(JSON.stringify(existing ? existing.data : {}));
  const idField = d.summary.id_field;
  let tab = "form";
  const err = h("div", { class: "form-error hidden" });
  const body = h("div", {});
  const jsonArea = h("textarea", { rows: "20", spellcheck: "false", class: "mono", "aria-label": "JSON" });
  const showErr = (m) => { err.textContent = m; err.classList.remove("hidden"); };
  const drawForm = () => fill(body, d.columns.map((col) => recordInput(col, obj, isNew)),
    h("p", { class: "help" }, "Fields marked * are required. Use the JSON tab for fields the contract does not declare."));
  const tabsEl = h("div", { class: "tabs", role: "tablist" });
  const showTab = (t) => {
    if (t === "json") { jsonArea.value = JSON.stringify(obj, null, 2); fill(body, jsonArea); }
    else {
      if (tab === "json") { try { obj = JSON.parse(jsonArea.value || "{}"); } catch (e) { showErr("JSON: " + e.message); return; } }
      drawForm();
    }
    tab = t;
    for (const b of tabsEl.children) b.classList.toggle("on", b.dataset.tab === t);
  };
  tabsEl.append(h("button", { "data-tab": "form", class: "on", onclick: () => showTab("form") }, "Form"),
    h("button", { "data-tab": "json", onclick: () => showTab("json") }, "JSON"));
  drawForm();
  const save = async () => {
    err.classList.add("hidden");
    let payload = obj;
    if (tab === "json") { try { payload = JSON.parse(jsonArea.value); } catch (e) { showErr("JSON: " + e.message); return; } }
    if (obj.__bad && tab === "form") { showErr(obj.__bad + ": not valid JSON"); return; }
    delete payload.__bad;
    try {
      if (isNew) await api("POST", apiPath(key) + "/records", payload);
      else await api("PUT", apiPath(key) + "/records/" + encodeURIComponent(String(existing.data[idField])), payload, { "If-Match": existing.etag });
      closeModal(); closeDetail();
      toast(isNew ? "Record created" : "Saved " + existing.data[idField]);
      after && after();
    } catch (e) {
      showErr(e.status === 412 ? "Someone changed this record since you opened it. Close this window and open the record again to see the current version." : e.message);
    }
  };
  modal((isNew ? "Add record to " : "Edit record " + existing.data[idField] + " in ") + key, [tabsEl, err, body],
    [h("button", { class: "btn subtle", onclick: closeModal }, "Cancel"), h("button", { class: "btn primary", onclick: save }, isNew ? "Create" : "Save")]);
}

function importModal(key, after) {
  const area = h("textarea", { rows: "12", spellcheck: "false", class: "mono", placeholder: '[\n  {"id": 1, "name": "..."},\n  {"id": 2, "name": "..."}\n]', "aria-label": "Records JSON" });
  const file = h("input", { type: "file", accept: ".json,application/json", "aria-label": "JSON file" });
  file.addEventListener("change", async () => { if (file.files[0]) area.value = await file.files[0].text(); });
  let mode = "upsert";
  const err = h("div", { class: "form-error hidden" });
  modal("Import records into " + key, [err,
    h("p", { class: "help" }, "A JSON array of records. Every row is validated first: if one is invalid, nothing is written. Each changed record is recorded in the history."),
    h("label", { class: "field" }, h("span", {}, "From a file"), file),
    h("label", { class: "field" }, h("span", {}, "Records"), area),
    h("label", { class: "field" }, h("span", {}, "Mode"), select([["upsert", "Upsert: add new records, replace those with the same id"],
      ["replace", "Replace all: the API holds exactly these records afterwards"]], mode, (v) => { mode = v; }, "Mode"))],
  [h("button", { class: "btn subtle", onclick: closeModal }, "Cancel"),
    h("button", { class: "btn primary", onclick: async () => {
      let rows;
      try { rows = JSON.parse(area.value); if (!Array.isArray(rows)) throw new Error("must be an array"); } catch (e) { err.textContent = "JSON: " + e.message; err.classList.remove("hidden"); return; }
      try {
        const r = await api("POST", apiPath(key) + "/import?mode=" + mode, rows);
        closeModal(); toast(`Imported: ${r.created} created, ${r.replaced} replaced, ${r.deleted} deleted (${r.total} records)`); after && after();
      } catch (e) { err.textContent = e.message; err.classList.remove("hidden"); }
    } }, "Import")]);
}

async function tabContract(body, key, d) {
  const c = d.contract;
  const fields = Object.entries(c.fields || {});
  fill(body,
    h("div", { class: "grid-2" },
      h("div", { class: "panel" }, h("h2", {}, "Behaviour"), props([
        ["Path", h("span", { class: "mono" }, d.summary.path)], ["ID field", h("span", { class: "mono" }, c.id_field)],
        ["Methods", h("span", { class: "mono" }, c.methods.join(" "))], ["Strict fields", c.strict_fields ? "yes: unknown fields are refused" : "no: extra fields are kept"],
        ["Max records", c.max_records ? String(c.max_records) : "no limit"],
        ["Client caching", c.cache_seconds ? `Cache-Control max-age=${c.cache_seconds}` : "no-cache (ETag / 304 always on)"],
        ["Server cache", c.cache && c.cache.enabled ? `on: ttl ${c.cache.ttl_seconds}s, up to ${c.cache.max_entries} queries` : "off"],
        ["CORS origins", (c.cors_origins || []).join(", ") || null]])),
      h("div", { class: "panel" }, h("h2", {}, "Access"), props([
        ["Auth", c.auth.type], ["Header", c.auth.header || null], ["Public reads", c.auth.read_public ? "yes: GET needs no credentials" : "no"],
        ["Tokens", (c.auth.tokens || []).length ? h("div", { class: "rule-list" }, c.auth.tokens.map((t) => h("code", { class: "rule" }, t))) : null],
        ["Users", c.auth.users ? Object.keys(c.auth.users).join(", ") : null],
        ["Allowed IPs", d.summary.default ? h("span", {}, lozenge("private only", "warn"), " restricted default") : h("div", { class: "rule-list" }, d.summary.allowed_ips.map((r) => h("code", { class: "rule" }, r)))],
        ["Rate limit", c.rate_limit ? `${c.rate_limit.requests_per_minute}/min per IP, burst ${c.rate_limit.burst}` : c.rate_limit_per_min ? c.rate_limit_per_min + "/min per IP" : "none"]]))),
    h("h2", { class: "mt" }, "Fields"),
    fields.length ? table([
      { label: "Field", render: ([k]) => h("span", { class: "mono" }, k) },
      { label: "Type", render: ([, f]) => lozenge(f.type) },
      { label: "Required", render: ([, f]) => f.required ? lozenge("required", "warn") : "" },
      { label: "Rules", render: ([, f]) => [f.format && "format " + f.format, f.enum && "one of " + f.enum.join(", "), f.min !== undefined && "min " + f.min, f.max !== undefined && "max " + f.max, f.pattern && "pattern " + f.pattern].filter(Boolean).join(" · ") },
      { label: "Description", render: ([, f]) => f.description || "" },
    ], fields) : h("p", { class: "muted" }, "No fields declared: any JSON object is accepted."),
    h("h2", { class: "mt" }, "File (without data)"), h("pre", { class: "code" }, JSON.stringify(c, null, 2)));
}

async function tabHistory(body, key) {
  const t = dataTable({ id: "h", cols: auditCols(false), defaultSort: ["ts:desc"], pageSize: 25,
    fetch: serverList("/api/audit", () => new URLSearchParams({ api: key })), onRow: (e) => showAudit(e, () => t.reload()),
    exportPath: "/api/audit", exportName: "history-" + key.replace("/", "-"), exportParams: () => new URLSearchParams({ api: key }),
    empty: ["No changes yet", "Every create, update, delete, import and contract change is recorded here, with who made it."] });
  fill(body, h("p", { class: "help" }, "Every change to this API's records and contract — through the API, this console, or a hand edit of the file. Open one to see the before/after and revert it."), t.el);
  return { onLive(topic, e) { if (topic === "audit" && e.api === key && t.page === 1) { clearTimeout(this.timer); this.timer = setTimeout(() => t.reload(), 600); } } };
}

async function tabActivity(body, key) {
  const cols = [
    { key: "ts", label: "Time", time: true, sort: true, filter: "time", render: (l) => when(l.ts) },
    { key: "method", label: "Method", sort: true, filter: "text", render: (l) => h("strong", {}, l.method || "") },
    { key: "url", label: "URL", sort: true, filter: "text", render: (l) => h("span", { class: "cell-flex" }, l.has_payload ? h("span", { class: "pl-flag" }, "RQ/RS") : null, h("span", { class: "mono" }, l.url || l.msg)) },
    { key: "status", label: "Status", num: true, sort: true, filter: "num", render: (l) => httpLozenge(l.status) },
    { key: "client", label: "Credential", sort: true, filter: "text", render: (l) => l.client || h("span", { class: "muted" }, "—") },
    { key: "duration_ms", label: "ms", num: true, sort: true, filter: "num", render: (l) => String(l.duration_ms || 0) },
    { key: "source_ip", label: "Source IP", sort: true, filter: "text", render: (l) => h("span", { class: "mono" }, l.source_ip || "") },
  ];
  const t = dataTable({ id: "a", cols, defaultSort: ["ts:desc"], pageSize: 25, initialFilters: { route: "=" + key },
    fetch: serverList("/api/logs", () => new URLSearchParams({ kind: "api" })), onRow: showLog,
    exportPath: "/api/logs", exportName: "requests-" + key.replace("/", "-"), exportParams: () => new URLSearchParams({ kind: "api" }),
    empty: ["No requests", "Calls to this API appear here with their full request and response."] });
  fill(body, t.el);
  return { onLive(topic, l) { if (topic === "logs" && l.route === key && t.page === 1) { clearTimeout(this.timer); this.timer = setTimeout(() => t.reload(), 800); } } };
}

async function tabCache(body, key, d) {
  const s = d.summary, st = s.cache;
  const total = st.hits + st.misses;
  fill(body,
    st.enabled ? null : banner("info", "The in-memory cache is off for this API",
      'Turn it on in the contract ("cache": {"enabled": true, "ttl_seconds": 60, "max_entries": 500}). GET responses are then kept in memory per query, and dropped on every change to the data.', true),
    h("div", { class: "cards" },
      h("div", { class: "card" }, h("div", { class: "label" }, "Status"), h("div", { class: "value" }, st.enabled ? "On" : "Off"),
        h("div", { class: "sub" }, st.enabled ? `ttl ${st.ttl_seconds}s · max ${st.max_entries} queries` : "")),
      h("div", { class: "card" }, h("div", { class: "label" }, "Cached queries"), h("div", { class: "value" }, String(st.entries)), h("div", { class: "sub" }, bytes(st.bytes))),
      h("div", { class: "card" }, h("div", { class: "label" }, "Hit rate"), h("div", { class: "value" }, total ? Math.round((st.hits / total) * 100) + "%" : "—"),
        h("div", { class: "sub" }, `${st.hits.toLocaleString()} hits · ${st.misses.toLocaleString()} misses`)),
      h("div", { class: "card" }, h("div", { class: "label" }, "Last purge"), h("div", { class: "value small" }, st.purged_at ? fmtDT(st.purged_at) : "never"))),
    h("div", { class: "panel" }, h("h2", {}, "How it works"), h("ul", { class: "plain" },
      h("li", {}, "Only GET requests that pass the API's IP allowlist, rate limit and auth are answered from the cache, so it never widens access."),
      h("li", {}, "Entries are keyed by the data version: any create, update, delete, import or hand edit of the file makes the old answers unreachable at once."),
      h("li", {}, "Responses say X-Cache: HIT or MISS. A request with Cache-Control: no-cache skips the cache."),
      h("li", {}, "cache_seconds is separate: it tells clients and CDNs how long they may keep answers (Cache-Control max-age)."))),
    st.enabled && isEditor() ? h("div", { class: "toolbar mt" }, h("button", { class: "btn primary", onclick: () => purgeCache(key, () => route()) }, icon("bolt"), "Purge cache now")) : null);
}

async function tabUsage(body, key, d) {
  const s = d.summary, c = d.contract, base = d.api_url + "/" + key;
  const authH = c.auth.type === "bearer" ? ' \\\n  -H "Authorization: Bearer <token>"' : c.auth.type === "api_key" ? ` \\\n  -H "${c.auth.header}: <key>"` : c.auth.type === "basic" ? " \\\n  -u <user>:<password>" : "";
  const readH = c.auth.read_public ? "" : authH;
  const ex = (title, cmd) => h("div", { class: "panel" }, h("div", { class: "panel-head" }, h("h2", {}, title),
    h("button", { class: "btn sm subtle", onclick: () => navigator.clipboard?.writeText(cmd).then(() => toast("Copied")) }, "Copy")), h("pre", { class: "code" }, cmd));
  const firstField = d.columns.find((x) => !x.id);
  const sample = {};
  for (const col of d.columns) if (!col.id && col.required) sample[col.name] = col.enum ? col.enum[0] : col.type === "number" || col.type === "integer" ? 1 : col.type === "boolean" ? true : col.format === "date" ? "2026-01-01" : "…";
  const out = [
    h("p", { class: "help" }, "Calls go to the API listener (", h("span", { class: "mono" }, d.api_url || "/"), "), not this console. ",
      h("a", { href: "#", onclick: (e) => { e.preventDefault(); download("/api/openapi.json", "openapi.json").catch((x) => toast(x.message, true)); } }, "Download the OpenAPI document"), " for Swagger UI, Postman or client generators."),
    ex("List, filter, sort and page", `curl "${base}?${firstField ? firstField.name + "[like]=x&" : ""}sort=-${firstField ? firstField.name : s.id_field}&page=1&limit=50"${readH}`),
    ex("Get one record", `curl "${base}/<id>"${readH}`)];
  if (s.methods.includes("POST")) out.push(ex("Create", `curl -X POST "${base}"${authH} \\\n  -H "Content-Type: application/json" \\\n  -d '${JSON.stringify(sample)}'`));
  if (s.methods.includes("PATCH")) out.push(ex("Change some fields (null removes one)", `curl -X PATCH "${base}/<id>"${authH} \\\n  -H 'If-Match: "<etag from GET>"' \\\n  -H "Content-Type: application/json" \\\n  -d '{"${firstField ? firstField.name : "field"}": null}'`));
  if (s.methods.includes("DELETE")) out.push(ex("Delete", `curl -X DELETE "${base}/<id>"${authH}`));
  out.push(ex("Contract (no data, no secrets)", `curl "${base}/_schema"${readH}`));
  out.push(h("div", { class: "panel" }, h("h2", {}, "Query syntax"), props([["?field=x", "equals"], ["?field[ne|gt|gte|lt|lte]=x", "compare (numbers as numbers, ISO dates sort correctly)"],
    ["?field[in]=a,b", "one of"], ["?field[like]=x", "contains"], ["?field[exists]=false", "missing"], ["?a.b=x", "nested fields"], ["?q=x", "search every field"],
    ["?sort=-a,b", "sort, - for descending"], ["?page=2&limit=50&fields=a,b", "paging and projection"]])));
  fill(body, h("div", { class: "grid-2" }, out));
}

// ---------------------------------------------------------------- contract editor (create / edit API)

const FIELD_TYPES = ["string", "number", "integer", "boolean", "object", "array", "any"];

function contractTemplate() {
  return {
    $schema: "https://raw.githubusercontent.com/skpjr001/fluid-config/main/schema/api.schema.json",
    name: "", version: "v1", description: "", enabled: true, id_field: "id", methods: ["GET"],
    auth: { type: "none" }, allowed_ips: [], cache: { enabled: false, ttl_seconds: 60, max_entries: 500 },
    fields: { name: { type: "string", required: true } },
  };
}

// contractForm edits everything but the data of an API file.
function contractForm(obj, isNew, fileRef) {
  const sec = (legend, ...kids) => h("fieldset", {}, h("legend", {}, legend), ...kids);
  const txt = (k, label, help, ph) => fieldInput({ k, label, type: "text", help, placeholder: ph, create: k === "name" || k === "version" ? false : false }, obj, isNew);
  const methods = h("div", { class: "checks" }, ["GET", "POST", "PUT", "PATCH", "DELETE"].map((m) => h("label", { class: "check" },
    h("input", { type: "checkbox", checked: (obj.methods || []).includes(m), onchange: (e) => {
      const s = new Set(obj.methods || []); e.target.checked ? s.add(m) : s.delete(m);
      obj.methods = ["GET", "POST", "PUT", "PATCH", "DELETE"].filter((x) => s.has(x)); } }), m)));
  obj.auth = obj.auth || { type: "none" };
  const authBox = h("div", {});
  const drawAuth = () => fill(authBox,
    fieldInput({ k: "auth.type", label: "Authentication", type: "select", options: [["none", "None"], ["bearer", "Bearer token (Authorization: Bearer …)"], ["api_key", "API key header"], ["basic", "HTTP Basic (users)"]] }, obj, isNew),
    obj.auth.type === "api_key" ? fieldInput({ k: "auth.header", label: "Header", type: "text", placeholder: "X-API-Key" }, obj, isNew) : null,
    obj.auth.type === "bearer" || obj.auth.type === "api_key" ? fieldInput({ k: "auth.tokens", label: "Tokens", type: "list",
      help: "One per line. Use Generate token below (only its hash is stored), env:VAR (read from the server's environment), sha256:<hex>, or a literal. Name a token for the history with name=env:VAR or name=sha256:<hex>. Stored literals show as ******** — leave them to keep them." }, obj, isNew) : null,
    obj.auth.type === "bearer" || obj.auth.type === "api_key" ? h("div", { class: "toolbar" },
      h("input", { type: "text", placeholder: "token name, e.g. ci-bot", "aria-label": "New token name", class: "tok-name" }),
      h("button", { class: "btn sm", onclick: async (e) => {
        const nameEl = e.target.closest(".toolbar").querySelector(".tok-name");
        const name = (nameEl.value.trim() || "token" + ((obj.auth.tokens || []).length + 1)).replace(/[^A-Za-z0-9_.-]/g, "-");
        try {
          const r = await api("POST", "/api/tokens/generate");
          obj.auth.tokens = [...(obj.auth.tokens || []), name + "=" + r.ref];
          drawAuth();
          const shown = h("div", { class: "form-ok" }, h("strong", {}, "Token " + name + ": "), h("span", { class: "mono" }, r.token),
            h("div", { class: "help" }, "Copy it now: only its SHA-256 hash is saved in the file, so it cannot be shown again."),
            h("button", { class: "btn sm", onclick: () => navigator.clipboard?.writeText(r.token).then(() => toast("Copied")) }, "Copy"));
          authBox.prepend(shown);
        } catch (err) { toast(err.message, true); }
      } }, icon("key"), "Generate token")) : null,
    obj.auth.type === "basic" ? fieldInput({ k: "auth.users", label: "Users", type: "kv", help: "One per line: user: password (env:VAR and sha256:<hex> work too)." }, obj, isNew) : null,
    obj.auth.type !== "none" ? fieldInput({ k: "auth.read_public", label: "Public reads: GET needs no credentials, writes do", type: "bool" }, obj, isNew) : null);
  const authSel = () => { const s = authBox.querySelector("select"); if (s) s.addEventListener("change", () => { setTimeout(drawAuth, 0); }); };
  drawAuth(); authSel();
  new MutationObserver(authSel).observe(authBox, { childList: true });

  // field builder
  obj.fields = obj.fields || {};
  const rows = h("div", { class: "field-rows" });
  const drawFields = () => fill(rows, Object.entries(obj.fields).map(([name, f]) => {
    const rename = h("input", { type: "text", value: name, "aria-label": "Field name", class: "mono", onchange: (e) => {
      const n = e.target.value.trim(); if (!n || n === name || obj.fields[n]) { e.target.value = name; return; }
      const next = {}; for (const [k, v] of Object.entries(obj.fields)) next[k === name ? n : k] = v; obj.fields = next; drawFields(); } });
    const num = (k, label) => h("input", { type: "number", value: f[k] ?? "", placeholder: label, "aria-label": name + " " + label, onchange: (e) => { if (e.target.value === "") delete f[k]; else f[k] = Number(e.target.value); } });
    const enumIn = h("input", { type: "text", value: (f.enum || []).join(", "), placeholder: "enum: a, b, c", "aria-label": name + " enum", onchange: (e) => {
      const l = e.target.value.split(",").map((x) => x.trim()).filter(Boolean);
      if (l.length) f.enum = f.type === "number" || f.type === "integer" ? l.map(Number) : l; else delete f.enum; } });
    return h("div", { class: "field-row" }, rename,
      select(FIELD_TYPES.map((t) => [t, t]), f.type || "any", (v) => { f.type = v; }, name + " type"),
      h("label", { class: "check" }, h("input", { type: "checkbox", checked: !!f.required, onchange: (e) => { if (e.target.checked) f.required = true; else delete f.required; } }), "required"),
      select([["", "format"], ["date", "date"], ["date-time", "date-time"], ["email", "email"], ["uri", "uri"]], f.format || "", (v) => { if (v) f.format = v; else delete f.format; }, name + " format"),
      enumIn, num("min", "min"), num("max", "max"),
      h("input", { type: "text", value: f.pattern || "", placeholder: "pattern (regex)", class: "mono", "aria-label": name + " pattern", onchange: (e) => { if (e.target.value) f.pattern = e.target.value; else delete f.pattern; } }),
      h("input", { type: "text", value: f.description || "", placeholder: "description", "aria-label": name + " description", onchange: (e) => { if (e.target.value) f.description = e.target.value; else delete f.description; } }),
      h("button", { class: "btn sm subtle", "aria-label": "Remove field " + name, onclick: () => { delete obj.fields[name]; drawFields(); } }, icon("trash")));
  }));
  drawFields();
  const addField = h("button", { class: "btn sm", onclick: () => { let i = 1; while (obj.fields["field" + i]) i++; obj.fields["field" + i] = { type: "string" }; drawFields(); } }, icon("plus"), "Add field");

  obj.cache = obj.cache || { enabled: false };
  return [
    sec("API",
      h("div", { class: "grid-2 tight" },
        txt("name", "Name", "Lowercase letters, digits, - and _. Part of the URL."),
        txt("version", "Version", "v1, v2, v1-beta …")),
      txt("description", "Description"),
      isNew && fileRef ? h("label", { class: "field" }, h("span", {}, "File (inside apis/)"),
        h("input", { type: "text", class: "mono", placeholder: "<name>.json — or a sub-folder: market/holidays.json", value: fileRef.value, onchange: (e) => { fileRef.value = e.target.value.trim(); } }),
        h("div", { class: "help" }, "Sub-folders only group files; the URL is always /<version>/<name>.")) : null,
      fieldInput({ k: "enabled", label: "Enabled (off: the API answers 404, the file is kept)", type: "bool" }, obj, isNew)),
    sec("Methods", methods, h("div", { class: "help" }, "GET is reading; POST creates, PUT replaces (or creates), PATCH merges, DELETE removes.")),
    sec("Who can call it",
      fieldInput({ k: "allowed_ips", label: "Allowed IPs", type: "list", help: IP_HELP + " Empty: loopback and private networks only (restricted default), so internet clients get 403." }, obj, isNew),
      authBox,
      h("div", { class: "grid-2 tight" },
        fieldInput({ k: "rate_limit.requests_per_minute", label: "Rate limit (requests / minute / IP)", type: "number", help: "Empty: no limit." }, obj, isNew),
        fieldInput({ k: "rate_limit.burst", label: "Burst", type: "number" }, obj, isNew)),
      fieldInput({ k: "cors_origins", label: "CORS origins", type: "list", help: "Browser apps allowed to call it, e.g. https://app.example.com (or *)." }, obj, isNew)),
    sec("Caching",
      fieldInput({ k: "cache.enabled", label: "In-memory cache: keep GET responses on the server until the data changes or the TTL passes", type: "bool" }, obj, isNew),
      h("div", { class: "grid-2 tight" },
        fieldInput({ k: "cache.ttl_seconds", label: "Cache TTL (seconds)", type: "number", help: "Default 60." }, obj, isNew),
        fieldInput({ k: "cache.max_entries", label: "Max cached queries", type: "number", help: "Default 500; least recently used are dropped." }, obj, isNew)),
      fieldInput({ k: "cache_seconds", label: "Client cache (Cache-Control max-age, seconds)", type: "number", help: "How long browsers and CDNs may keep answers. ETag / 304 is always on." }, obj, isNew)),
    sec("Records",
      h("div", { class: "grid-2 tight" },
        txt("id_field", "ID field", "Default id. Numeric ids are generated as max+1, others as random hex."),
        fieldInput({ k: "max_records", label: "Max records", type: "number", help: "Empty: no limit." }, obj, isNew)),
      fieldInput({ k: "strict_fields", label: "Strict: refuse fields not declared below", type: "bool" }, obj, isNew),
      h("div", { class: "field-head" }, h("strong", {}, "Fields"), h("span", { class: "help" }, "Validated on every write.")), rows, addField),
  ];
}

// cleanContract drops empty optional settings so the file stays tidy.
function cleanContract(obj) {
  const c = JSON.parse(JSON.stringify(obj));
  const fields = c.fields; delete c.fields;
  const out = prune(c) || {};
  out.auth = out.auth || { type: "none" };
  if (out.auth.type === "none") out.auth = { type: "none" };
  if (out.cache && !out.cache.enabled) delete out.cache;
  if (out.rate_limit && !out.rate_limit.requests_per_minute) delete out.rate_limit;
  if (obj.enabled === false) out.enabled = false;
  if (fields && Object.keys(fields).length) out.fields = fields;
  delete out.data;
  return out;
}

function contractModal(title, obj, isNew, fileRef, onSave, existingKey) {
  let tab = "form";
  const err = h("div", { class: "form-error hidden" });
  const ok = h("div", { class: "form-ok hidden" });
  const body = h("div", {});
  const jsonArea = h("textarea", { rows: "24", spellcheck: "false", class: "mono", "aria-label": "JSON" });
  const showErr = (m) => { ok.classList.add("hidden"); err.textContent = m; err.classList.remove("hidden"); };
  const current = () => { if (tab === "json") return JSON.parse(jsonArea.value); return cleanContract(obj); };
  const tabsEl = h("div", { class: "tabs", role: "tablist" });
  const showTab = (t) => {
    if (t === "json") { jsonArea.value = JSON.stringify(cleanContract(obj), null, 2); fill(body, jsonArea, h("div", { class: "help" }, "The API file without its data, validated against ", h("a", { href: BASE + "/api/schema/api.schema.json", target: "_blank", rel: "noopener" }, "the JSON Schema"), ". Records stay as they are.")); }
    else {
      if (tab === "json") { try { obj = JSON.parse(jsonArea.value || "{}"); } catch (e) { showErr("JSON: " + e.message); return; } }
      fill(body, contractForm(obj, isNew, fileRef));
    }
    tab = t;
    for (const b of tabsEl.children) b.classList.toggle("on", b.dataset.tab === t);
  };
  tabsEl.append(h("button", { "data-tab": "form", class: "on", onclick: () => showTab("form") }, "Form"),
    h("button", { "data-tab": "json", onclick: () => showTab("json") }, "JSON"));
  fill(body, contractForm(obj, isNew, fileRef));
  const validate = async () => {
    err.classList.add("hidden");
    let c; try { c = current(); } catch (e) { showErr("JSON: " + e.message); return; }
    const r = await api("POST", "/api/validate" + (existingKey ? "?existing=" + encodeURIComponent(existingKey) : ""), isNew ? { ...c, data: [] } : c);
    if (r.valid) { ok.textContent = "Valid: " + r.key + " at " + r.path + (isNew ? "" : " (" + r.records + " records still valid)"); ok.classList.remove("hidden"); }
    else showErr(r.error);
  };
  modal(title, [tabsEl, err, ok, body],
    [h("button", { class: "btn subtle", onclick: closeModal }, "Cancel"),
      h("button", { class: "btn", onclick: () => validate().catch((e) => showErr(e.message)) }, "Validate"),
      h("button", { class: "btn primary", onclick: async () => {
        let c; try { c = current(); } catch (e) { showErr("JSON: " + e.message); return; }
        try { await onSave(c); } catch (e) { showErr(e.message); }
      } }, isNew ? "Create API" : "Save contract")]);
  document.querySelector(".modal").classList.add("wide");
}

function contractEditor(s, contract) {
  const obj = JSON.parse(JSON.stringify(contract));
  contractModal("Edit contract of " + s.key, obj, false, null, async (c) => {
    const r = await api("PUT", apiPath(s.key), c);
    closeModal(); toast("Saved " + r.key);
    if (r.key !== s.key) navigate(uiPath(r.key), true); else route();
  }, s.key);
}

async function pageNewAPI() {
  if (!isEditor()) { page("Create API", {}, viewerNote()); return; }
  const p = page("Create API", { crumb: "new", desc: "A new API is a new JSON file in the apis/ folder. Describe its contract here; records can be added right after." },
    h("div", { class: "grid-2" },
      h("div", { class: "panel" }, h("h2", {}, "Start from"),
        h("div", { class: "toolbar" },
          h("button", { class: "btn primary", onclick: () => start(contractTemplate()) }, icon("plus"), "Blank API"),
          h("button", { class: "btn", onclick: () => start(sampleHolidays()) }, "Example: holidays"),
          h("button", { class: "btn", onclick: () => start(sampleFlags()) }, "Example: feature flags")),
        h("p", { class: "help" }, "Or paste a whole API file (with data) below.")),
      h("div", { class: "panel" }, h("h2", {}, "Paste an API file"),
        (() => {
          const area = h("textarea", { rows: "8", spellcheck: "false", class: "mono", placeholder: '{"name": "...", "version": "v1", "fields": {...}, "data": [...]}', "aria-label": "API file JSON" });
          const err = h("div", { class: "form-error hidden" });
          const file = h("input", { type: "text", class: "mono", placeholder: "file inside apis/ (optional): market/holidays.json", "aria-label": "File path" });
          return [err, area, file, h("div", { class: "toolbar mt" }, h("button", { class: "btn", onclick: async () => {
            try {
              const spec = JSON.parse(area.value);
              const r = await api("POST", "/api/apis" + (file.value.trim() ? "?file=" + encodeURIComponent(file.value.trim()) : ""), spec);
              toast("Created " + r.key + " in " + r.file); navigate(uiPath(r.key));
            } catch (e) { err.textContent = e.message; err.classList.remove("hidden"); }
          } }, "Create from JSON"))];
        })())));
  const start = (tpl) => {
    const fileRef = { value: "" };
    contractModal("Create API", tpl, true, fileRef, async (c) => {
      const r = await api("POST", "/api/apis" + (fileRef.value ? "?file=" + encodeURIComponent(fileRef.value) : ""), { ...c, data: [] });
      closeModal(); toast("Created " + r.key + " in " + r.file); navigate(uiPath(r.key));
    });
  };
  return { el: p };
}

function sampleHolidays() {
  return { ...contractTemplate(), name: "holidays", description: "Trading holidays", methods: ["GET", "POST", "PUT", "PATCH", "DELETE"],
    auth: { type: "bearer", tokens: [], read_public: true }, cache_seconds: 300,
    cache: { enabled: true, ttl_seconds: 300, max_entries: 500 }, strict_fields: true,
    fields: { date: { type: "string", format: "date", required: true }, exchange: { type: "string", enum: ["NSE", "BSE", "MCX"], required: true },
      reason: { type: "string", min: 2, max: 120, required: true } } };
}
function sampleFlags() {
  return { ...contractTemplate(), name: "flags", description: "Feature flags read by the apps", id_field: "key", methods: ["GET", "PUT", "PATCH"],
    auth: { type: "api_key", header: "X-API-Key", tokens: [], read_public: true },
    cache: { enabled: true, ttl_seconds: 30, max_entries: 200 },
    fields: { key: { type: "string", pattern: "^[a-z0-9_.-]+$", required: true }, enabled: { type: "boolean", required: true },
      rollout: { type: "integer", min: 0, max: 100, description: "percent of users" }, notes: { type: "string" } } };
}

// ---------------------------------------------------------------- history

async function pageAudit() {
  const search = h("input", { type: "search", placeholder: "Search record ids and values…", "aria-label": "Search history", onchange: () => t.reload() });
  const t = dataTable({ cols: auditCols(true), defaultSort: ["ts:desc"], pageSize: 50,
    fetch: serverList("/api/audit", () => { const q = new URLSearchParams(); if (search.value) q.set("q", search.value); return q; }),
    onRow: (e) => showAudit(e, () => t.reload()), exportPath: "/api/audit", exportName: "history",
    exportParams: () => { const q = new URLSearchParams(); if (search.value) q.set("q", search.value); return q; },
    empty: ["No changes yet", "Changes through the APIs, this console and hand edits of the files appear here."] });
  page("History", { desc: "Every change to records and contracts across all APIs: who, when, through what, and the record before and after. Open an entry to revert it." },
    h("div", { class: "toolbar" }, search), t.el);
  return { onLive(topic) { if (topic === "audit" && t.page === 1) { clearTimeout(this.timer); this.timer = setTimeout(() => t.reload(), 600); } } };
}

// ---------------------------------------------------------------- access

const IP_HELP = "One per line: * (anyone), an IP, a CIDR (52.66.0.0/16) or an IPv4 wildcard (52.66.*.*, 52.66.*, 52.66.1-5.*).";

async function pageAccess() {
  const a = await api("GET", "/api/access");
  const isDefault = (l) => l.length === a.default.length && l.every((x, i) => x === a.default[i]);
  const status = (l) => {
    if (!l.length) return lozenge("Nobody", "danger");
    if (l.includes("*")) return lozenge("Everyone", "warn");
    return isDefault(l) ? lozenge("Restricted default", "success") : lozenge("Restricted", "success");
  };
  const lists = [
    { k: "admin", title: "Console", what: "People and tools using this console, its API and /metrics." },
    { k: "catalog", title: "Catalog (/_catalog, /openapi.json)", what: "Who may read the list of APIs on the API listener" + (a.public_catalog ? " (public_catalog is on: no token needed)." : " (an admin token is also required).") },
  ];
  const rules = (l) => h("div", { class: "rule-list" }, l.map((r) => h("code", { class: "rule" }, r)));
  const panel = (l) => h("div", { class: "panel" },
    h("div", { class: "panel-head" }, h("h2", {}, l.title), status(a[l.k])),
    h("p", { class: "help" }, l.what),
    a[l.k].length ? rules(a[l.k]) : h("p", { class: "muted" }, "Empty list: every client is refused."),
    l.k === "admin" ? h("p", { class: "help" }, icon("lock"), " File-only: change it in access.json in the dynamic config directory, so nobody can lock the console or open it to the internet from here.") : null);
  const apiCols = [
    { key: "key", label: "API", sort: true, filter: "text", render: (x) => h("span", { class: "key" }, x.key) },
    { key: "allowed_ips", label: "Who can call it", v: (x) => x.default ? "private only (default)" : x.allowed_ips.join(", "), sort: true, filter: "text",
      render: (x) => x.default ? h("span", {}, lozenge("private only", "warn"), " restricted default") : x.allowed_ips.includes("*") ? lozenge("anyone", x.auth === "none" ? "danger" : "") : rules(x.allowed_ips) },
    { key: "auth", label: "Auth", sort: true, filter: "text", render: authCell },
    { key: "methods", label: "Methods", v: (x) => x.methods.join(" "), sort: true, filter: "text", render: (x) => h("span", { class: "mono" }, x.methods.join(" ")) },
    { key: "status", label: "Status", v: (x) => x.enabled ? "enabled" : "disabled", sort: true, filter: [["enabled", "enabled"], ["disabled", "disabled"]], render: (x) => x.enabled ? lozenge("enabled", "success") : lozenge("disabled") },
  ];
  const notes = a.notes.filter((n) => n.level !== "info");
  const ipIn = h("input", { type: "text", placeholder: "e.g. 52.66.10.20", value: a.client_ip, "aria-label": "IP address to test" });
  const out = h("div", { class: "tester-out" });
  const verdict = (v, label) => h("div", { class: "tester-row" },
    v.allowed ? lozenge("allowed", "success") : lozenge("refused", "danger"), h("span", {}, label),
    v.rule ? h("span", { class: "muted" }, "matched ", h("code", {}, v.rule)) : null);
  const test = async () => {
    try {
      const r = await api("POST", "/api/access/test", { ip: ipIn.value.trim() });
      fill(out, lists.map((l) => verdict(r[l.k], l.title)), r.apis.map((v) => verdict(v, "api " + v.api)));
    } catch (e) { fill(out, h("div", { class: "form-error" }, e.message)); }
  };
  ipIn.addEventListener("keydown", (e) => { if (e.key === "Enter") test(); });
  const edit = () => {
    const obj = { catalog: a.catalog };
    const err = h("div", { class: "form-error hidden" });
    modal("Edit catalog allowlist", [err,
      fieldInput({ k: "catalog", label: "Catalog (/_catalog, /openapi.json)", type: "list", help: IP_HELP + " Empty refuses everyone." }, obj, false),
      h("p", { class: "help" }, "The console list is file-only. Each API has its own Allowed IPs in its contract.")],
    [h("button", { class: "btn subtle", onclick: closeModal }, "Cancel"),
      h("button", { class: "btn subtle", onclick: () => save({ catalog: [...a.default] }) }, "Reset to restricted default"),
      h("button", { class: "btn primary", onclick: () => save(obj, err) }, "Save")]);
  };
  const save = async (obj, err) => {
    try { await api("PUT", "/api/access", obj); closeModal(); toast("Saved"); route(); }
    catch (e) { if (err) { err.textContent = e.message; err.classList.remove("hidden"); } else toast(e.message, true); }
  };
  const el = page("Access", {
    desc: "Who may call each API (its Allowed IPs), and who may use this console and the catalog. Anything else gets 403 before authentication, and the refusal is logged.",
    actions: isEditor() ? [h("button", { class: "btn primary", onclick: edit }, icon("edit"), "Edit catalog allowlist")] : null,
  }, viewerNote(),
    notes.map((n) => banner(n.level === "error" ? "danger" : "warn", n.level === "error" ? "Error" : "Warning", n.message, true)),
    banner("info", "You are connecting from " + a.client_ip,
      a.trusted_proxies.length ? "Addresses are taken from X-Forwarded-For when the request comes through a trusted proxy (" + a.trusted_proxies.join(", ") + ")."
        : "No reverse proxy is trusted, so the connecting address is what is checked.", true),
    h("h2", {}, "APIs"),
    h("p", { class: "help" }, "Each API has its own Allowed IPs (allowed_ips, or cidrs, in its file). Without any, only loopback and private networks may call it."),
    dataTable({ id: "apis", cols: apiCols, rows: a.apis, onRow: (x) => navigate(uiPath(x.key) + "?tab=contract"), pageSize: 10, defaultSort: ["key:asc"], exportName: "api-access",
      empty: ["No APIs", "Create one to set who can call it."] }).el,
    h("div", { class: "grid-2 mt" }, lists.map(panel)),
    h("div", { class: "grid-2 mt" },
      h("div", { class: "panel" }, h("h2", {}, "Test an address"),
        h("div", { class: "tester" }, ipIn, h("button", { class: "btn", onclick: test }, "Check")), out),
      h("div", { class: "panel" }, h("h2", {}, "Rule syntax"),
        props([["*", "anyone"], ["203.0.113.7", "one address (IPv4 or IPv6)"], ["10.0.0.0/8", "CIDR (IPv4 or IPv6)"],
          ["52.66.*.*  or  52.66.*", "IPv4 wildcard octets"], ["192.168.1-5.*", "IPv4 octet range"],
          ["(none)", "restricted default: " + a.default.join(", ")]]))));
  test();
  return { el };
}

// ---------------------------------------------------------------- system

async function pageSystem() {
  const st = await api("GET", "/api/status");
  const errs = Object.entries(st.config.errors || {});
  const up = st.uptime_sec;
  const el = page("Status", { desc: "Runtime state of this fluid-config server." },
    errs.length ? banner("danger", "Invalid config files (previous version still in use)", errs.map(([f, e]) => f + ": " + e).join("\n"))
      : banner("info", "All config files are valid", "Loaded " + fmtDT(st.config.loaded_at) + " from " + st.config.dir),
    h("div", { class: "cards" },
      h("div", { class: "card" }, h("div", { class: "label" }, "Version"), h("div", { class: "value" }, st.version)),
      h("div", { class: "card" }, h("div", { class: "label" }, "Uptime"), h("div", { class: "value" },
        up > 86400 ? Math.floor(up / 86400) + "d" : up > 3600 ? Math.floor(up / 3600) + "h" : Math.floor(up / 60) + "m")),
      h("div", { class: "card" }, h("div", { class: "label" }, "Database"), h("div", { class: "value" }, bytes(st.database.size_bytes)),
        h("div", { class: "sub" }, "logs " + st.database.retention_days + " d · history " + st.database.audit_retention_days + " d")),
      h("div", { class: "card" }, h("div", { class: "label" }, "Cache memory"), h("div", { class: "value" }, bytes(st.cache.bytes)),
        h("div", { class: "sub" }, st.config.cached_apis + " API(s) cache")),
      h("div", { class: "card" }, h("div", { class: "label" }, "Dropped log rows"), h("div", { class: "value" }, String(st.logs_dropped)))),
    h("div", { class: "grid-2" },
      h("div", { class: "panel" }, h("h2", {}, "Configuration"), props([
        ["APIs", String(st.config.apis)], ["Records", st.config.records.toLocaleString()], ["Directory", h("span", { class: "mono" }, st.config.dir)],
        ["Admin tokens", "admin.json (file only, not editable here)"]])),
      h("div", { class: "panel" }, h("h2", {}, "Runtime"), props([
        ["API listener", h("span", { class: "mono" }, st.listeners.api)], ["Console listener", h("span", { class: "mono" }, st.listeners.admin)],
        ["API URL", h("span", { class: "mono" }, st.api_url)], ["Database", h("span", { class: "mono" }, st.database.path)],
        ["Signed in as", state.me.name + " (" + state.me.role + ")"]]))),
    h("div", { class: "panel notes" }, h("h2", {}, "Deployment notes"),
      h("p", { class: "help" }, "Checked at startup for this way of running fluid-config (" +
        (st.environment.container ? st.environment.container + " container" : st.environment.systemd ? "host, systemd" : "host") + ")."),
      st.deployment.length ? h("ul", { class: "timeline" }, ["error", "warn", "info"].flatMap((lv) =>
        st.deployment.filter((d) => d.level === lv).map((d) => h("li", {},
          lozenge(d.level, d.level === "info" ? "info" : d.level === "error" ? "danger" : "warn"),
          h("div", {}, d.message), h("span", { class: "mono muted" }, d.topic))))) : h("div", { class: "muted" }, "Nothing to report.")),
    isEditor() ? h("div", { class: "toolbar" }, h("button", { class: "btn", onclick: async () => {
      try { const r = await api("POST", "/api/reload"); toast(Object.keys(r.errors || {}).length ? "Reloaded with errors" : "Reloaded"); route(); }
      catch (e) { toast(e.message, true); }
    } }, icon("refresh"), "Reload config files")) : null,
    h("p", { class: "help" }, "Prometheus metrics: GET /metrics on this listener with a viewer or editor token."));
  return { el };
}

function pageNotFound() {
  page("Not found", {}, emptyState("This page does not exist", "", link("/", "Go to overview")));
}

// ---------------------------------------------------------------- login & boot

function renderLogin(error) {
  const input = h("input", { type: "password", autocomplete: "current-password", placeholder: "Admin token", "aria-label": "Admin token" });
  const submit = async (e) => {
    e.preventDefault();
    state.token = input.value.trim();
    try {
      state.me = await api("GET", "/api/whoami");
      try { sessionStorage.setItem("fluid-token", state.token); } catch { /* ignore */ }
      boot();
    } catch (err) {
      state.token = null;
      renderLogin(err.status === 401 ? "That token was not accepted." : err.message);
    }
  };
  fill($app, h("div", { class: "login" }, h("form", { class: "login-card", onsubmit: submit },
    h("div", { class: "logo-mark" }, "F"), h("h1", {}, "fluid-config console"),
    h("p", {}, "Sign in with a viewer or editor token from admin.json (fluid-config token prints them)."),
    error ? h("div", { class: "form-error" }, error) : null,
    h("label", { class: "field" }, h("span", {}, "Token"), input),
    h("button", { class: "btn primary", type: "submit" }, "Sign in"))));
  input.focus();
}

async function boot() {
  if (!state.token) return renderLogin();
  if (!state.me) {
    try { state.me = await api("GET", "/api/whoami"); } catch { return; }
  }
  renderShell();
  try { state.live = localStorage.getItem("fluid-live") !== "0"; } catch { state.live = true; }
  if (state.live) startLive();
  route();
}

boot();

