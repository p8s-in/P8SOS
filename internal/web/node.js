// The Server page (admins): storage, OS templates, home network, Debian
// updates, reboot and the task log. Uses the dashboard helpers
// ($, esc, call, toast, isAdmin, API, token).
"use strict";

let node = null, nodeTimer = null, taskOpen = "";

document.body.insertAdjacentHTML("beforeend", `
<dialog id="dlgNode" class="wide"><div class="dlg-body">
  <h2>Server <span class="sub" id="ndHost"></span></h2>
  <div id="ndRunning" class="toast" style="max-width:none;box-shadow:none;margin:8px 0;border-left-color:var(--warn)" hidden></div>

  <h3>VM storage</h3>
  <div id="ndStorage"></div>

  <h3>OS templates</h3>
  <div class="sub">Images new VMs are made from. Adding one downloads it from the publisher (a few hundred MB) and checks its checksum.</div>
  <table class="snaps"><tbody id="ndTpl"></tbody></table>

  <h3>Home network</h3>
  <div id="ndLan"></div>

  <h3>Debian updates and power</h3>
  <div id="ndOS"></div>

  <h3>Tasks</h3>
  <table class="snaps"><tbody id="ndTasks"></tbody></table>
  <div id="ndLogBox" hidden><div class="inline"><b id="ndLogTitle"></b><span class="spacer"></span><button class="btn" id="ndLogClose">Hide log</button></div>
    <pre id="ndLog" class="mono" style="max-height:260px;overflow:auto;background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:8px;font-size:12px;white-space:pre-wrap"></pre></div>
  <div class="dlg-actions"><button class="btn" id="ndClose">Close</button></div>
</div></dialog>`);

const ndGB = b => (b / 1073741824).toFixed(b >= 10 * 1073741824 ? 0 : 1) + " GB";
const ndWhen = s => { try { return new Date(s).toLocaleString(); } catch { return s; } };
const ndState = {running: "⏳ running", ok: "✔ done", failed: "✘ failed"};

function nodeShell(){
  if (!isAdmin() || $("#bNode")) return;
  const b = document.createElement("button");
  b.className = "btn"; b.id = "bNode"; b.textContent = "Server";
  ($("#bSettings") || $("#bAudit")).after(b);
  b.onclick = openNode;
}

async function openNode(){
  taskOpen = "";
  $("#ndLogBox").hidden = true;
  $("#dlgNode").showModal();
  await loadNode();
}

async function loadNode(quiet){
  try {
    const r = await fetch(API + "/node", {headers: {"Authorization": "Bearer " + token}});
    if (!r.ok) throw new Error((await r.json().catch(() => ({})))?.error?.message || "HTTP " + r.status);
    node = await r.json();
  } catch (e) { if (!quiet) toast(e.message, true); } // p8sd may be restarting (template added)
  if (node) renderNode();
  if (taskOpen) await showTask(taskOpen, true);
  clearTimeout(nodeTimer);
  if ($("#dlgNode").open && (node?.running || !node)) nodeTimer = setTimeout(() => loadNode(true), 3000);
}

function renderNode(){
  const n = node, busy = !!n.running, dis = busy ? "disabled" : "";
  $("#ndHost").textContent = n.hostname ? "· " + n.hostname + (n.os.kernel ? " · kernel " + n.os.kernel : "") : "";
  $("#ndRunning").hidden = !busy;
  if (busy) $("#ndRunning").innerHTML = `⏳ <b>${esc(n.running.title)}${n.running.arg ? " " + esc(n.running.arg) : ""}</b> is running (started ${esc(ndWhen(n.running.started))}). Other server tasks wait until it finishes. <a href="#" id="ndRunLog">Show log</a>`;
  if (busy) $("#ndRunLog").onclick = e => { e.preventDefault(); showTask(n.running.id); };

  const s = n.storage;
  if (s.error) $("#ndStorage").innerHTML = `<div class="sub" style="color:var(--bad)">${esc(s.error)}</div>`;
  else {
    const pct = s.size ? s.used / s.size * 100 : 0;
    $("#ndStorage").innerHTML = `<div class="inline"><b>${esc(s.pool)}</b><span class="sub">${esc(s.health)} · ${ndGB(s.used)} used of ${ndGB(s.size)} · ${ndGB(s.free)} free</span></div>
      <div class="meter" style="margin:6px 0"><i style="width:${Math.min(100, pct).toFixed(0)}%"></i></div>
      <div class="inline">${s.file
        ? (s.can_grow >= 1073741824
          ? `<button class="btn" id="ndGrow" ${dis}>Grow by about ${ndGB(s.can_grow)}</button><span class="sub">uses the system disk's free space (keeps room for Debian); VMs keep running</span>`
          : `<span class="sub">The pool already uses the system disk's free space (${ndGB(s.system_free)} left for Debian).</span>`)
        : `<span class="sub">The pool is on its own disk; add a disk to get more space.</span>`}</div>`;
    if ($("#ndGrow")) $("#ndGrow").onclick = () => runTask("storage-grow", "", "Grow VM storage now? VMs keep running.");
  }

  $("#ndTpl").innerHTML = n.catalog.length ? n.catalog.map(c => `<tr><td><b>${esc(c.label)}</b><div class="sub mono">${esc(c.name)}</div></td>
    <td style="text-align:right">${c.installed ? `<span class="sub">✔ added</span>` : `<button class="btn" data-tpl="${esc(c.name)}" ${dis}>Add</button>`}</td></tr>`).join("")
    : `<tr><td class="sub">The template list is not available (make-template.sh missing?).</td></tr>`;
  for (const b of $("#ndTpl").querySelectorAll("[data-tpl]")) b.onclick = () => runTask("template", b.dataset.tpl,
    "Add " + b.dataset.tpl + "? It downloads a few hundred MB; the dashboard restarts for a moment when it's done.");

  $("#ndLan").innerHTML = n.lan
    ? `<div class="sub">✔ VMs can join your home network (choose "Home network" when creating a VM).</div>`
    : `<div class="inline"><button class="btn" id="ndLanGo" ${dis}>Set up</button><span class="sub">Lets VMs get addresses from your router, like other devices. Needs an Ethernet cable;
       the network drops for a few seconds and the old settings come back by themselves if anything goes wrong.</span></div>`;
  if ($("#ndLanGo")) $("#ndLanGo").onclick = () => runTask("lan-setup", "", "Set up the home network now? The server's network drops for a few seconds; if it doesn't come back within 60 s the old settings are restored.");

  const o = n.os;
  $("#ndOS").innerHTML = `<div class="sub">${o.checked
      ? `${o.upgradable ? `<b>${o.upgradable} update${o.upgradable > 1 ? "s" : ""}</b> available${o.security ? ` (${o.security} security)` : ""}` : "Debian is up to date"} · checked ${esc(ndWhen(o.checked))}`
      : "Not checked yet."}${o.reboot_required ? ` · <b style="color:var(--warn)">reboot needed</b> to finish earlier updates` : ""}</div>
    <div class="inline"><button class="btn" id="ndOSCheck" ${dis}>Check</button>
      <button class="btn primary" id="ndOSUp" ${dis} ${o.checked && !o.upgradable ? "hidden" : ""}>Install updates</button>
      <span class="spacer"></span><button class="btn danger" id="ndReboot" ${dis}>Reboot server</button></div>`;
  $("#ndOSCheck").onclick = () => runTask("os-check", "");
  $("#ndOSUp").onclick = () => runTask("os-upgrade", "", "Install Debian updates now? VMs keep running; it can take several minutes.");
  $("#ndReboot").onclick = () => runTask("reboot", "", "Reboot the server now? Every VM stops; VMs set to autostart come back when the server is up (about 1-3 minutes). You'll need to sign in again.");

  $("#ndTasks").innerHTML = n.tasks.length ? n.tasks.map(t => `<tr><td>${esc(t.title)}${t.arg ? ` <span class="mono">${esc(t.arg)}</span>` : ""}
      <div class="sub">${esc(ndWhen(t.started))} · ${esc(t.by)}</div></td>
      <td style="color:${t.state === "failed" ? "var(--bad)" : t.state === "ok" ? "var(--ok)" : "var(--warn)"}">${ndState[t.state] || esc(t.state)}${t.exit != null && t.exit !== 0 ? " (exit " + t.exit + ")" : ""}</td>
      <td style="text-align:right"><button class="btn" data-task="${esc(t.id)}">Log</button></td></tr>`).join("")
    : `<tr><td class="sub">No tasks yet.</td></tr>`;
  for (const b of $("#ndTasks").querySelectorAll("[data-task]")) b.onclick = () => showTask(b.dataset.task);
}

async function runTask(kind, arg, question){
  if (question && !confirm(question)) return;
  try {
    const t = await call("POST", "/node/tasks", {kind, arg});
    toast(t.title + " started");
    taskOpen = t.id;
    await loadNode();
  } catch (e) { toast(e.message, true); }
}

async function showTask(id, quiet){
  taskOpen = id;
  try {
    const r = await fetch(API + "/node/tasks/" + encodeURIComponent(id), {headers: {"Authorization": "Bearer " + token}});
    if (!r.ok) throw new Error("HTTP " + r.status);
    const t = await r.json();
    $("#ndLogBox").hidden = false;
    $("#ndLogTitle").textContent = t.title + (t.arg ? " " + t.arg : "") + " — " + (ndState[t.state] || t.state);
    const log = $("#ndLog"), atEnd = log.scrollTop + log.clientHeight >= log.scrollHeight - 20;
    log.textContent = t.log || "(no output yet)";
    if (atEnd || !quiet) log.scrollTop = log.scrollHeight;
  } catch (e) { if (!quiet) toast(e.message, true); }
}

$("#ndClose").onclick = () => { clearTimeout(nodeTimer); $("#dlgNode").close(); };
$("#ndLogClose").onclick = () => { taskOpen = ""; $("#ndLogBox").hidden = true; };
