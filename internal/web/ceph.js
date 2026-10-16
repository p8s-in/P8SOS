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
    osd: "adding disk " + (t.detail || ""), "osd-remove": "taking out " + (t.detail || ""),
    "mon-alone": "making this monitor the only one", remove: "removing Ceph from this server"}[t.kind] || t.kind;
  if (t.running) return `<div class="sub">⏳ ${esc(what)} …</div>`;
  if (t.error) return `<div class="sub" style="color:var(--bad)">✘ ${esc(what)}: ${esc(t.error)}</div>`;
  if (t.kind === "osd-remove" && t.detail && t.detail.includes("taken out")) return `<div class="sub">${esc(t.detail)}</div>`;
  if ((t.kind === "mon-alone" || t.kind === "remove") && t.detail) return `<div class="sub" style="color:var(--ok)">✔ ${esc(what)} done — ${esc(t.detail)}</div>`;
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
  // no empty disk here: say why and how (disks with data are never offered)
  if (c.configured && i.installed && !disks.length && !osds.length && !c.status_error)
    disks.push(`<div class="sub">No empty disk on this server. Ceph only takes a disk with nothing on it (disks with data or partitions are never offered).
      Add one — on a VirtualBox VM: shut it down, Settings → Storage → add a new 20 GB disk, start it — then reload this page.</div>`);
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
  const upDisks = (s?.osds || []).filter(o => o.up && o.in).length;
  if (c.status_error) {
    // nothing to add while Ceph doesn't answer: the box above says what to do
  } else if (c.configured && !c.pool_ready && !upDisks) {
    pool = `<div class="hint" style="margin-top:8px">Next: press <b>Give to Ceph</b> on an empty disk above (one per server is best).
      When a disk shows <b>up</b>, you can create the VM pool here.</div>`;
  } else if (c.configured && !c.pool_ready) {
    pool = `<div class="inline" style="margin-top:8px"><label for="cephSize" style="margin:0">Copies of every block</label>
      <select id="cephSize"><option value="0">automatic (3; fewer with fewer servers)</option><option value="3">3</option><option value="2">2</option><option value="1">1 (no safety)</option></select>
      <button class="btn primary" id="cephPool">Create the VM pool</button></div>`;
  } else if (c.pool_ready) {
    pool = `<div class="sub">VM pool <span class="mono">${esc(c.pool)}</span> is ready: choose storage <b>ceph</b> when making a VM.
      VMs on Ceph move between servers without copying disks and restart on another server when theirs dies.</div>`;
  }
  // Ceph doesn't answer (a monitor's server is gone), or the person wants it off this server
  let repair = "";
  const busySelf = self?.info?.task?.running ? "disabled" : "";
  if (c.configured && c.status_error) {
    repair = `<div class="hint" style="margin-top:8px"><b>Ceph doesn't answer.</b> Usually a server with a Ceph monitor is off or can't be reached
      (Ceph needs more than half of its monitors). When that server comes back, Ceph works again by itself. If it is gone for good:
      <div class="inline" style="margin-top:6px;flex-wrap:wrap">
        ${self?.info?.mon ? `<button class="btn primary" id="cephAlone" ${busySelf} title="The other monitors are taken out of Ceph's monitor list">Keep Ceph with this server's monitor only</button>` : ""}
        <span class="spacer"></span>
        <label style="display:flex;gap:6px;align-items:center;font-weight:400;margin:0"><input type="checkbox" id="cephWipe" style="width:auto"> also wipe the disks this server gave to Ceph</label>
        <button class="btn danger" id="cephRemove" ${busySelf}>Remove Ceph from this server</button></div>
      <div class="sub">Keep: VMs on Ceph work again; blocks whose only copies were on the gone server are lost.
        Remove: when you don't use Ceph (no VM may have disks on it); its files are put aside, not deleted, and you can set Ceph up again here.</div></div>`;
  } else if (c.configured) {
    repair = `<details style="margin-top:6px"><summary class="sub">Remove Ceph from this server</summary>
      <div class="inline" style="flex-wrap:wrap"><label style="display:flex;gap:6px;align-items:center;font-weight:400;margin:0"><input type="checkbox" id="cephWipe" style="width:auto"> also wipe the disks this server gave to Ceph</label>
      <span class="spacer"></span><button class="btn danger" id="cephRemove" ${busySelf}>Remove Ceph from this server</button></div>
      <div class="sub">Only when no VM has disks on Ceph. With other servers in Ceph, take their disks out first (Remove on each disk).</div></details>`;
  }
  $("#ndCeph").innerHTML = `
    <div class="sub">Storage made of the servers' own disks, shared by all of them — like Proxmox's Ceph. Needs at least 3 servers with
      an empty disk each for real safety; works with 1 for trying it out.</div>
    ${head}${c.status_error ? repair : ""}${adv.length && !c.status_error ? `<ul class="sub" style="margin:6px 0">${adv.map(a => `<li>${esc(a)}</li>`).join("")}</ul>` : ""}
    <table class="snaps"><tbody>${(c.nodes || []).map(n => cephNode(c, n)).join("")}</tbody></table>
    ${init}${pool}${c.status_error ? "" : repair}`;
  if ($("#cephAlone")) $("#cephAlone").onclick = e => {
    if (!armed(e.target, "Sure the other server is gone for good?")) return;
    cephDo("POST", "/ceph/mon-alone", {}, "Making this server's monitor the only one");
  };
  if ($("#cephRemove")) $("#cephRemove").onclick = e => {
    const wipe = $("#cephWipe")?.checked;
    if (!armed(e.target, wipe ? "Remove Ceph AND wipe its disks?" : "Remove Ceph from this server?")) return;
    cephDo("POST", "/ceph/remove", {wipe}, "Removing Ceph from this server");
  };

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
