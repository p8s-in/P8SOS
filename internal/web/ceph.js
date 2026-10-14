// Server page → Ceph: storage made of the servers' own disks, shared by all of
// them (VMs on it move without copying disks and restart elsewhere when their
// server dies). Uses the dashboard helpers ($, esc, call, toast, armed) and ndGB.
"use strict";

let cephData = null, cephTimer = null;

async function loadCeph(){
  const box = $("#ndCeph");
  if (!box) return;
  try { cephData = await call("GET", "/ceph"); }
  catch (e) { box.innerHTML = `<div class="sub">${esc(e.message)}</div>`; return; }
  renderCeph();
  clearTimeout(cephTimer);
  const busy = (cephData.ceph.nodes || []).some(n => n.info?.task?.running);
  if ($("#dlgNode").open && busy) cephTimer = setTimeout(loadCeph, 4000);
}

// "192.168.1.11/24" -> "192.168.1.0/24"
function cephNet(cidr){
  const [ip, bits] = cidr.split("/"), b = +bits;
  const n = ip.split(".").reduce((a, x) => (a << 8 >>> 0) + (+x), 0) >>> 0;
  const m = b ? (0xffffffff << (32 - b)) >>> 0 : 0, v = (n & m) >>> 0;
  return [v >>> 24, v >>> 16 & 255, v >>> 8 & 255, v & 255].join(".") + "/" + b;
}

function cephTask(t){
  if (!t?.kind) return "";
  const what = {install: "installing Ceph", init: "setting Ceph up", mon: "adding a monitor", mgr: "adding a manager",
    osd: "adding disk " + (t.detail || ""), "osd-remove": "taking out " + (t.detail || "")}[t.kind] || t.kind;
  if (t.running) return `<div class="sub">⏳ ${esc(what)} …</div>`;
  if (t.error) return `<div class="sub" style="color:var(--bad)">✘ ${esc(what)}: ${esc(t.error)}</div>`;
  if (t.kind === "osd-remove" && t.detail && t.detail.includes("taken out")) return `<div class="sub">${esc(t.detail)}</div>`;
  return `<div class="sub" style="color:var(--ok)">✔ ${esc(what)} done</div>`;
}

function cephNode(c, n){
  const i = n.info, id = esc(n.id);
  if (!n.online) return `<tr><td><b>${esc(n.name)}</b> <span class="sub" style="color:var(--bad)">offline</span></td><td></td></tr>`;
  if (!i) return `<tr><td><b>${esc(n.name)}</b><div class="sub">${esc(n.error || "no answer")} (update it?)</div></td><td></td></tr>`;
  const busy = i.task?.running ? "disabled" : "";
  let parts = [], btns = [];
  if (!i.installed) {
    btns.push(`<button class="btn" data-cp="install" data-node="${id}" ${busy}>Install Ceph</button>`);
    parts.push(`<span class="sub">Ceph not installed (about 300 MB download)</span>`);
  } else if (c.configured) {
    parts.push(i.mon ? "✔ monitor" : "no monitor", i.mgr ? "✔ manager" : "no manager");
    if (!i.mon) btns.push(`<button class="btn" data-cp="mon" data-node="${id}" ${busy}>Add monitor</button>`);
    if (!i.mgr) btns.push(`<button class="btn" data-cp="mgr" data-node="${id}" ${busy}>Add manager</button>`);
  } else parts.push(`<span class="sub">installed</span>`);
  const osds = (i.osds || []).map(o => {
    const st = (c.status?.osds || []).find(x => x.id === o);
    const state = st ? (st.up ? "up" : "<span style=\"color:var(--bad)\">down</span>") + (st.in ? "" : ", out") : "";
    return `osd.${o} ${state} <button class="btn" data-cp="osd-rm" data-osd="${o}" ${busy} title="Take this disk out of Ceph">Remove</button>`;
  });
  const disks = c.configured && i.installed ? (i.disks || []).map(d =>
    `<div class="sub mono">${esc(d.label)} <button class="btn" data-cp="osd" data-node="${id}" data-disk="${esc(d.path)}" ${busy}>Give to Ceph</button></div>`) : [];
  return `<tr><td><b>${esc(n.name)}</b>${n.self ? ' <span class="sub">(this server)</span>' : ""} <span class="sub">${esc((i.addrs || []).join(", "))}</span>
      <div class="sub">${parts.join(" · ")}${osds.length ? " · disks: " + osds.join(" · ") : ""}</div>
      ${disks.join("")}${cephTask(i.task)}</td>
    <td style="text-align:right;white-space:nowrap;vertical-align:top">${btns.join(" ")}</td></tr>`;
}

function renderCeph(){
  const c = cephData.ceph, s = c.status, adv = cephData.advice || [];
  const self = (c.nodes || []).find(n => n.self);
  let head = "";
  if (s) {
    const col = s.health === "HEALTH_OK" ? "var(--ok)" : s.health === "HEALTH_WARN" ? "var(--warn)" : "var(--bad)";
    const pct = s.size_bytes ? Math.round(s.used_bytes * 100 / s.size_bytes) : 0;
    head = `<div><b style="color:${col}">● ${esc(s.health.replace("HEALTH_", ""))}</b>
      · monitors ${s.quorum.length}/${s.mons} working · manager ${s.mgr_up ? "up" : "<b style=\"color:var(--bad)\">down</b>"}
      · disks ${s.osd_up}/${s.osd_count} up · ${ndGB(s.used_bytes)} used of ${ndGB(s.size_bytes)} (${pct}%)
      · network ${esc(c.network || "")}</div>
      ${(s.problems || []).map(p => `<div class="sub">⚠ ${esc(p)}</div>`).join("")}`;
  } else if (c.status_error) head = `<div class="sub" style="color:var(--bad)">✘ ${esc(c.status_error)}</div>`;
  let init = "";
  if (!c.configured && self?.info?.installed) {
    const guess = (self.info.addrs || [])[0];
    init = `<div class="row" style="margin-top:8px"><div><label for="cephNet">Network the servers share (Ceph traffic, encrypted)</label>
      <input id="cephNet" value="${guess ? esc(cephNet(guess)) : ""}" placeholder="192.168.1.0/24" autocomplete="off"></div>
      <div><label>&nbsp;</label><button class="btn primary" id="cephInit" ${self.info.task?.running ? "disabled" : ""}>Set up Ceph</button></div></div>
      <div class="sub">This server becomes the first monitor and manager. Use a network every server is on (a separate 10 Gbit network is best).</div>`;
  }
  let pool = "";
  if (c.configured && !c.pool_ready) {
    pool = `<div class="inline" style="margin-top:8px"><label for="cephSize" style="margin:0">Copies of every block</label>
      <select id="cephSize"><option value="0">automatic (3; fewer with fewer servers)</option><option value="3">3</option><option value="2">2</option><option value="1">1 (no safety)</option></select>
      <button class="btn primary" id="cephPool">Create the VM pool</button></div>`;
  } else if (c.pool_ready) {
    pool = `<div class="sub">VM pool <span class="mono">${esc(c.pool)}</span> is ready: choose storage <b>ceph</b> when making a VM.
      VMs on Ceph move between servers without copying disks and restart on another server when theirs dies.</div>`;
  }
  $("#ndCeph").innerHTML = `
    <div class="sub">Storage made of the servers' own disks, shared by all of them — like Proxmox's Ceph. Needs at least 3 servers with
      an empty disk each for real safety; works with 1 for trying it out.</div>
    ${head}${adv.length ? `<ul class="sub" style="margin:6px 0">${adv.map(a => `<li>${esc(a)}</li>`).join("")}</ul>` : ""}
    <table class="snaps"><tbody>${(c.nodes || []).map(n => cephNode(c, n)).join("")}</tbody></table>
    ${init}${pool}`;

  if ($("#cephInit")) $("#cephInit").onclick = () => cephDo("POST", "/ceph/init", {network: $("#cephNet").value.trim()}, "Setting Ceph up");
  if ($("#cephPool")) $("#cephPool").onclick = async () => {
    try {
      const r = await call("POST", "/ceph/pool", {size: +$("#cephSize").value});
      toast(`VM pool made: ${r.size} cop${r.size === 1 ? "y" : "ies"} of every block`);
    } catch (e) { toast(e.message, true); }
    loadCeph();
  };
  for (const b of $("#ndCeph").querySelectorAll("button[data-cp]")) b.onclick = async () => {
    const node = b.dataset.node || "";
    switch (b.dataset.cp) {
      case "install": return cephDo("POST", "/ceph/install", {node}, "Installing Ceph");
      case "mon": return cephDo("POST", "/ceph/mons", {node}, "Adding a monitor");
      case "mgr": return cephDo("POST", "/ceph/mgrs", {node}, "Adding a manager");
      case "osd":
        if (!armed(b, "Erase it and give it to Ceph?")) return;
        return cephDo("POST", "/ceph/osds", {node, disk: b.dataset.disk}, "Giving the disk to Ceph");
      case "osd-rm":
        if (!armed(b, "Take it out?")) return;
        return cephDo("DELETE", "/ceph/osds/" + b.dataset.osd, null, "Taking the disk out (Ceph first moves its data)");
    }
  };
}

async function cephDo(method, path, body, msg){
  try { await call(method, path, body); toast(msg + " …"); }
  catch (e) { toast(e.message, true); }
  loadCeph();
}
