"use strict";
/* Proxmox-style pages: a VM or container opens in the content area with a
   toolbar and tabs (Summary, Console, Hardware, Network & firewall,
   Snapshots, Backups, Options), and a task bar at the bottom lists what the
   server is doing. The Manage dialogs are reused: shown non-modal, as a page.
   Uses index.html's $, call, esc, toast, ic, gb, vms, isAdmin, sshLine; containers.js's cts. */

const PAGE_TABS = {
  dlgManage: [["summary", "Summary"], ["console", "Console"], ["resources", "Hardware"], ["network", "Network & firewall"],
    ["snapshots", "Snapshots"], ["backups", "Backups"], ["options", "Options"]],
  dlgCTM: [["summary", "Summary"], ["console", "Console"], ["resources", "Resources"], ["network", "Firewall"],
    ["snapshots", "Snapshots"], ["backups", "Backups"], ["options", "Options"]],
};
const pageTab = {dlgManage: "summary", dlgCTM: "summary"};

function pageName(dlg){ return dlg.id === "dlgManage" ? mName : ctCur; }

function ensurePageChrome(dlg){
  if (dlg.querySelector(".tabs")) return;
  const body = dlg.querySelector(".dlg-body"), h2 = body.querySelector("h2"), sub = h2.nextElementSibling;
  h2.insertAdjacentHTML("beforebegin", `<div class="page-crumb"><a data-home>Datacenter</a> › <span data-kind></span></div>`);
  const anchor = dlg.id === "dlgCTM" ? body.querySelector("#cmPower") : sub;
  anchor.insertAdjacentHTML("afterend", `<div class="page-bar" data-bar></div>` +
    `<nav class="tabs" role="tablist">${PAGE_TABS[dlg.id].map(([id, label]) => `<button type="button" role="tab" data-ptab="${id}">${label}</button>`).join("")}</nav>`);
  if (dlg.id === "dlgManage") body.querySelector('[data-tab="summary"]').insertAdjacentHTML("afterbegin", `<div class="kv" id="mInfo"></div>`);
  dlg.querySelector(".tabs").addEventListener("click", e => { const b = e.target.closest("[data-ptab]"); if (b) selectTab(dlg, b.dataset.ptab); });
  dlg.querySelector("[data-home]").onclick = () => dlg.close();
  dlg.addEventListener("close", () => {
    dlg.classList.remove("as-page");
    for (const f of dlg.querySelectorAll(".console-frame")) f.innerHTML = "";
    markSide();
  });
}

function showPage(dlg){
  for (const d of document.querySelectorAll("dialog.as-page")) if (d !== dlg) d.close();
  ensurePageChrome(dlg);
  dlg.classList.add("as-page");
  if (!dlg.open) dlg.show();
  dlg.scrollTop = 0;
  dlg.querySelector("[data-kind]").textContent = (dlg.id === "dlgCTM" ? "container " : "VM ") + pageName(dlg);
  // a different guest: drop the old console
  for (const f of dlg.querySelectorAll(".console-frame")) if (f.dataset.for !== pageName(dlg)) { f.innerHTML = ""; f.dataset.for = ""; }
  selectTab(dlg, pageTab[dlg.id]);
  pageInfo(dlg);
  markSide();
}

function selectTab(dlg, tab){
  pageTab[dlg.id] = tab;
  for (const b of dlg.querySelectorAll("[data-ptab]")) b.classList.toggle("on", b.dataset.ptab === tab);
  for (const el of dlg.querySelectorAll(".dlg-body > [data-tab]")) el.hidden = el.dataset.tab !== tab;
  const frame = dlg.querySelector(`[data-tab="console"] .console-frame`), name = pageName(dlg);
  if (tab === "console") {
    if (frame.dataset.for !== name || !frame.firstChild) {
      const src = dlg.id === "dlgCTM" ? `/terminal?ct=${encodeURIComponent(name)}` : `/console?vm=${encodeURIComponent(name)}`;
      frame.innerHTML = `<iframe title="Console of ${esc(name)}" src="${src}" allow="clipboard-read; clipboard-write"></iframe>`;
      frame.dataset.for = name;
    }
  } else if (frame.firstChild) { frame.innerHTML = ""; frame.dataset.for = ""; } // closes the connection
}

function markSide(){
  const open = document.querySelector("dialog.as-page[open]");
  const name = open ? pageName(open) : "", kind = open?.id === "dlgCTM" ? "ct" : "vm";
  for (const b of document.querySelectorAll("#side .tree-vm")) {
    b.classList.toggle("on", !!open && (kind === "ct" ? b.dataset.ct === name : b.dataset.vm === name));
  }
  $("#bHome")?.classList.toggle("on", !open);
}

const kv = (k, v) => v === "" || v == null ? "" : `<div><b>${k}</b><span>${v}</span></div>`;
const when = t => { const d = new Date(t); return isNaN(d) ? esc(t || "") : esc(d.toLocaleString()); };

// toolbar and summary facts, refreshed with every poll
function pageInfo(dlg){
  if (!dlg?.classList.contains("as-page") || !dlg.open) return;
  const bar = dlg.querySelector("[data-bar]");
  if (dlg.id === "dlgManage") {
    const v = vms.find(x => x.name === mName); if (!v) return;
    const b = busy.has(v.name) ? "disabled" : "";
    bar.dataset.n = v.name;
    bar.innerHTML = (v.state === "running"
      ? `<button class="btn primary" type="button" data-pt="console">Console</button><button class="btn" data-a="stop" ${b}>Shutdown</button>` +
        `<button class="btn" data-a="reboot" ${b}>Reboot</button><button class="btn" data-a="kill" ${b}>Force off</button>`
      : `<button class="btn primary" data-a="start" ${b}>Start</button>`) + `<button class="btn danger" data-a="delete" ${b}>Delete…</button>`;
    const os = v.os_type === "windows11" ? "Windows 11 (UEFI, TPM)" : v.os_type === "windows" ? "Windows" : "Linux";
    const tpl = templates.find(t => t.name === v.template)?.label || v.template.replace(/^iso:/, "installed from ");
    $("#mInfo").innerHTML = kv("State", `<span class="state ${esc(v.state)}"><span class="dot"></span>${esc(v.state)}</span>`) +
      kv("IP address", v.ip ? `<span class="mono copy" data-copy="${esc(v.ip)}">${esc(v.ip)}</span>` : "—") +
      kv("Public IP", v.public ? `<span class="mono">${esc(v.public.ip)}</span>` : "") +
      kv("SSH", typeof sshLine === "function" ? sshLine(v.name, v.user).replace(/^<div[^>]*>|<\/div>$/g, "") || "" : "") +
      kv("System", esc(os) + (tpl ? " · " + esc(tpl) : "")) +
      kv("Size", `${v.vcpus} CPU · ${gb(v.memory_mib)} RAM · ${v.disk_gb} GB disk`) +
      kv("Network", v.public ? "public IP" : v.network === "lan" ? (v.vlan ? "home network, VLAN " + v.vlan : "home network") : "private (NAT)") +
      kv("Server", v.node ? esc(v.node) + (v.node_id ? ` <span class="sub">${esc(v.node_id)}</span>` : "") : "") +
      kv("Owner", isAdmin() ? esc(v.owner || "admins only") : "") +
      kv("Created", v.created_at ? when(v.created_at) : "") +
      kv("Start with the server", v.autostart ? "yes" : "no");
  } else {
    const c = (typeof cts !== "undefined" ? cts : []).find(x => x.name === ctCur); if (!c) return;
    for (const btn of dlg.querySelectorAll('#cmPower [data-cm="console"]')) btn.onclick = () => selectTab(dlg, "console");
    bar.hidden = true;
    const used = c.disk_used ? (c.disk_used < 107374182 ? "<0.1" : (c.disk_used / 1073741824).toFixed(1)) + " of " : "";
    $("#cmSummary").innerHTML = `<div class="kv">` + kv("State", `<span class="state ${esc(c.state)}"><span class="dot"></span>${esc(c.state)}</span>`) +
      kv("IP address", c.ip ? `<span class="mono copy" data-copy="${esc(c.ip)}">${esc(c.ip)}</span>` : "—") +
      kv("Public IP", c.public_ip ? `<span class="mono">${esc(c.public_ip)}</span>` : "") +
      kv("SSH", typeof sshLine === "function" ? sshLine(c.name, "root").replace(/^<div[^>]*>|<\/div>$/g, "") || "" : "") +
      kv("System", esc(typeof ctTplLabel === "function" ? ctTplLabel(c.template) : c.template)) +
      kv("Size", `${c.vcpus} CPU · ${gb(c.memory_mib)} RAM · ${used}${c.disk_gb} GB disk`) +
      kv("Network", c.public_ip ? "public IP" : c.network === "lan" ? "home network" : "private (NAT)") +
      kv("Owner", isAdmin() ? esc(c.owner || "admins only") : "") +
      kv("Created", c.created ? when(c.created) : "") +
      kv("Start with the server", c.autostart ? "yes" : "no") + `</div>`;
  }
}
document.addEventListener("click", e => {
  const b = e.target.closest("[data-pt]");
  if (b) { const dlg = b.closest("dialog"); if (dlg) selectTab(dlg, b.dataset.pt); }
  if (e.target.closest("#bHome")) for (const d of document.querySelectorAll("dialog.as-page[open]")) d.close();
});

// keep the page in step with each redraw
const _renderManage = renderManage;
renderManage = function(){ _renderManage(); pageInfo($("#dlgManage")); };
if (typeof renderCTManage === "function") {
  const _renderCTManage = renderCTManage;
  renderCTManage = function(){ _renderCTManage(); pageInfo($("#dlgCTM")); };
}
const _renderVMsPage = renderVMs;
renderVMs = function(){ _renderVMsPage(); markSide(); };

/* ---------- task bar ---------- */
document.body.insertAdjacentHTML("beforeend", `<div id="taskbar" hidden>
  <div class="tb-head" id="tbHead" role="button" tabindex="0" aria-expanded="false"><b>Tasks</b><span class="tb-last" id="tbLast">nothing yet</span><span class="sub" id="tbToggle">▲</span></div>
  <div class="tb-list" id="tbList" hidden><table><tbody id="tbBody"></tbody></table></div></div>`);
let tbOpen = false, tbTick = 0, nodeTasks = [];
$("#tbHead").onclick = () => {
  tbOpen = !tbOpen;
  $("#tbList").hidden = !tbOpen; $("#tbToggle").textContent = tbOpen ? "▼" : "▲";
  $("#tbHead").setAttribute("aria-expanded", tbOpen);
  if (tbOpen) loadTasks(true);
};
$("#tbHead").onkeydown = e => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); $("#tbHead").click(); } };

const KIND = {backup: "Backup", restore: "Restore", "ct-backup": "Container backup", "ct-restore": "Container restore",
  "iso-download": "ISO download", "offsite-sync": "Off-site copy", "offsite-fetch": "Off-site fetch"};
async function loadTasks(force){
  if (!token || !$("#vmBody")) { $("#taskbar").hidden = true; return; }
  $("#taskbar").hidden = false;
  tbTick++;
  let jobs = [];
  try { jobs = (await call("GET", "/jobs")).jobs || []; } catch {}
  if (isAdmin() && (force || tbOpen || tbTick % 3 === 1)) {
    try { nodeTasks = (await call("GET", "/node/tasks")).tasks || []; } catch { nodeTasks = []; }
  }
  const rows = jobs.map(j => ({t: j.started_at, what: `${KIND[j.kind] || j.kind} · ${j.target.replace(/^offsite:/, "")}`,
      st: j.state, msg: j.state === "failed" ? j.error : j.message}))
    .concat(nodeTasks.map(t => ({t: t.started, what: t.title || t.kind, st: t.state === "ok" ? "done" : t.state, msg: t.by ? "by " + t.by : ""})))
    .filter(r => r.t).sort((a, b) => (b.t > a.t ? 1 : -1)).slice(0, 40);
  const running = rows.filter(r => r.st === "running").length, last = rows[0];
  $("#tbLast").textContent = running ? `${running} running — ${rows.find(r => r.st === "running").what}`
    : last ? `${last.what}: ${last.st}${last.msg ? " — " + last.msg : ""}` : "nothing yet";
  if (tbOpen) $("#tbBody").innerHTML = rows.length ? rows.map(r => `<tr><td>${esc(new Date(r.t).toLocaleString())}</td><td>${esc(r.what)}</td>
      <td><span class="state ${r.st === "done" ? "running" : r.st === "failed" ? "stopped" : "starting"}"><span class="dot"></span>${esc(r.st)}</span></td>
      <td class="msg">${esc(r.msg || "")}</td></tr>`).join("") : `<tr><td class="msg">No tasks since p8sd started.</td></tr>`;
}
const _refreshPage = refresh;
refresh = async function(...a){ await _refreshPage(...a); loadTasks(); };
