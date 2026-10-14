"use strict";
/* ---------- PCI / USB passthrough: Manage → Hardware ---------- */
// Uses the dashboard's globals: $, esc, call, toast, armed, refresh, mName, me, vms.

document.querySelector('#dlgManage [data-tab="resources"]').insertAdjacentHTML("beforeend", `<div id="mDevBox" hidden>
  <h3>PCI and USB devices</h3>
  <div class="sub">Give the VM one of this server's devices: a graphics card (gaming, AI, video encoding), a network card,
    an NVMe disk, a USB controller, or a USB stick, dongle or printer. The VM then owns it like real hardware.</div>
  <div id="mDevIOMMU"></div>
  <table class="snaps"><tbody id="mDevs"></tbody></table>
  <div class="inline" style="flex-wrap:wrap">
    <select id="mDevPick" style="flex:1 1 320px" aria-label="Device to give to the VM"></select>
    <button class="btn" id="mDevAdd">Give to VM</button></div>
  <div class="hint" id="mDevHint"></div>
</div>`);

let devInfo = null, devFor = "", devKey = "";

async function loadDevices(force){
  const v = vms.find(x => x.name === mName);
  if (!v || !(me?.role === "admin" || me?.role === "operator")) { $("#mDevBox").hidden = true; return; }
  if (!force && devFor === mName && devInfo) return renderDevs(v);
  devFor = mName;
  try { devInfo = await call("GET", `/vms/${encodeURIComponent(mName)}/devices`); }
  catch (e) { devInfo = null; $("#mDevBox").hidden = true; return; }
  devKey = "";
  renderDevs(v);
}

function devLabel(p){ return `${p.name} · ${p.addr}`; }

function renderDevs(v){
  const box = $("#mDevBox"), d = devInfo;
  if (!d) { box.hidden = true; return; }
  box.hidden = false;
  const admin = me?.role === "admin", stopped = v.state === "stopped";
  const key = [v.name, v.state, JSON.stringify(v.pci || []), JSON.stringify(v.usb || []), d.available.iommu].join("|");
  if (key === devKey) return;
  devKey = key;
  const a = d.available, pciBy = Object.fromEntries((a.pci || []).map(p => [p.addr, p])), usbBy = Object.fromEntries((a.usb || []).map(u => [u.id, u]));
  $("#mDevIOMMU").innerHTML = a.iommu ? "" : `<div class="hint" style="color:var(--warn)">The CPU's IOMMU is off, so PCI devices can't be given (USB works).
    ${admin ? `<button class="btn" id="mDevIOMMUOn" type="button">Turn on IOMMU</button> then reboot the server.` : "An admin can turn it on."}
    If it stays off after the reboot, turn on <b>VT-d</b> (Intel) or <b>AMD-Vi / IOMMU</b> (AMD) in the BIOS.
    ${v.node_id && v.node_id !== "n1" ? ` For this VM's server, run <code>sudo p8 iommu on</code> there.` : ""}</div>`;
  const rows = [
    ...(v.pci || []).map(x => {
      const p = pciBy[x];
      return `<tr data-kind="pci" data-id="${esc(x)}"><td><b>${esc(p ? p.name : "PCI device")}</b> <span class="sub mono">${esc(x)}</span>
        <div class="sub">${p ? esc(p.kind) : "not on this server any more"}${p?.driver ? " · driver " + esc(p.driver) : ""}</div></td>
        <td style="text-align:right">${admin ? `<button class="btn danger" data-dev="rm" ${stopped ? "" : 'disabled title="Shut the VM down first"'}>Take away</button>` : ""}</td></tr>`;
    }),
    ...(v.usb || []).map(x => {
      const u = usbBy[x];
      return `<tr data-kind="usb" data-id="${esc(x)}"><td><b>${esc(u ? u.name : "USB device")}</b> <span class="sub mono">${esc(x)}</span>
        <div class="sub">USB · ${u ? "plugged in (" + esc(u.port) + ")" : "not plugged in now"}</div></td>
        <td style="text-align:right">${admin ? `<button class="btn danger" data-dev="rm">Take away</button>` : ""}</td></tr>`;
    })];
  $("#mDevs").innerHTML = rows.length ? rows.join("") : `<tr><td class="sub">No devices given to this VM.</td></tr>`;
  const mine = new Set([...(v.pci || []), ...(v.usb || [])]);
  const groupsOf = k => ({graphics: "Graphics cards", audio: "Audio", network: "Network cards", storage: "Disk controllers / NVMe", usb: "USB controllers", other: "Other PCI devices"})[k] || k;
  const pciOpts = {};
  for (const p of a.pci || []) {
    if (p.bridge || mine.has(p.addr)) continue;
    const used = d.used_by[p.addr];
    (pciOpts[p.kind] = pciOpts[p.kind] || []).push(`<option value="pci|${esc(p.addr)}" ${used || !a.iommu ? "disabled" : ""}>${esc(devLabel(p))}${used ? " — in VM " + esc(used) : ""}${p.warn ? " ⚠" : ""}</option>`);
  }
  const usbOpts = (a.usb || []).filter(u => !mine.has(u.id)).map(u => {
    const used = d.used_by[u.id];
    return `<option value="usb|${esc(u.id)}" ${used ? "disabled" : ""}>${esc(u.name)} · ${esc(u.id)}${used ? " — in VM " + esc(used) : ""}</option>`;
  });
  $("#mDevPick").innerHTML = `<option value="">Choose a device…</option>` +
    Object.entries(pciOpts).map(([k, o]) => `<optgroup label="${esc(groupsOf(k))} (PCI${stopped ? "" : " — shut the VM down first"})">${o.join("")}</optgroup>`).join("") +
    (usbOpts.length ? `<optgroup label="USB devices (plugged in now)">${usbOpts.join("")}</optgroup>` : "");
  $("#mDevPick").disabled = $("#mDevAdd").disabled = !admin;
  showDevHint();
  if ($("#mDevIOMMUOn")) $("#mDevIOMMUOn").onclick = () => typeof runTask === "function"
    ? runTask("iommu-on", "", "Turn on the IOMMU? The server needs a reboot afterwards (Server page → Reboot).")
    : toast("Run: sudo p8 iommu on", true);
}

function showDevHint(){
  const val = $("#mDevPick").value, [kind, id] = val.split("|");
  let h = "PCI devices: the VM must be shut down; the device leaves the server while the VM runs. USB: plugged in at once, even while it runs.";
  if (kind === "pci" && devInfo) {
    const p = (devInfo.available.pci || []).find(x => x.addr === id);
    if (p?.others?.length) h = `Its IOMMU group goes with it: ${p.others.map(esc).join(", ")}.`;
    if (p?.warn) h += ` <b style="color:var(--warn)">⚠ ${esc(p.warn)}.</b>`;
  }
  $("#mDevHint").innerHTML = h;
}
$("#mDevPick").onchange = showDevHint;

$("#mDevAdd").onclick = async () => {
  const val = $("#mDevPick").value;
  if (!val) return toast("Choose a device first", true);
  const [kind, id] = val.split("|"), b = $("#mDevAdd");
  const p = kind === "pci" ? (devInfo?.available.pci || []).find(x => x.addr === id) : null;
  if (p?.warn && !armed(b, "It may hurt the server — sure?")) return;
  b.disabled = true;
  try {
    const r = await call("POST", `/vms/${encodeURIComponent(mName)}/devices`, {kind, id});
    toast(r.note || "given to the VM");
  } catch (e) { toast(e.message, true); }
  b.disabled = false;
  await refresh(); loadDevices(true);
};

$("#mDevs").addEventListener("click", async e => {
  const b = e.target.closest("[data-dev]"), row = b?.closest("tr");
  if (!b || !row) return;
  if (!armed(b, "Take it away?")) return;
  b.disabled = true;
  try { await call("DELETE", `/vms/${encodeURIComponent(mName)}/devices/${row.dataset.kind}/${encodeURIComponent(row.dataset.id)}`); toast("device taken away"); }
  catch (err) { toast(err.message, true); b.disabled = false; }
  await refresh(); loadDevices(true);
});

/* ---------- disk / network speed limits: Manage → Hardware ---------- */
document.querySelector('#dlgManage [data-tab="resources"]').insertAdjacentHTML("beforeend", `<div id="mLimBox">
  <h3>Speed limits</h3>
  <div class="sub" id="mLimSub">So one busy VM can't slow the others down. Each disk and each network card gets the limit; empty = no limit.</div>
  <div class="row" style="grid-template-columns:repeat(auto-fit,minmax(150px,1fr))">
    <div><label for="mLimR">Disk read (MB/s)</label><input id="mLimR" type="number" min="5" placeholder="no limit"></div>
    <div><label for="mLimW">Disk write (MB/s)</label><input id="mLimW" type="number" min="5" placeholder="no limit"></div>
    <div><label for="mLimI">Disk IOPS</label><input id="mLimI" type="number" min="100" step="100" placeholder="no limit"></div>
    <div><label for="mLimD">Network in (Mbit/s)</label><input id="mLimD" type="number" min="1" placeholder="no limit"></div>
    <div><label for="mLimU">Network out (Mbit/s)</label><input id="mLimU" type="number" min="1" placeholder="no limit"></div>
  </div>
  <div class="inline"><span class="hint" style="flex:1">Changes apply at once, also while the VM runs.</span><button class="btn" id="mLimGo">Save limits</button></div>
</div>`);
const LIM = [["#mLimR", "read_mbs"], ["#mLimW", "write_mbs"], ["#mLimI", "iops"], ["#mLimD", "net_down_mbit"], ["#mLimU", "net_up_mbit"]];
let limKey = "";
function renderLimits(v){
  const staff = me?.role === "admin" || me?.role === "operator", l = v.io_limits || {};
  const key = v.name + JSON.stringify(l) + staff;
  if (key === limKey) return;
  limKey = key;
  for (const [id, k] of LIM) { $(id).value = l[k] || ""; $(id).disabled = !staff; }
  $("#mLimGo").hidden = !staff;
}
$("#mLimGo").onclick = async () => {
  const body = {};
  for (const [id, k] of LIM) body[k] = +$(id).value || 0;
  const b = $("#mLimGo"); b.disabled = true;
  try { await call("PUT", `/vms/${encodeURIComponent(mName)}/limits`, body); toast(Object.values(body).some(Boolean) ? "speed limits saved" : "speed limits removed"); limKey = ""; }
  catch (e) { toast(e.message, true); }
  b.disabled = false; refresh();
};

/* ---------- CPU / RAM hot-plug: Manage → Hardware ---------- */
// right under Resize (before "Extra disks")
[...document.querySelectorAll('#dlgManage [data-tab="resources"] > h3')].find(h => h.textContent === "Extra disks")
  .insertAdjacentHTML("beforebegin", `<div id="mHotBox">
  <h3>Hot-plug</h3>
  <div class="sub">Add CPUs and RAM while the VM runs (up to the ceilings below), with Resize above. Turning it on or off needs the VM shut down.
    Linux adds them by itself; Windows takes more CPUs, more RAM only on Server / Enterprise editions.</div>
  <div class="row" style="grid-template-columns:auto 1fr 1fr auto;align-items:end">
    <label class="check" style="margin:0 0 10px"><input id="mHotOn" type="checkbox"> CPU + RAM hot-plug</label>
    <div><label for="mHotCPU">Most CPUs</label><input id="mHotCPU" type="number" min="1" placeholder="twice now"></div>
    <div><label for="mHotMem">Most RAM (MB)</label><input id="mHotMem" type="number" min="256" step="128" placeholder="twice now"></div>
    <div><label>&nbsp;</label><button class="btn" id="mHotGo">Save</button></div>
  </div>
</div>`);
let hotKey = "";
function renderHot(v){
  const staff = me?.role === "admin" || me?.role === "operator", stopped = v.state === "stopped";
  $("#mHotBox").hidden = !staff;
  const key = [v.name, v.state, v.hot_cpus, v.hot_mem_mib].join("|");
  if (!staff || key === hotKey) return;
  hotKey = key;
  const on = !!(v.hot_cpus || v.hot_mem_mib);
  $("#mHotOn").checked = on;
  $("#mHotCPU").value = v.hot_cpus || ""; $("#mHotMem").value = v.hot_mem_mib || "";
  for (const id of ["#mHotOn", "#mHotCPU", "#mHotMem", "#mHotGo"]) $(id).disabled = !stopped;
  $("#mHotGo").title = stopped ? "" : "Shut the VM down first";
}
$("#mHotGo").onclick = async () => {
  const b = $("#mHotGo"); b.disabled = true;
  try {
    const v = await call("POST", `/vms/${encodeURIComponent(mName)}/hotplug`, {on: $("#mHotOn").checked, max_vcpus: +$("#mHotCPU").value || 0, max_mib: +$("#mHotMem").value || 0});
    toast(v.hot_cpus ? `hot-plug on: up to ${v.hot_cpus} CPUs and ${gb(v.hot_mem_mib)} RAM while it runs` : "hot-plug off"); hotKey = "";
  } catch (e) { toast(e.message, true); }
  b.disabled = false; refresh();
};

/* ---------- move the disks to another storage: Manage → Hardware ---------- */
$("#mDevBox").insertAdjacentHTML("beforebegin", `<div id="mStBox" hidden>
  <h3>Disk storage</h3>
  <div class="sub" id="mStSub"></div>
  <div class="inline"><select id="mStPick" style="flex:0 1 260px" aria-label="Storage to move the disks to"></select>
    <label class="check" style="margin:0" id="mStSnapL"><input id="mStSnap" type="checkbox"> delete its snapshots (they can't move)</label>
    <span class="spacer"></span><button class="btn" id="mStGo">Move disks</button></div>
  <div class="hint">Copies every disk of the VM (only the used part) while it is shut down, then deletes the old ones. Ceph: the VM can then move between servers in seconds and fail over.</div>
</div>`);
let stKey = "";
function renderStorage(v){
  const staff = me?.role === "admin" || me?.role === "operator";
  const list = host?.storages || ["local"];
  $("#mStBox").hidden = !staff || list.length < 2;
  const cur = v.storage || "local", key = [v.name, v.state, cur, list.join()].join("|");
  if (!staff || key === stKey) return;
  stKey = key;
  $("#mStSub").innerHTML = `Its disks are on <b>${esc(cur)}</b>.`;
  $("#mStPick").innerHTML = list.filter(s => s !== cur).map(s => `<option value="${esc(s)}">${esc(s === "local" ? "local (this server's ZFS pool)" : s)}</option>`).join("");
  const stopped = v.state === "stopped";
  $("#mStGo").disabled = !stopped; $("#mStGo").title = stopped ? "" : "Shut the VM down first";
}
$("#mStGo").onclick = async () => {
  const b = $("#mStGo"), target = $("#mStPick").value;
  if (!armed(b, `Move to ${target}?`)) return;
  b.disabled = true;
  try {
    const j = await call("POST", `/vms/${encodeURIComponent(mName)}/storage`, {storage: target, drop_snapshots: $("#mStSnap").checked});
    trackJob(j, () => { stKey = ""; b.disabled = false; });
  } catch (e) { toast(e.message, true); b.disabled = false; }
};
