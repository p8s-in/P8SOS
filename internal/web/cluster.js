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
  <div id="clInvites"></div>
  <div id="clAddBox" hidden style="border:1px solid var(--line);border-radius:8px;padding:10px;margin-top:8px">
    <nav class="tabs" style="margin-top:0"><button type="button" data-cltab="lan" class="on">On this network</button><button type="button" data-cltab="wan">Another network</button></nav>
    <div data-clp="lan">
      <div class="inline"><span class="sub" style="flex:1">P8S OS servers on the same Wi-Fi / LAN as this one. Adding sends an invitation; someone accepts it on that server.</span>
        <button class="btn" id="clFind" type="button">Search again</button></div>
      <table class="snaps"><tbody id="clFound"></tbody></table>
      <div id="clWait" class="hint" hidden></div>
      <details style="margin-top:8px"><summary class="sub" style="cursor:pointer">Not listed? Add it with a command instead</summary>
        <div class="inline"><span class="sub" style="flex:1">Makes a one-time command (valid for an hour) to run on the new server.</span><button class="btn" id="clMakeLan" type="button">Make command</button></div>
      </details>
    </div>
    <div data-clp="wan" hidden>
      <div class="sub">For a server in another house, office or a VPS. It reaches this server over the internet, so your router must let it in.</div>
      <label for="clPublic">This server's public address</label>
      <div class="inline" style="margin-top:0"><input id="clPublic" placeholder="public IP, or a name like myhome.duckdns.org" style="flex:1"><button class="btn" id="clCheck" type="button">Check</button></div>
      <div class="hint" id="clCheckOut"></div>
      <ol class="steps sub">
        <li>On your router, forward <b>TCP ${location.port || 443}</b> (joining) and <b>UDP 51820</b> (the tunnel) to this server (${esc(location.hostname)}).</li>
        <li>If your public IP changes, use a free dynamic-DNS name (e.g. duckdns.org) instead of the number.</li>
        <li>Behind Jio/Airtel CGNAT nothing can reach this server from outside; then make the server with a public IP (e.g. a VPS) the master.</li>
      </ol>
      <div class="inline"><span class="spacer"></span><button class="btn primary" id="clMakeWan" type="button">Make join command</button></div>
    </div>
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
  if (role) {
    const seen = cluster.peer?.master_seen_s ?? -1;
    $("#clSub").innerHTML += `<div class="hint">${cluster.size >= 3
      ? `No single master: if the master is gone for about a minute, the other servers choose a new one (they need a majority: ${Math.floor(cluster.size / 2) + 1} of ${cluster.size}).`
      : "With 3 or more servers the cluster also survives losing its master. With 2, a worker can take over by hand."}${cluster.term ? ` Term ${cluster.term}.` : ""}</div>` +
      (role === "master" && cluster.quorum === false ? `<div class="hint" style="color:var(--bad)">This master can't reach most of the servers: high availability is paused here.</div>` : "") +
      (role === "worker" && seen >= 45 ? `<div class="hint" style="color:var(--warn)">The master hasn't answered for ${seen < 100000 ? seen + " s" : "a long time"}.
        ${cluster.size >= 3 ? "The servers will choose a new master if most of them agree." : ""}
        <button class="btn" id="clPromote" type="button">Make this server the master</button></div>` : "");
  }
  $("#clNodes").innerHTML = nodes.map(n => {
    const c = n.status?.capacity || {}, on = n.status?.online;
    const room = c.mem_total_mib ? `${clGB(c.mem_used_mib || 0)} of ${clGB(c.mem_total_mib)} GB RAM given to VMs · ${c.vms || 0} VMs · ${c.cpus || "?"} CPUs` : "";
    return `<tr><td><b>${esc(n.name)}</b> <span class="sub">${esc(n.id)} · ${esc(n.role)}${n.self ? " · this server" : ""} · ${esc(n.wg_ip)}</span>
      <div class="sub">${room}</div>${!n.self && cluster.role === "master" ? `<div class="sub">${n.status?.tunnel ? "tunnel up · last handshake " + esc(new Date(n.status.tunnel).toLocaleTimeString()) : `<span style="color:var(--warn)">tunnel not up yet</span> — on another network, check UDP 51820 forwarding to this server`}</div>` : ""}
      ${!on && n.status?.error ? `<div class="sub" style="color:var(--bad)">${esc(n.status.error)}</div>` : ""}</td>
      <td style="white-space:nowrap"><span class="state ${on ? "running" : "stopped"}"><span class="dot"></span>${on ? "online" : "offline"}</span></td>
      <td style="text-align:right;white-space:nowrap">${role === "master" && !n.self ? `<button class="btn" data-clsh="${esc(n.id)}" data-name="${esc(n.name)}" type="button" title="Root shell of ${esc(n.name)}">&gt;_ Shell</button> <button class="btn" data-clren="${esc(n.id)}" data-name="${esc(n.name)}" type="button" title="Give ${esc(n.name)} a new name">Rename</button> <button class="btn danger" data-clrm="${esc(n.id)}" data-name="${esc(n.name)}" type="button">Remove</button>` : ""}</td></tr>`;
  }).join("");
  $("#clActions").innerHTML = role === "worker"
    ? `<span class="spacer"></span><button class="btn danger" id="clLeave" type="button">Leave the cluster</button>`
    : `<span class="spacer"></span>${role ? "" : `<button class="btn" id="clJoinOpen" type="button">Join a master…</button>`}<button class="btn primary" id="clAddOpen" type="button">+ Add a server</button>`;
  if ($("#clAddOpen")) $("#clAddOpen").onclick = () => { $("#clAddBox").hidden = false; $("#clJoinBox").hidden = true; clTab("lan"); };
  if ($("#clJoinOpen")) $("#clJoinOpen").onclick = () => { $("#clJoinBox").hidden = false; $("#clAddBox").hidden = true; };
  if ($("#clPromote")) $("#clPromote").onclick = async () => {
    if (!confirm("Make this server the cluster master? Only do this when the old master is really gone (switched off or broken); it becomes a worker when it comes back.")) return;
    try { const r = await call("POST", "/cluster/promote", {}); toast(r.note); setTimeout(() => location.reload(), 4000); } catch (e) { toast(e.message, true); }
  };
  if ($("#clLeave")) $("#clLeave").onclick = async () => {
    if (!confirm("Leave the cluster? This server's VMs keep running here; the master won't show them any more.")) return;
    try { await call("POST", "/cluster/leave"); toast("This server stands alone again. On the master it shows as offline: press Remove there."); loadCluster(); } catch (e) { toast(e.message, true); }
  };
}
async function makeCommand(masterURL, endpoint){
  try {
    const r = await call("POST", "/cluster/join-code", {master_url: masterURL, endpoint});
    $("#clCmd").textContent = r.command; $("#clCmdBox").hidden = false;
    loadCluster();
  } catch (e) { toast(e.message, true); }
}
$("#clMakeLan").onclick = () => makeCommand(location.origin, "");
$("#clMakeWan").onclick = () => {
  const h = $("#clPublic").value.trim().replace(/^https?:\/\//, "").replace(/[:/].*$/, "");
  if (!h) return toast("Enter this server's public address first", true);
  makeCommand(`https://${h.includes(":") ? "[" + h + "]" : h}:${location.port || 443}`, h);
};
for (const b of document.querySelectorAll("[data-cltab]")) b.onclick = () => clTab(b.dataset.cltab);
function clTab(t){
  for (const b of document.querySelectorAll("[data-cltab]")) b.classList.toggle("on", b.dataset.cltab === t);
  for (const p of document.querySelectorAll("[data-clp]")) p.hidden = p.dataset.clp !== t;
  $("#clCmdBox").hidden = true;
  if (t === "lan") findServers();
  if (t === "wan" && !$("#clPublic").value) call("GET", "/cluster/public-ip").then(r => { if (r.ip && !$("#clPublic").value) $("#clPublic").value = r.ip; }).catch(() => {});
}
let clFound = [], clWaitTimer = null;
async function findServers(){
  $("#clFound").innerHTML = `<tr><td class="muted">Searching this network…</td></tr>`;
  try { clFound = (await call("GET", "/cluster/discover")).servers || []; }
  catch (e) { $("#clFound").innerHTML = `<tr><td class="sub">${esc(e.message)}</td></tr>`; return; }
  $("#clFound").innerHTML = clFound.length ? clFound.map((f, i) => `<tr><td><b>${esc(f.name)}</b> <span class="sub mono">${esc(f.ip)}</span>
      <div class="sub">${f.cpus ? f.cpus + " CPUs · " : ""}${f.mem_total_mib ? clGB(f.mem_total_mib) + " GB RAM · " : ""}P8S OS ${esc(f.version || "?")}${f.why ? ` · <b>${esc(f.why)}</b>` : ""}</div></td>
      <td style="text-align:right">${f.available ? `<button class="btn primary" data-clinv="${i}" type="button">Add</button>` : `<span class="sub">not available</span>`}</td></tr>`).join("")
    : `<tr><td class="sub">No other P8S OS server answered. Is it switched on, on the same network, with P8S OS updated? (It answers on UDP port 51821.)</td></tr>`;
}
$("#clFound").addEventListener("click", async e => {
  const b = e.target.closest("[data-clinv]"); if (!b) return;
  const f = clFound[+b.dataset.clinv];
  b.disabled = true;
  try {
    const r = await call("POST", "/cluster/invite-send", f);
    $("#clWait").hidden = false;
    $("#clWait").innerHTML = `Invitation sent to <b>${esc(f.name)}</b>. On ${esc(f.name)}, open <b>Server → Cluster</b> (or run <code>p8 cluster accept</code>) and accept it —
      check that it shows the number <b style="font-size:16px;letter-spacing:2px">${esc(r.confirm)}</b>. Waiting…`;
    const before = (cluster?.nodes || []).length;
    clearInterval(clWaitTimer);
    let n = 0;
    clWaitTimer = setInterval(async () => {
      if (++n > 300 || !$("#dlgNode").open) return clearInterval(clWaitTimer);
      await loadCluster();
      if ((cluster?.nodes || []).length > before) {
        clearInterval(clWaitTimer); $("#clWait").innerHTML = `✔ <b>${esc(f.name)}</b> joined.`; toast(f.name + " joined the cluster"); findServers();
      }
    }, 3000);
  } catch (err) { toast(err.message, true); b.disabled = false; }
});
$("#clCheck").onclick = async () => {
  const h = $("#clPublic").value.trim().replace(/^https?:\/\//, "").replace(/[:/].*$/, "");
  if (!h) return;
  $("#clCheckOut").textContent = "Checking…";
  try {
    const r = await call("POST", "/cluster/check", {host: h, port: +(location.port || 443)});
    $("#clCheckOut").innerHTML = r.private
      ? `<b style="color:var(--bad)">${esc(h)} is a local or CGNAT address</b> — servers on other networks can't use it. Use your public IP or a dynamic-DNS name.`
      : r.tcp ? `✔ Port ${location.port || 443} answered at ${esc(h)}. (UDP 51820 can only be tested by joining; the server list shows "tunnel up" when it works.)`
      : `Port ${location.port || 443} didn't answer at ${esc(h)} from here. Check the port forwarding — some routers can't test themselves; then try joining from the other place.`;
  } catch (e) { $("#clCheckOut").textContent = e.message; }
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
  const sh = e.target.closest("[data-clsh]");
  if (sh) return void window.open(`/terminal?host=1&node=${encodeURIComponent(sh.dataset.clsh)}&name=${encodeURIComponent(sh.dataset.name)}`, "p8s-shell-" + sh.dataset.clsh);
  const rn = e.target.closest("[data-clren]");
  if (rn) {
    const n = (prompt(`New name for ${rn.dataset.name} (lowercase letters, digits, hyphens):`, rn.dataset.name) || "").trim().toLowerCase();
    if (!n || n === rn.dataset.name) return;
    try { const r = await call("POST", `/cluster/nodes/${encodeURIComponent(rn.dataset.clren)}/rename`, {name: n}); toast(r.note); setTimeout(loadCluster, 8000); }
    catch (err) { toast(err.message, true); }
    return;
  }
  const b = e.target.closest("[data-clrm]"); if (!b) return;
  const id = b.dataset.clrm;
  if (!confirm(`Remove ${b.dataset.name || id} from the cluster? It is told to stand alone again (its VMs keep running there, but this dashboard no longer shows them). You can add it again later with + Add a server.`)) return;
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

/* ---------- invitations to this server (it may join a master) ---------- */
document.body.insertAdjacentHTML("beforeend", `<div id="clBanner" hidden style="position:fixed;top:58px;right:16px;z-index:20;max-width:420px;
  background:var(--panel);border:1px solid var(--line);border-left:4px solid var(--accent);border-radius:10px;padding:12px 14px;box-shadow:0 8px 24px rgba(0,0,0,.15)"></div>`);
let clInv = [], clInvTick = 0;
async function loadInvites(){
  if (!token || !isAdmin()) { $("#clBanner").hidden = true; return; }
  try { clInv = (await call("GET", "/cluster/invites")).invites || []; } catch { clInv = []; }
  const card = i => `<div style="margin:4px 0 10px"><b>${esc(i.master_name)}</b> <span class="sub mono">(${esc(i.from)})</span> wants to add this server to its cluster.
      <div class="sub">Its VMs will then be managed from ${esc(i.master_name)}'s dashboard. Only accept if ${esc(i.master_name)} shows the same number:</div>
      <div style="font-size:22px;font-weight:700;letter-spacing:4px;margin:6px 0">${esc(i.confirm)}</div>
      <div class="inline" style="margin:0"><button class="btn primary" data-clacc="${esc(i.id)}" type="button">Accept</button><button class="btn" data-cldec="${esc(i.id)}" type="button">Decline</button></div></div>`;
  $("#clBanner").innerHTML = clInv.map(card).join("");
  $("#clBanner").hidden = !clInv.length;
  if ($("#clInvites")) $("#clInvites").innerHTML = clInv.length ? `<div style="border:1px solid var(--line);border-radius:8px;padding:10px;margin-top:8px">${clInv.map(card).join("")}</div>` : "";
}
document.addEventListener("click", async e => {
  const a = e.target.closest("[data-clacc]"), d = e.target.closest("[data-cldec]");
  if (!a && !d) return;
  const id = (a || d).dataset.clacc || (a || d).dataset.cldec;
  (a || d).disabled = true;
  try {
    if (a) { const r = await call("POST", `/cluster/invites/${encodeURIComponent(id)}/accept`); toast(`Joined ${r.master}'s cluster as ${r.id}. Manage this server's VMs from there now.`); }
    else { await call("POST", `/cluster/invites/${encodeURIComponent(id)}/decline`); toast("Invitation declined"); }
  } catch (err) { toast(err.message, true); }
  loadInvites();
  if ($("#dlgNode").open) loadCluster();
});
const _refreshCl = refresh;
refresh = async function(...a){ await _refreshCl(...a); if (clInvTick++ % 3 === 0) loadInvites(); };
const _loadCluster = loadCluster;
loadCluster = async function(){ await _loadCluster(); loadInvites(); };
