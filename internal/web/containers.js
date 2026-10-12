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
  <label for="ctTpl">Template</label><select id="ctTpl" required></select>
  <div class="hint" id="ctTplHint"></div>
  <div class="row">
    <div><label for="ctCPU">CPUs</label><input id="ctCPU" type="number" min="1" value="1"></div>
    <div><label for="ctRAM">RAM (MB)</label><input id="ctRAM" type="number" min="64" step="64" value="512"></div>
    <div><label for="ctDisk">Disk (GB)</label><input id="ctDisk" type="number" min="1" value="8"></div>
  </div>
  <label for="ctNet">Network</label><select id="ctNet"><option value="nat">Private (NAT) — reaches the internet</option></select>
  <label for="ctPw">Root password</label><input id="ctPw" type="password" minlength="8" autocomplete="new-password" placeholder="at least 8 characters (or use an SSH key)">
  <label for="ctKeys">SSH public keys (one per line)</label><textarea id="ctKeys" placeholder="ssh-ed25519 AAAA…"></textarea>
  <div id="ctOwnerBox" hidden><label for="ctOwner">Owner</label><select id="ctOwner"></select></div>
  <label style="display:flex;gap:8px;align-items:center;font-weight:400"><input type="checkbox" id="ctStart" checked style="width:auto"> Start after creating</label>
  <div class="dlg-actions"><button class="btn" value="cancel" formnovalidate>Cancel</button><button class="btn primary" id="ctGo" value="ok">Create</button></div>
</div></form></dialog>

<dialog id="dlgCTM" class="wide"><div class="dlg-body">
  <h2 id="cmTitle"></h2><div class="sub" id="cmSub"></div>
  <div class="inline" id="cmPower" style="flex-wrap:wrap"></div>
  <h3>Resources</h3>
  <div class="row">
    <div><label for="cmCPU">CPUs</label><input id="cmCPU" type="number" min="1"></div>
    <div><label for="cmRAM">RAM (MB)</label><input id="cmRAM" type="number" min="64" step="64"></div>
    <div><label for="cmDisk">Disk (GB)</label><input id="cmDisk" type="number" min="1"></div>
  </div>
  <label style="display:flex;gap:8px;align-items:center;font-weight:400"><input type="checkbox" id="cmAuto" style="width:auto"> Start with the server</label>
  <div class="hint">Changes apply at once, also while it runs. The disk can shrink only down to what's used.</div>
  <div class="dlg-actions" style="justify-content:flex-start"><button class="btn primary" id="cmSave">Save resources</button></div>
  <h3>Snapshots</h3>
  <div class="inline"><input id="cmSnapName" placeholder="name (optional)" style="flex:1"><button class="btn" id="cmSnap">Take snapshot</button></div>
  <table class="snaps"><tbody id="cmSnaps"></tbody></table>
  <h3>Password</h3>
  <div class="inline"><input id="cmUser" value="root" style="flex:0 0 120px" aria-label="User"><input id="cmPw" type="password" minlength="8" placeholder="new password" autocomplete="new-password" style="flex:1"><button class="btn" id="cmPwGo">Set</button></div>
  <div id="cmOwnerBox" hidden><h3>Owner</h3><div class="inline"><select id="cmOwner" style="flex:1"></select><button class="btn" id="cmOwnerGo">Change</button></div></div>
  <div class="dlg-actions"><button class="btn danger" id="cmDel">Delete container…</button><span class="spacer"></span><button class="btn" id="cmClose">Close</button></div>
</div></dialog>`);

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
      <td data-k="Name"><b>${esc(c.name)}</b><div class="sub">container · ${esc(ctTplLabel(c.template))}${c.network === "lan" ? " · home network" : ""}${isAdmin() && c.owner ? ` · <b>${esc(c.owner)}</b>` : ""}</div></td>
      <td data-k="IP">${ip}</td><td data-k="CPU">${c.vcpus}</td><td data-k="RAM">${gb(c.memory_mib)}</td>
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
async function openCreateCT(){
  $("#fCT").reset();
  try { ctTpls = (await call("GET", "/ct-templates")).templates; } catch { ctTpls = []; }
  $("#ctTpl").innerHTML = ctTpls.map(t => `<option value="${esc(t.name)}">${esc(t.label)}</option>`).join("");
  $("#ctTplHint").innerHTML = ctTpls.length ? "" : (isAdmin()
    ? `No container templates yet. Open <b>Server → Container templates</b> and add one (Debian, Ubuntu, Alpine…; 50-200 MB).`
    : "No container templates on this server yet; ask your provider.");
  $("#ctGo").disabled = !ctTpls.length;
  const lan = isAdmin() ? !!host?.networks?.includes("lan") : !!me?.lan;
  $("#ctNet").innerHTML = `<option value="nat">Private (NAT) — reaches the internet</option>` + (lan ? `<option value="lan">Home network — address from your router</option>` : "");
  $("#ctKeys").value = !isAdmin() && me?.ssh_keys?.length ? me.ssh_keys.join("\n") : "";
  $("#ctOwnerBox").hidden = !(isAdmin() && me?.accounts_on);
  if (!$("#ctOwnerBox").hidden) {
    try { userList = (await call("GET", "/users")).users; } catch {}
    $("#ctOwner").innerHTML = `<option value="">Admins only</option>` + (userList || []).filter(u => u.role !== "admin").map(u => `<option>${esc(u.name)}</option>`).join("");
  }
  $("#dlgCT").showModal(); $("#ctName").focus();
}

$("#fCT").addEventListener("submit", async e => {
  if (e.submitter?.value !== "ok") return;
  e.preventDefault();
  const body = {name: $("#ctName").value.trim(), template: $("#ctTpl").value, vcpus: +$("#ctCPU").value, memory_mib: +$("#ctRAM").value,
    disk_gb: +$("#ctDisk").value, network: $("#ctNet").value, root_password: $("#ctPw").value,
    ssh_keys: $("#ctKeys").value.split("\n").map(s => s.trim()).filter(Boolean), start: $("#ctStart").checked};
  if (!$("#ctOwnerBox").hidden && $("#ctOwner").value) body.owner = $("#ctOwner").value;
  if (!body.root_password && !body.ssh_keys.length && !confirm("No password and no SSH key: you can still use the browser console. Create anyway?")) return;
  $("#ctGo").disabled = true;
  try {
    const r = await call("POST", "/containers", body);
    $("#dlgCT").close();
    toast(r.warning ? r.warning : `${body.name} created${body.start ? " and starting" : ""}`, !!r.warning);
    await loadCTs(); renderCTs();
  } catch (err) { toast(err.message, true); }
  finally { $("#ctGo").disabled = false; }
});

/* ---------- manage ---------- */
async function openCTManage(name){
  ctCur = name;
  if (!ctTpls.length) { try { ctTpls = (await call("GET", "/ct-templates")).templates; } catch {} }
  renderCTManage();
  $("#dlgCTM").showModal();
  loadCTSnaps();
  $("#cmOwnerBox").hidden = !(isAdmin() && me?.accounts_on);
  if (!$("#cmOwnerBox").hidden) {
    try { userList = (await call("GET", "/users")).users; } catch {}
    const c = cts.find(x => x.name === name);
    $("#cmOwner").innerHTML = `<option value="">Admins only</option>` + (userList || []).filter(u => u.role !== "admin").map(u => `<option ${u.name === c?.owner ? "selected" : ""}>${esc(u.name)}</option>`).join("");
  }
}

function renderCTManage(){
  const c = cts.find(x => x.name === ctCur);
  if (!c) return;
  $("#cmTitle").textContent = c.name;
  $("#cmSub").innerHTML = `Container · ${esc(ctTplLabel(c.template))} · <span class="state ${esc(c.state)}"><span class="dot"></span>${esc(c.state)}</span>${c.ip ? ` · <span class="mono">${esc(c.ip)}</span>` : ""}`;
  $("#cmPower").innerHTML = c.state === "running"
    ? `<button class="btn" data-cm="console">Console</button><button class="btn" data-cm="stop">Shutdown</button><button class="btn" data-cm="reboot">Reboot</button><button class="btn" data-cm="kill">Force off</button>`
    : `<button class="btn primary" data-cm="start">Start</button>`;
  for (const b of $("#cmPower").querySelectorAll("[data-cm]")) b.onclick = () => b.dataset.cm === "console" ? ctConsole(c.name) : ctPower(c.name, b.dataset.cm);
  $("#cmCPU").value = c.vcpus; $("#cmRAM").value = c.memory_mib; $("#cmDisk").value = c.disk_gb; $("#cmAuto").checked = c.autostart;
}

async function loadCTSnaps(){
  try {
    const l = (await call("GET", `/containers/${encodeURIComponent(ctCur)}/snapshots`)).snapshots;
    $("#cmSnaps").innerHTML = l.length ? l.slice().reverse().map(s => `<tr><td><b>${esc(s.name)}</b><div class="sub">${esc(new Date(s.created).toLocaleString())}</div></td>
      <td style="text-align:right"><button class="btn" data-sr="${esc(s.name)}">Roll back</button> <button class="btn danger" data-sd="${esc(s.name)}">Delete</button></td></tr>`).join("")
      : `<tr><td class="sub">No snapshots yet. A snapshot is instant and lets you undo changes later.</td></tr>`;
  } catch (err) { $("#cmSnaps").innerHTML = `<tr><td class="sub">${esc(err.message)}</td></tr>`; }
}

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
$("#cmDel").onclick = () => ctDelete(ctCur);
$("#cmClose").onclick = () => $("#dlgCTM").close();

