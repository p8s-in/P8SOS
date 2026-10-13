"use strict";
/* Cluster (master + worker servers) in the dashboard: the Cluster section on
   the Server page, servers in the sidebar tree, and "Server" in Create VM.
   Uses index.html's $, call, esc, toast, isAdmin, vms, gb, ic, renderVMs; node.js's openNode. */

let cluster = null; // GET /cluster (admins)

/* ---------- Server page: Cluster section ---------- */
document.querySelector("#dlgNode #ndTasks").closest("table").previousElementSibling // the "Tasks" heading
  .insertAdjacentHTML("beforebegin", `<div id="clBox">
  <h3>Cluster</h3>
  <div class="sub" id="clSub"></div>
  <table class="snaps"><tbody id="clNodes"></tbody></table>
  <div class="inline" id="clActions" style="flex-wrap:wrap"></div>
  <div id="clAddBox" hidden style="border:1px solid var(--line);border-radius:8px;padding:10px;margin-top:8px">
    <label for="clEndpoint">Address the new server uses to reach this one</label>
    <input id="clEndpoint" placeholder="leave empty on the same network; public IP or name over the internet">
    <div class="hint">Over the internet, forward TCP port <b>${location.port || 443}</b> and UDP port <b>51820</b> to this server on your router.</div>
    <div class="inline"><span class="spacer"></span><button class="btn primary" id="clMake" type="button">Make join command</button></div>
    <div id="clCmdBox" hidden>
      <div class="sub">On the new server (P8S OS installed), run this once within an hour:</div>
      <pre id="clCmd" class="mono copy" title="Click to copy" style="white-space:pre-wrap;word-break:break-all;font-size:12.5px;background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:8px"></pre>
    </div>
  </div>
  <div id="clJoinBox" hidden style="border:1px solid var(--line);border-radius:8px;padding:10px;margin-top:8px">
    <label for="clURL">Master address</label><input id="clURL" placeholder="https://192.168.0.106:8006">
    <label for="clCode">Join code</label><input id="clCode" placeholder="from the master: Server → Cluster → Add a server">
    <div class="inline"><span class="spacer"></span><button class="btn primary" id="clJoinGo" type="button">Join</button></div>
  </div>
</div>`);

const clGB = mib => (mib / 1024).toFixed(mib >= 10240 ? 0 : 1);
async function loadCluster(){
  try { cluster = await call("GET", "/cluster"); } catch { cluster = null; $("#clBox").hidden = true; return; }
  $("#clBox").hidden = false;
  const role = cluster.role, nodes = cluster.nodes || [];
  $("#clSub").innerHTML = !role
    ? "This server stands alone. Add more servers (another laptop or PC with P8S OS) and the dashboard puts new VMs on whichever has room — like one bigger server."
    : role === "master"
      ? `This server is the <b>master</b>: one dashboard for all servers; new VMs go to the server with the most free memory.`
      : `This server is a <b>worker</b> of <b>${esc(nodes.find(n => n.role === "master")?.name || "the master")}</b>. Manage its VMs from the master's dashboard.`;
  $("#clNodes").innerHTML = nodes.map(n => {
    const c = n.status?.capacity || {}, on = n.status?.online;
    const room = c.mem_total_mib ? `${clGB(c.mem_used_mib || 0)} of ${clGB(c.mem_total_mib)} GB RAM given to VMs · ${c.vms || 0} VMs · ${c.cpus || "?"} CPUs` : "";
    return `<tr><td><b>${esc(n.name)}</b> <span class="sub">${esc(n.id)} · ${esc(n.role)}${n.self ? " · this server" : ""} · ${esc(n.wg_ip)}</span>
      <div class="sub">${room}</div>${!on && n.status?.error ? `<div class="sub" style="color:var(--bad)">${esc(n.status.error)}</div>` : ""}</td>
      <td style="white-space:nowrap"><span class="state ${on ? "running" : "stopped"}"><span class="dot"></span>${on ? "online" : "offline"}</span></td>
      <td style="text-align:right">${role === "master" && !n.self ? `<button class="btn danger" data-clrm="${esc(n.id)}" type="button">Remove</button>` : ""}</td></tr>`;
  }).join("");
  $("#clActions").innerHTML = role === "worker"
    ? `<span class="spacer"></span><button class="btn danger" id="clLeave" type="button">Leave the cluster</button>`
    : `<span class="spacer"></span>${role ? "" : `<button class="btn" id="clJoinOpen" type="button">Join a master…</button>`}<button class="btn primary" id="clAddOpen" type="button">+ Add a server</button>`;
  if ($("#clAddOpen")) $("#clAddOpen").onclick = () => { $("#clAddBox").hidden = false; $("#clJoinBox").hidden = true; $("#clCmdBox").hidden = true; };
  if ($("#clJoinOpen")) $("#clJoinOpen").onclick = () => { $("#clJoinBox").hidden = false; $("#clAddBox").hidden = true; };
  if ($("#clLeave")) $("#clLeave").onclick = async () => {
    if (!confirm("Leave the cluster? This server's VMs keep running here; the master won't show them any more.")) return;
    try { await call("POST", "/cluster/leave"); toast("This server stands alone again. Remove it on the master too."); loadCluster(); } catch (e) { toast(e.message, true); }
  };
}
$("#clMake").onclick = async () => {
  try {
    const r = await call("POST", "/cluster/join-code", {master_url: location.origin, endpoint: $("#clEndpoint").value.trim()});
    $("#clCmd").textContent = r.command; $("#clCmdBox").hidden = false;
    loadCluster();
  } catch (e) { toast(e.message, true); }
};
$("#clCmd").onclick = () => navigator.clipboard?.writeText($("#clCmd").textContent).then(() => toast("Command copied"));
$("#clJoinGo").onclick = async () => {
  const go = $("#clJoinGo"); go.disabled = true;
  try {
    const r = await call("POST", "/cluster/join", {url: $("#clURL").value.trim(), code: $("#clCode").value.trim()});
    toast(`Joined ${r.master}'s cluster as ${r.id}`); $("#clJoinBox").hidden = true; loadCluster();
  } catch (e) { toast(e.message, true); }
  go.disabled = false;
};
$("#clNodes").addEventListener("click", async e => {
  const b = e.target.closest("[data-clrm]"); if (!b) return;
  const id = b.dataset.clrm;
  try { await call("DELETE", `/cluster/nodes/${encodeURIComponent(id)}`); }
  catch (err) {
    if (!/still has VMs/.test(err.message) || !confirm(err.message + "\n\nRemove it anyway? Its VMs keep running there but disappear from this dashboard.")) return toast(err.message, true);
    try { await call("DELETE", `/cluster/nodes/${encodeURIComponent(id)}?force=1`); } catch (e2) { return toast(e2.message, true); }
  }
  toast(id + " removed from the cluster"); loadCluster();
});
if (typeof openNode === "function") {
  const _openNode = openNode;
  openNode = async function(){ $("#clAddBox").hidden = true; $("#clJoinBox").hidden = true; loadCluster(); return _openNode(); };
}

/* ---------- sidebar: VMs grouped by server ---------- */
const _renderVMsCl = renderVMs;
renderVMs = function(){
  _renderVMsCl();
  const side = $("#sideVMs");
  if (!side || !vms.some(v => v.node_id)) return;
  const q = ($("#vmFilter")?.value || "").trim().toLowerCase();
  const list = q ? vms.filter(v => (v.name + " " + (v.ip || "") + " " + (v.owner || "") + " " + v.template + " " + (v.node || "")).toLowerCase().includes(q)) : vms;
  const btn = v => `<button class="tree-vm ${esc(v.state)}" data-vm="${esc(v.name)}" title="VM ${esc(v.name)} on ${esc(v.node || "")} — ${esc(v.state)}"><span class="dot"></span>${ic("vm")}<span class="nm">${esc(v.name)}</span></button>`;
  const groups = new Map();
  for (const v of list) { const k = v.node_id || "n1"; if (!groups.has(k)) groups.set(k, {name: v.node, vms: []}); groups.get(k).vms.push(v); }
  const ids = [...groups.keys()].sort((a, b) => (+a.slice(1)) - (+b.slice(1)));
  // the master's VMs sit under the "server" entry above; workers get their own entry
  side.innerHTML = ids.map(id => (id === "n1" ? "" : `<div class="tree-node btn" style="cursor:default" title="cluster server ${esc(id)}">${ic("server")}<span>${esc(groups.get(id).name || id)}</span></div>`) +
    groups.get(id).vms.map(btn).join("")).join("");
  if (typeof markSide === "function") markSide();
};

/* ---------- Create VM: which server ---------- */
const nodeBoxHTML = `<div id="cNodeBox" hidden><label for="cNode">Server</label><select id="cNode"></select>
  <div class="hint" id="cNodeHint">Automatic puts it on the server with the most free memory.</div></div>`;
$("#cNet").closest("dialog").querySelector("#cNetHint").insertAdjacentHTML("afterend", nodeBoxHTML);
async function loadNodeChoice(){
  $("#cNodeBox").hidden = true;
  if (!isAdmin()) return;
  let r;
  const tpl = $("#cTpl").value || "", net = $("#cNet").value || "";
  const q = new URLSearchParams({memory_mib: +$("#cMem").value || 1024, disk_gb: +$("#cDisk").value || 0,
    network: net.startsWith("pool:") ? "" : net, iso: tpl.startsWith("iso:") ? tpl.slice(4) : "", ip_pool: net.startsWith("pool:") ? net.slice(5) : ""});
  try { r = await call("GET", "/cluster/candidates?" + q); }
  catch { return; }
  const l = r.servers || [];
  if (l.length < 2) return;
  const best = l.find(s => !s.reason);
  $("#cNode").innerHTML = `<option value="">Automatic${best ? ` (now: ${esc(best.name)})` : ""}</option>` + l.map(s =>
    `<option value="${esc(s.id)}" ${s.reason ? "disabled" : ""}>${esc(s.name)} — ${s.reason ? esc(s.reason) : clGB(Math.max(s.free_mib, 0)) + " GB RAM free"}</option>`).join("");
  $("#cNodeBox").hidden = false;
}
const _openCreateCl = openCreate;
openCreate = async function(...a){ const r = await _openCreateCl(...a); loadNodeChoice(); return r; };
for (const id of ["#cMem", "#cNet", "#cTpl"]) $(id)?.addEventListener("change", () => { if (!$("#cNodeBox").hidden) loadNodeChoice(); });
