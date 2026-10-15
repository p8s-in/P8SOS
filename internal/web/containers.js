// Containers (LXC) in the dashboard: list, sidebar entries, Create CT, Manage
// (resources, snapshots, password, owner) and the browser terminal.
// Uses the dashboard helpers ($, esc, call, toast, me, isAdmin, gb, ic, userList).
"use strict";

let cts = [], ctOn = false, ctBusy = new Set(), ctTpls = [], ctCur = "";

document.body.insertAdjacentHTML("beforeend", `
<dialog id="dlgCT" class="wide"><form method="dialog" id="fCT"><div class="dlg-body">
  <h2>Create container</h2>
  <div class="sub">A container shares this server's Linux kernel: it starts in a second and needs much less memory than a VM. Linux only.</div>
  <label for="ctName">Name</label><input id="ctName" required pattern="[a-z][a-z0-9\\-]{0,47}[a-z0-9]" placeholder="web2" autocomplete="off">
  <label for="ctTpl">Template</label><select id="ctTpl"></select>
  <div class="hint" id="ctTplHint"></div>
  <div id="ctAddBox" hidden style="border:1px dashed var(--line);border-radius:8px;padding:10px;margin-top:8px">
    <div class="sub" style="margin-bottom:6px"><b>Add a template</b> — downloaded once (50-200 MB), then every container starts from it in a second.</div>
    <div class="inline" style="flex-wrap:wrap;margin-top:0"><select id="ctAddSel" style="flex:1;min-width:180px"></select><button class="btn primary" type="button" id="ctAddGo">Add</button></div>
    <div class="hint" id="ctAddStatus"></div>
  </div>
  <div id="ctPlanBox" hidden><label>Size and price</label><div id="ctPlans" class="plans"></div></div>
  <div class="row" id="ctSizeRow">
    <div><label for="ctCPU">CPUs</label><input id="ctCPU" type="number" min="1" value="1"></div>
    <div><label for="ctRAM">RAM (MB)</label><input id="ctRAM" type="number" min="64" step="64" value="512"></div>
    <div><label for="ctDisk">Disk (GB)</label><input id="ctDisk" type="number" min="1" value="8"></div>
  </div>
  <label for="ctNet">Network</label><select id="ctNet"><option value="nat">Private (NAT) — reaches the internet</option></select>
  <div id="ctNodeBox" hidden><label for="ctNode">Server</label><select id="ctNode"></select>
    <div class="hint">Automatic: the server with the most free memory that has the template.</div></div>
  <label for="ctPw">Root password</label><input id="ctPw" type="password" minlength="8" autocomplete="new-password" placeholder="leave empty: P8S makes one and shows it">
  <label for="ctKeys">SSH public keys (one per line)</label><textarea id="ctKeys" placeholder="ssh-ed25519 AAAA…"></textarea>
  <div id="ctOwnerBox" hidden><label for="ctOwner">Owner</label><select id="ctOwner"></select></div>
  <label style="display:flex;gap:8px;align-items:center;font-weight:400"><input type="checkbox" id="ctStart" checked style="width:auto"> Start after creating</label>
  <div class="dlg-actions"><button class="btn" value="cancel" formnovalidate>Cancel</button><button class="btn primary" id="ctGo" value="ok">Create</button></div>
</div></form></dialog>

<dialog id="dlgCTM" class="wide"><div class="dlg-body">
  <h2 id="cmTitle"></h2><div class="sub" id="cmSub"></div>
  <div class="inline" id="cmPower" style="flex-wrap:wrap"></div>
  <div data-tab="summary"><h3>Login</h3><div id="cmLogin" class="sub">…</div></div>
  <div data-tab="resources">
  <h3>Resources</h3>
  <div class="row">
    <div><label for="cmCPU">CPUs</label><input id="cmCPU" type="number" min="1"></div>
    <div><label for="cmRAM">RAM (MB)</label><input id="cmRAM" type="number" min="64" step="64"></div>
    <div><label for="cmDisk">Disk (GB)</label><input id="cmDisk" type="number" min="1"></div>
  </div>
  <label style="display:flex;gap:8px;align-items:center;font-weight:400"><input type="checkbox" id="cmAuto" style="width:auto"> Start with the server</label>
  <div class="hint">Changes apply at once, also while it runs. The disk can shrink only down to what's used.</div>
  <div class="dlg-actions" style="justify-content:flex-start"><button class="btn primary" id="cmSave">Save resources</button></div>
  </div>
  <div data-tab="snapshots">
  <h3>Snapshots</h3>
  <div class="inline"><input id="cmSnapName" placeholder="name (optional)" style="flex:1"><button class="btn" id="cmSnap">Take snapshot</button></div>
  <table class="snaps"><tbody id="cmSnaps"></tbody></table>
  </div>
  <div data-tab="backups">
  <h3>Backups</h3>
  <div class="sub">A full copy of the disk in the backup folder (works while it runs). Comes back as a new container.</div>
  <div class="inline">
    <label style="display:flex;gap:6px;align-items:center;font-weight:400;margin:0;white-space:nowrap"><input id="cmAutoB" type="checkbox" style="width:auto"> Every day at <span id="cmAutoAt">02:30</span>, keep</label>
    <input id="cmKeep" type="number" min="1" max="365" value="7" style="flex:0 0 64px" aria-label="Backups to keep">
    <span class="spacer"></span><button class="btn" id="cmBackup">Back up now</button></div>
  <table class="snaps"><tbody id="cmBackups"></tbody></table>
  </div>
  <div data-tab="network">
  <div id="cmFwBox"></div>
  </div>
  <div data-tab="options">
  <h3>SSH</h3>
  <div class="inline"><span class="sub" style="flex:1" id="cmSSHHint">The SSH server is installed by itself on the first start (needs internet). If SSH doesn't answer, install or repair it here.</span>
    <button class="btn" id="cmSSH">Install / repair SSH</button></div>
  <pre id="cmSSHOut" class="mono" hidden style="max-height:180px;overflow:auto;background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:8px;font-size:12px;white-space:pre-wrap"></pre>
  <h3>Password</h3>
  <div class="inline"><input id="cmUser" value="root" style="flex:0 0 120px" aria-label="User"><input id="cmPw" type="password" minlength="8" placeholder="new password" autocomplete="new-password" style="flex:1"><button class="btn" id="cmPwGo">Set</button></div>
  <div id="cmOwnerBox" hidden><h3>Owner</h3><div class="inline"><select id="cmOwner" style="flex:1"></select><button class="btn" id="cmOwnerGo">Change</button></div></div>
  <div id="cmPubBox" hidden><h3>Public IP addresses</h3><div id="cmPub"></div></div>
  <div id="cmMoveBox" hidden><h3>Move to another server</h3>
    <div class="inline"><select id="cmMoveTo" style="flex:1"></select>
      <label style="display:flex;gap:6px;align-items:center;font-weight:400;margin:0"><input type="checkbox" id="cmMoveStart" style="width:auto" checked> start it there</label>
      <button class="btn" id="cmMoveGo">Move</button></div>
    <div class="hint">Shut it down first. Its disk is copied over the cluster tunnel, then deleted here. Snapshots and backups stay behind.</div></div>
  </div>
  <div data-tab="summary" class="page-only"><div id="cmSummary"></div></div>
  <div data-tab="console" class="page-only"><div class="console-frame" id="cmConsole"></div></div>
  <div class="dlg-actions"><button class="btn danger" id="cmDel">Delete container…</button><span class="spacer"></span><button class="btn" id="cmClose">Close</button></div>
</div></dialog>`);

const ctFw = fwPanel($("#cmFwBox"), () => `/containers/${encodeURIComponent(ctCur)}`, () => ctCur, "container");

/* ---------- data ---------- */
async function loadCTs(){
  try { const r = await call("GET", "/containers"); cts = r.containers || []; ctOn = !!r.enabled; }
  catch { cts = []; ctOn = false; }
}

function ctMatches(c){
  const q = ($("#vmFilter")?.value || "").trim().toLowerCase();
  return !q || (c.name + " " + (c.ip || "") + " " + (c.owner || "") + " " + c.template).toLowerCase().includes(q);
}

/* ---------- list + sidebar ---------- */
function renderCTs(){
  if (!$("#ctBody")) return;
  $("#bNewCT").hidden = !ctOn;
  if (!$("#bNewCT").onclick) $("#bNewCT").onclick = openCreateCT;
  $("#ctPanel").hidden = !ctOn || (!cts.length && !isAdmin());
  const list = cts.filter(ctMatches);
  $("#sideCTs").innerHTML = list.map(c => `<button class="tree-vm ${esc(c.state)}" data-ct="${esc(c.name)}" title="Container ${esc(c.name)} — ${esc(c.state)}"><span class="dot"></span>${ic("box")}<span class="nm">${esc(c.name)}</span></button>`).join("");
  const run = cts.filter(c => c.state === "running").length;
  $("#ctCount").textContent = cts.length ? `${run} running · ${cts.length} total` : "";
  if (!cts.length) { $("#ctBody").innerHTML = `<tr><td colspan="8" class="empty">No containers yet. Click <b>+ Create CT</b> for a light Linux server.</td></tr>`; return; }
  if (!list.length) { $("#ctBody").innerHTML = `<tr><td colspan="8" class="empty">No container matches.</td></tr>`; return; }
  $("#ctBody").innerHTML = list.map(c => {
    const b = ctBusy.has(c.name), dis = b ? "disabled" : "";
    const main = c.state === "running"
      ? `<button class="btn" data-c="console" ${dis}>Console</button><button class="btn" data-c="stop" ${dis}>Shutdown</button>`
      : `<button class="btn" data-c="start" ${dis}>Start</button>`;
    const more = `<details class="menu"><summary class="btn" title="More actions">⋯</summary><div class="menu-list">
      ${c.state === "running" ? `<button class="btn" data-c="reboot">Reboot</button><button class="btn" data-c="kill">Force off</button>` : ""}
      <button class="btn danger" data-c="delete">Delete…</button></div></details>`;
    const ip = c.ip ? `<span class="mono copy" data-copy="ssh root@${esc(c.ip)}" title="Click to copy the SSH command">${esc(c.ip)}</span>` : `<span class="muted">${c.state === "running" ? "starting…" : "—"}</span>`;
    const used = c.disk_used ? (c.disk_used < 107374182 ? "<0.1" : (c.disk_used / 1073741824).toFixed(1)) + " of " : "";
    return `<tr data-ct="${esc(c.name)}">
      <td data-k="State"><span class="state ${esc(c.state)}"><span class="dot"></span>${b ? "working…" : esc(c.state)}</span></td>
      <td data-k="Name"><b>${esc(c.name)}</b><div class="sub">container · ${esc(ctTplLabel(c.template))}${c.network === "lan" ? " · home network" : (c.network || "").startsWith("vnet:") ? " · " + esc(c.network.slice(5)) : ""}${c.node && cts.some(x => x.node && x.node !== c.node) ? " · on " + esc(c.node) : ""}${c.public_ip ? ` · public <span class="mono">${esc(c.public_ip)}</span>` : ""}${isAdmin() && c.owner ? ` · <b>${esc(c.owner)}</b>` : ""}</div></td>
      <td data-k="IP">${ip}${typeof sshLine === "function" ? sshLine(c.name, "root") : ""}</td><td data-k="CPU">${c.vcpus}</td><td data-k="RAM">${ramCell(c.mem_used_mib, c.memory_mib)}</td>
      <td data-k="Disk">${used}${c.disk_gb} GB</td><td data-k="Autostart">${c.autostart ? "yes" : "no"}</td>
      <td><div class="actions">${main}<button class="btn" data-c="manage" ${dis}>Manage</button>${more}</div></td></tr>`;
  }).join("");
}

function ctTplLabel(n){ return ctTpls.find(t => t.name === n)?.label || n; }

document.addEventListener("click", async e => {
  const tree = e.target.closest("[data-ct].tree-vm");
  if (tree) return openCTManage(tree.dataset.ct);
  const btn = e.target.closest("button[data-c]");
  if (!btn) return;
  btn.closest("details.menu")?.removeAttribute("open");
  const name = btn.closest("tr").dataset.ct, a = btn.dataset.c;
  if (a === "manage") return openCTManage(name);
  if (a === "delete") return ctDelete(name);
  if (a === "console") return ctConsole(name);
  await ctPower(name, a);
});

async function ctPower(name, a){
  const path = {start: "start", stop: "stop", reboot: "reboot", kill: "stop"}[a];
  ctBusy.add(name); renderCTs();
  try { await call("POST", `/containers/${encodeURIComponent(name)}/${path}`, a === "kill" ? {force: true} : undefined); toast(`${name}: ${a === "kill" ? "forced off" : a + (a.endsWith("p") ? "ped" : "ed")}`); }
  catch (err) { toast(err.message, true); }
  finally { ctBusy.delete(name); await loadCTs(); renderCTs(); if ($("#dlgCTM").open && ctCur === name) renderCTManage(); }
}

function ctConsole(name){ window.open(`/terminal?ct=${encodeURIComponent(name)}`, `p8s-term-${name}`); }

async function ctDelete(name){
  if (prompt(`Delete container "${name}" with its disk and snapshots? This can't be undone.\nType the name to confirm:`) !== name) return;
  try { await call("DELETE", `/containers/${encodeURIComponent(name)}`); toast(`${name} deleted`); $("#dlgCTM").close(); }
  catch (err) { toast(err.message, true); }
  await loadCTs(); renderCTs();
}

/* ---------- create ---------- */
async function loadCTTemplates(){
  try { ctTpls = (await call("GET", "/ct-templates")).templates; } catch { ctTpls = []; }
  const cur = $("#ctTpl").value;
  $("#ctTpl").innerHTML = ctTpls.map(t => `<option value="${esc(t.name)}" ${t.name === cur ? "selected" : ""}>${esc(t.label)}</option>`).join("");
  $("#ctTplHint").innerHTML = ctTpls.length ? "" : (isAdmin()
    ? "No container templates on this server yet: add one below."
    : "No container templates on this server yet; ask your provider.");
  $("#ctGo").disabled = !ctTpls.length;
  if (isAdmin()) await loadCTCatalog();
}

// admins add templates right here (a Server task: download, check, unpack)
let ctAddTimer = null;
async function loadCTCatalog(){
  let n;
  try { n = await call("GET", "/node"); } catch { $("#ctAddBox").hidden = true; return; }
  const left = (n.ct_catalog || []).filter(c => !c.installed);
  $("#ctAddBox").hidden = !n.containers || !left.length;
  $("#ctAddSel").innerHTML = left.map(c => `<option value="${esc(c.name)}">${esc(c.label)}${c.name === "debian-13" ? " (recommended)" : ""}</option>`).join("");
  const run = n.running;
  $("#ctAddGo").disabled = !!run;
  clearTimeout(ctAddTimer);
  if (run) {
    $("#ctAddStatus").textContent = run.kind === "ct-template" ? `Adding ${run.arg}… (downloading and unpacking; 1-5 minutes)` : `Waiting: "${run.title}" is running on the server.`;
    if ($("#dlgCT").open) ctAddTimer = setTimeout(loadCTTemplates, 3000);
  } else {
    const last = (n.tasks || []).find(t => t.kind === "ct-template");
    $("#ctAddStatus").innerHTML = last?.state === "failed"
      ? `<span style="color:var(--bad)">Adding ${esc(last.arg)} failed</span> — see <b>Server → Tasks → Log</b> (often no internet on the server).`
      : "";
  }
}

async function openCreateCT(){
  $("#fCT").reset();
  $("#ctAddStatus").textContent = "";
  await loadCTTemplates();
  const lan = isAdmin() ? !!host?.networks?.includes("lan") : !!me?.lan;
  $("#ctNet").innerHTML = `<option value="nat">Private (NAT) — reaches the internet</option>` + (lan ? `<option value="lan">Home network — address from your router</option>` : "");
  const pools = (await call("GET", "/ippools").catch(() => ({pools: []}))).pools || [];
  for (const p of pools) $("#ctNet").insertAdjacentHTML("beforeend",
    `<option value="pool:${esc(p.name)}" ${p.free ? "" : "disabled"}>Public IP — ${esc(p.name)} (${p.free} free)</option>`);
  if (typeof vnetOptions === "function") await vnetOptions($("#ctNet"), "Virtual network");
  let servers = [];
  if (isAdmin()) { try { const c = await call("GET", "/cluster"); if (c.role === "master") servers = c.nodes || []; } catch {} }
  $("#ctNodeBox").hidden = servers.length < 2;
  $("#ctNode").innerHTML = `<option value="">Automatic</option>` + servers.map(n => `<option value="${esc(n.id)}">${esc(n.name)}</option>`).join("");
  await loadCTPlans();
  $("#ctKeys").value = !isAdmin() && me?.ssh_keys?.length ? me.ssh_keys.join("\n") : "";
  $("#ctOwnerBox").hidden = !(isAdmin() && me?.accounts_on);
  if (!$("#ctOwnerBox").hidden) {
    try { userList = (await call("GET", "/users")).users; } catch {}
    $("#ctOwner").innerHTML = `<option value="">Admins only</option>` + (userList || []).filter(u => u.role !== "admin").map(u => `<option>${esc(u.name)}</option>`).join("");
  }
  $("#dlgCT").showModal(); $("#ctName").focus();
}

$("#ctAddGo").onclick = async () => {
  const name = $("#ctAddSel").value;
  if (!name) return;
  try {
    await call("POST", "/node/tasks", {kind: "ct-template", arg: name});
    toast("Adding " + name + " — you can keep this window open");
  } catch (err) { toast(err.message, true); }
  loadCTTemplates();
};
$("#dlgCT").addEventListener("close", () => clearTimeout(ctAddTimer));

/* P8S Cloud plans: the same sizes and prices as VMs */
let ctPlanList = [], ctPlan = null;
async function loadCTPlans(){
  ctPlan = null;
  try { const r = await call("GET", "/plans"); ctPlanList = r.plans || []; if (r.currency && typeof cloudCur !== "undefined") cloudCur = r.currency; }
  catch { ctPlanList = []; }
  $("#ctPlanBox").hidden = !ctPlanList.length;
  if (!ctPlanList.length) { $("#ctSizeRow").hidden = false; return; }
  $("#ctPlans").innerHTML = ctPlanList.map(p => `<div class="plan" data-ctplan="${esc(p.id)}" tabindex="0">
      <b>${esc(p.name)}${p.hidden ? ' <span class="pill">hidden</span>' : ""}</b>
      <div class="price">${esc(money(p.price_monthly))}<span class="sub">/mo</span></div>
      <div class="sub">${p.vcpus} CPU · ${gb(p.memory_mib)} · ${p.disk_gb} GB${p.public_ip ? " · public IP" : ""}</div></div>`).join("")
    + (isAdmin() ? `<div class="plan" data-ctplan="" tabindex="0"><b>Custom</b><div class="sub">any size, not billed</div></div>` : "");
  pickCTPlan(isAdmin() ? "" : ctPlanList[0].id);
}
function pickCTPlan(id){
  ctPlan = ctPlanList.find(p => p.id === id) || null;
  for (const el of document.querySelectorAll("#ctPlans .plan")) el.classList.toggle("sel", el.dataset.ctplan === (ctPlan?.id || ""));
  $("#ctSizeRow").hidden = !!ctPlan;
  $("#ctNet").disabled = !!ctPlan?.public_ip;
}
document.addEventListener("click", e => { const p = e.target.closest("#ctPlans .plan"); if (p) pickCTPlan(p.dataset.ctplan); });

$("#fCT").addEventListener("submit", async e => {
  if (e.submitter?.value !== "ok") return;
  e.preventDefault();
  const body = {name: $("#ctName").value.trim(), template: $("#ctTpl").value, vcpus: +$("#ctCPU").value, memory_mib: +$("#ctRAM").value,
    disk_gb: +$("#ctDisk").value, ...($("#ctNet").value.startsWith("pool:") ? {ip_pool: $("#ctNet").value.slice(5)} : {network: $("#ctNet").value}), root_password: $("#ctPw").value,
    ssh_keys: $("#ctKeys").value.split("\n").map(s => s.trim()).filter(Boolean), start: $("#ctStart").checked};
  if (!$("#ctOwnerBox").hidden && $("#ctOwner").value) body.owner = $("#ctOwner").value;
  if (!$("#ctNodeBox").hidden && $("#ctNode").value) body.node = $("#ctNode").value;
  if (ctPlan) { body.plan = ctPlan.id; delete body.vcpus; delete body.memory_mib; delete body.disk_gb; if (ctPlan.public_ip) { delete body.ip_pool; delete body.network; } }
  $("#ctGo").disabled = true;
  try {
    const r = await call("POST", "/containers", body);
    $("#dlgCT").close();
    toast(r.warning ? r.warning : `${body.name} created${body.start ? " and starting" : ""}`, !!r.warning);
    if (r.login) showLogin(body.name, r.login, "ct");
    await loadCTs(); renderCTs();
  } catch (err) { toast(err.message, true); }
  finally { $("#ctGo").disabled = false; }
});

/* ---------- manage ---------- */
async function openCTManage(name){
  ctCur = name;
  $("#cmSSHOut").hidden = true;
  if (typeof loadLogin === "function") loadLogin($("#cmLogin"), name, "ct");
  if (!ctTpls.length) { try { ctTpls = (await call("GET", "/ct-templates")).templates; } catch {} }
  renderCTManage();
  showPage($("#dlgCTM"));
  loadCTSnaps(); loadCTBackups(); ctFw.load();
  $("#cmOwnerBox").hidden = !(isAdmin() && me?.accounts_on);
  if (!$("#cmOwnerBox").hidden) {
    try { userList = (await call("GET", "/users")).users; } catch {}
    const c = cts.find(x => x.name === name);
    $("#cmOwner").innerHTML = `<option value="">Admins only</option>` + (userList || []).filter(u => u.role !== "admin").map(u => `<option ${u.name === c?.owner ? "selected" : ""}>${esc(u.name)}</option>`).join("");
  }
  $("#cmMoveBox").hidden = true;
  if (isAdmin()) {
    try {
      const cl = await call("GET", "/cluster"), c = cts.find(x => x.name === name);
      const others = cl.role === "master" ? (cl.nodes || []).filter(n => n.id !== c?.node_id) : [];
      $("#cmMoveBox").hidden = !others.length;
      $("#cmMoveTo").innerHTML = others.map(n => `<option value="${esc(n.id)}">${esc(n.name)}</option>`).join("");
    } catch {}
  }
}
$("#cmMoveGo").onclick = async () => {
  const to = $("#cmMoveTo").value; if (!to) return;
  try {
    const j = await call("POST", `/containers/${encodeURIComponent(ctCur)}/move`, {to, start: $("#cmMoveStart").checked});
    toast(`Moving ${ctCur} …`);
    if (typeof trackJob === "function") trackJob(j, async () => { await loadCTs(); renderCTs(); renderCTManage(); });
  } catch (err) { toast(err.message, true); }
};

function renderCTManage(){
  const c = cts.find(x => x.name === ctCur);
  if (!c) return;
  $("#cmTitle").textContent = c.name;
  $("#cmSub").innerHTML = `Container · ${esc(ctTplLabel(c.template))}${c.node ? " · on " + esc(c.node) : ""} · <span class="state ${esc(c.state)}"><span class="dot"></span>${esc(c.state)}</span>${c.ip ? ` · <span class="mono">${esc(c.ip)}</span>` : ""}`;
  $("#cmPower").innerHTML = c.state === "running"
    ? `<button class="btn" data-cm="console">Console</button><button class="btn" data-cm="stop">Shutdown</button><button class="btn" data-cm="reboot">Reboot</button><button class="btn" data-cm="kill">Force off</button>`
    : `<button class="btn primary" data-cm="start">Start</button>`;
  for (const b of $("#cmPower").querySelectorAll("[data-cm]")) b.onclick = () => b.dataset.cm === "console" ? ctConsole(c.name) : ctPower(c.name, b.dataset.cm);
  $("#cmCPU").value = c.vcpus; $("#cmRAM").value = c.memory_mib; $("#cmDisk").value = c.disk_gb; $("#cmAuto").checked = c.autostart;
  $("#cmPubBox").hidden = !c.public;
  if (c.public && typeof renderPublicIPs === "function") {
    const key = JSON.stringify(c.public) + c.state;
    if ($("#cmPub").dataset.key !== key) {
      $("#cmPub").dataset.key = key;
      renderPublicIPs($("#cmPub"), "containers", c.name, c.public, {onChanged: async () => { $("#cmPub").dataset.key = ""; await loadCTs(); renderCTManage(); }});
    }
  }
}

async function loadCTSnaps(){
  try {
    const l = (await call("GET", `/containers/${encodeURIComponent(ctCur)}/snapshots`)).snapshots;
    $("#cmSnaps").innerHTML = l.length ? l.slice().reverse().map(s => `<tr><td><b>${esc(s.name)}</b><div class="sub">${esc(new Date(s.created).toLocaleString())}</div></td>
      <td style="text-align:right"><button class="btn" data-sr="${esc(s.name)}">Roll back</button> <button class="btn danger" data-sd="${esc(s.name)}">Delete</button></td></tr>`).join("")
      : `<tr><td class="sub">No snapshots yet. A snapshot is instant and lets you undo changes later.</td></tr>`;
  } catch (err) { $("#cmSnaps").innerHTML = `<tr><td class="sub">${esc(err.message)}</td></tr>`; }
}

async function loadCTBackups(){
  call("GET", `/containers/${encodeURIComponent(ctCur)}/backup-schedule`).then(sch => {
    $("#cmAutoB").checked = sch.daily; $("#cmKeep").value = sch.keep; $("#cmAutoAt").textContent = sch.at || "";
  }).catch(() => {});
  try {
    const l = (await call("GET", `/containers/${encodeURIComponent(ctCur)}/backups`)).backups;
    $("#cmBackups").innerHTML = l.length ? l.map(b => `<tr data-id="${esc(b.id)}"><td><b>${esc(new Date(b.created_at).toLocaleString())}</b>
      <div class="sub">${fmtBytes(b.bytes)}${b.consistent ? "" : " · taken while busy (crash-consistent)"}</div></td>
      <td style="text-align:right"><button class="btn" data-br>Restore as new…</button> <button class="btn danger" data-bd>Delete</button></td></tr>`).join("")
      : `<tr><td class="sub">No backups yet.</td></tr>`;
  } catch (err) { $("#cmBackups").innerHTML = `<tr><td class="sub">${esc(err.message)}</td></tr>`; }
}
async function saveCTSchedule(){
  const name = ctCur;
  try {
    const s = await call("PUT", `/containers/${encodeURIComponent(name)}/backup-schedule`, {daily: $("#cmAutoB").checked, keep: +$("#cmKeep").value || 7});
    toast(s.daily ? `${name}: daily backup at ${s.at}, keeping ${s.keep}` : `${name}: daily backup off`);
  } catch (err) { toast(err.message, true); }
}
$("#cmAutoB").onchange = saveCTSchedule;
$("#cmKeep").onchange = () => { if ($("#cmAutoB").checked) saveCTSchedule(); };
$("#cmBackup").onclick = async () => {
  const name = ctCur;
  try { const j = await call("POST", `/containers/${encodeURIComponent(name)}/backups`); trackJob(j, () => { if (ctCur === name) loadCTBackups(); }); }
  catch (err) { toast(err.message, true); }
};
$("#cmBackups").addEventListener("click", async e => {
  const tr = e.target.closest("tr[data-id]"); if (!tr) return;
  const path = `/ct-backups/${encodeURIComponent(ctCur)}/${encodeURIComponent(tr.dataset.id)}`;
  try {
    if (e.target.closest("[data-br]")) {
      const n = prompt("Name for the restored container:", ctCur + "-restored");
      if (!n) return;
      trackJob(await call("POST", path + "/restore", {name: n.trim()}));
    } else if (e.target.closest("[data-bd]")) {
      if (!confirm("Delete this backup? It can't be undone.")) return;
      await call("DELETE", path); loadCTBackups();
    }
  } catch (err) { toast(err.message, true); }
});

$("#cmSnaps").addEventListener("click", async e => {
  const r = e.target.closest("[data-sr]"), d = e.target.closest("[data-sd]");
  const enc = encodeURIComponent(ctCur);
  try {
    if (r) {
      if (!confirm(`Roll ${ctCur} back to "${r.dataset.sr}"? Everything changed since is lost (newer snapshots too). The container must be stopped.`)) return;
      await call("POST", `/containers/${enc}/snapshots/${encodeURIComponent(r.dataset.sr)}/rollback`, {destroy_newer: true});
      toast("Rolled back to " + r.dataset.sr);
    } else if (d) {
      if (!confirm(`Delete snapshot "${d.dataset.sd}"?`)) return;
      await call("DELETE", `/containers/${enc}/snapshots/${encodeURIComponent(d.dataset.sd)}`);
    } else return;
  } catch (err) { toast(err.message, true); }
  loadCTSnaps();
});

$("#cmSnap").onclick = async () => {
  try { await call("POST", `/containers/${encodeURIComponent(ctCur)}/snapshots`, $("#cmSnapName").value.trim() ? {name: $("#cmSnapName").value.trim()} : undefined); $("#cmSnapName").value = ""; toast("Snapshot taken"); }
  catch (err) { toast(err.message, true); }
  loadCTSnaps();
};
$("#cmSave").onclick = async () => {
  try {
    await call("PATCH", `/containers/${encodeURIComponent(ctCur)}`, {vcpus: +$("#cmCPU").value, memory_mib: +$("#cmRAM").value, disk_gb: +$("#cmDisk").value, autostart: $("#cmAuto").checked});
    toast("Resources saved"); await loadCTs(); renderCTs(); renderCTManage();
  } catch (err) { toast(err.message, true); }
};
$("#cmPwGo").onclick = async () => {
  try { await call("POST", `/containers/${encodeURIComponent(ctCur)}/password`, {user: $("#cmUser").value.trim() || "root", password: $("#cmPw").value}); $("#cmPw").value = ""; toast("Password set"); }
  catch (err) { toast(err.message, true); }
};
$("#cmOwnerGo").onclick = async () => {
  try { await call("PUT", `/containers/${encodeURIComponent(ctCur)}/owner`, {owner: $("#cmOwner").value}); toast("Owner changed"); await loadCTs(); renderCTs(); }
  catch (err) { toast(err.message, true); }
};
$("#cmSSH").onclick = async () => {
  const b = $("#cmSSH"), out = $("#cmSSHOut");
  b.disabled = true; b.textContent = "Installing… (up to a few minutes)"; out.hidden = true;
  try {
    const r = await fetch(API + `/containers/${encodeURIComponent(ctCur)}/setup-ssh`, {method: "POST", headers: {"Authorization": "Bearer " + token}});
    const j = await r.json().catch(() => ({}));
    out.textContent = (j.output || "").trim().split("\n").slice(-25).join("\n"); out.hidden = !out.textContent;
    if (r.ok) toast("SSH server is ready"); else toast(j?.error?.message || "HTTP " + r.status, true);
  } catch (err) { toast(err.message, true); }
  finally { b.disabled = false; b.textContent = "Install / repair SSH"; }
};
$("#cmDel").onclick = () => ctDelete(ctCur);
$("#cmClose").onclick = () => $("#dlgCTM").close();

