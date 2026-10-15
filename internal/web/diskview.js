"use strict";
/* ---------- Server page → Disks (admins change, operators look) ---------- */
// Every physical disk of a server with what is on it and its SMART health;
// wipe / protect a disk; make a ZFS pool from empty disks for VM disks.
// Uses $, esc, call, toast, armed, me and ndGB (node.js).

let dkNode = "", dkData = null;
const dkSize = b => b >= 1e12 ? (b / 1e12).toFixed(1) + " TB" : (b / 1e9).toFixed(0) + " GB";
const dkQ = (extra) => { const q = new URLSearchParams(extra || {}); if (dkNode) q.set("node", dkNode); const s = q.toString(); return s ? "?" + s : ""; };

function dkHealth(s){
  if (!s) return "";
  if (!s.available) return `<span class="sub" title="${esc(s.note || "")}">SMART: n/a</span>`;
  const warn = !s.passed || s.reallocated > 0 || s.pending > 0 || s.media_errors > 0 || s.wear_pct >= 90;
  const bits = [s.temp_c ? s.temp_c + " °C" : "", s.power_on_hours ? Math.round(s.power_on_hours / 24) + " days on" : "",
    s.wear_pct ? s.wear_pct + "% worn" : "", s.reallocated ? s.reallocated + " reallocated" : "", s.pending ? s.pending + " pending" : "",
    s.media_errors ? s.media_errors + " media errors" : ""].filter(Boolean).join(" · ");
  return `<span style="color:var(${!s.passed ? "--bad" : warn ? "--warn" : "--ok"})">● ${!s.passed ? "FAILING" : warn ? "check it" : "healthy"}</span> <span class="sub">${esc(bits)}</span>`;
}

async function loadDisks(){
  const box = $("#ndDisks");
  if (!box) return;
  if (me?.role !== "admin" && me?.role !== "operator") { box.innerHTML = `<div class="sub">For admins.</div>`; return; }
  let servers = [];
  try { const c = await call("GET", "/cluster"); if (c.role === "master") servers = c.nodes || []; } catch {}
  box.innerHTML = `${servers.length > 1 ? `<div class="inline"><label for="dkNode" style="margin:0">Server</label>
      <select id="dkNode" style="flex:0 1 220px">${servers.map(n => `<option value="${n.self ? "" : esc(n.id)}" ${(n.self ? "" : n.id) === dkNode ? "selected" : ""}>${esc(n.name)}${n.self ? " (this one)" : ""}</option>`).join("")}</select></div>` : ""}
    <div id="dkBody" class="sub">Reading the disks…</div>`;
  if ($("#dkNode")) $("#dkNode").onchange = () => { dkNode = $("#dkNode").value; loadDisks(); };
  try { dkData = await call("GET", "/node/disks" + dkQ()); }
  catch (e) { $("#dkBody").innerHTML = `<span style="color:var(--bad)">${esc(e.message)}</span>`; return; }
  renderDisks();
}

function renderDisks(){
  const admin = me?.role === "admin", d = dkData, disks = d.disks || [], pools = d.pools || [];
  const free = disks.filter(x => x.usage === "empty" && !x.protected && !/USB/.test(x.kind));
  $("#dkBody").outerHTML = `<div id="dkBody">
    <table class="snaps"><tbody>${disks.map(x => `<tr data-dk="${esc(x.name)}">
      <td><b>${esc(x.name)}</b> <span class="sub">${dkSize(x.size)} · ${esc(x.kind)}${x.model ? " · " + esc(x.model) : ""}${x.serial ? " (" + esc(x.serial) + ")" : ""}</span>
        <div class="sub">${x.protected ? "🔒 protected · " : ""}<b>${esc(x.usage)}</b>${x.usage === "data" || x.usage.startsWith("old") ? " — " + esc((x.contents || []).join(", ")) : ""}${(x.mounted || []).length ? " · mounted " + esc(x.mounted.join(", ")) : ""}</div>
        <div>${dkHealth(x.smart)}</div></td>
      <td style="text-align:right;white-space:nowrap">${admin ? `<button class="btn" data-dka="protect">${x.protected ? "Unprotect" : "Protect"}</button>
        ${!x.in_use && !x.protected && x.usage !== "empty" ? `<button class="btn danger" data-dka="wipe">Wipe</button>` : ""}` : ""}</td></tr>`).join("") || `<tr><td class="sub">No disks found.</td></tr>`}</tbody></table>
    <div class="hint">Wipe and new pools never touch the system disk, disks in a ZFS pool or Ceph, or mounted disks. A disk with data is only wiped after you type its name.
      <b>Protect</b> a disk whose data you keep (it can then never be wiped or used).</div>
    <h4 style="margin:12px 0 4px">ZFS pools for VM disks</h4>
    <table class="snaps"><tbody>${pools.map(p => `<tr data-pool="${esc(p.name)}"><td><b>${esc(p.name)}</b> <span class="sub mono">${esc(p.pool)}</span>
        <div class="sub">${esc(p.layout)} of ${p.disks.length} disk(s) · ${esc(p.health || "?")}${p.size ? ` · ${dkSize(p.free)} free of ${dkSize(p.size)}` : ""} · ${p.vm_disks} VM disk(s)</div></td>
      <td style="text-align:right">${admin ? `<button class="btn danger" data-pooldel>Destroy</button>` : ""}</td></tr>`).join("") || `<tr><td class="sub">None made here yet (the server's own pool is "local").</td></tr>`}</tbody></table>
    ${admin ? (free.length ? `<div class="inline" style="flex-wrap:wrap;gap:6px;margin-top:6px">
        <input id="dkPName" placeholder="storage name, e.g. fast" style="flex:1 1 150px" aria-label="Storage name">
        <select id="dkPLayout" style="flex:0 1 210px" aria-label="Layout">
          <option value="single">single disk</option><option value="mirror">mirror (2+ disks, survives one failing)</option>
          <option value="raidz1">RAID-Z1 (3+ disks, survives one)</option><option value="raidz2">RAID-Z2 (4+ disks, survives two)</option></select>
        <span class="sub">disks:</span>${free.map(x => `<label class="inline" style="margin:0;gap:4px"><input type="checkbox" data-dkpick="${esc(x.name)}"> ${esc(x.name)} <span class="sub">${dkSize(x.size)}</span></label>`).join("")}
        <button class="btn primary" id="dkPMake" type="button">Make pool</button></div>`
      : `<div class="sub">To make a pool, add an empty disk (or wipe one).</div>`) : ""}
  </div>`;
  const body = $("#dkBody");
  body.addEventListener("click", dkClick);
  if ($("#dkPMake")) $("#dkPMake").onclick = dkMake;
}

async function dkClick(e){
  const b = e.target.closest("button"); if (!b) return;
  const row = b.closest("tr");
  try {
    if (b.dataset.dka === "protect") {
      const on = b.textContent.trim() === "Protect";
      await call("POST", `/node/disks/${encodeURIComponent(row.dataset.dk)}/protect` + dkQ(), {on});
      toast(on ? `${row.dataset.dk} is protected` : `${row.dataset.dk} is no longer protected`);
    } else if (b.dataset.dka === "wipe") {
      const name = row.dataset.dk, x = dkData.disks.find(d => d.name === name);
      let confirm = "";
      if (!x.empty) {
        confirm = prompt(`${name} has data (${(x.contents || []).join(", ")}). Everything on it will be gone for good.\n\nType ${name} to wipe it:`) || "";
        if (confirm !== name) return toast("Not wiped", true);
      } else if (!armed(b, "Wipe — sure?")) return;
      b.disabled = true;
      await call("POST", `/node/disks/${encodeURIComponent(name)}/wipe` + dkQ(), {confirm});
      toast(`${name} wiped`);
    } else if (b.hasAttribute("data-pooldel")) {
      const name = row.dataset.pool;
      const c = prompt(`Destroy storage ${name}? Its disks become empty again.\n\nType ${name} to destroy it:`) || "";
      if (c !== name) return;
      await call("DELETE", `/node/pools/${encodeURIComponent(name)}` + dkQ({confirm: c}));
      toast(`storage ${name} destroyed`);
    } else return;
  } catch (err) { toast(err.message, true); }
  loadDisks();
}

async function dkMake(){
  const name = $("#dkPName").value.trim().toLowerCase(), layout = $("#dkPLayout").value;
  const picked = [...document.querySelectorAll("[data-dkpick]:checked")].map(c => c.dataset.dkpick);
  if (!name) return toast("Give the storage a name", true);
  if (!picked.length) return toast("Tick the disk(s) to use", true);
  const go = $("#dkPMake"); go.disabled = true; go.textContent = "Making…";
  try { const r = await call("POST", "/node/pools" + dkQ(), {name, layout, disks: picked}); toast(r.note || "pool made"); }
  catch (e) { toast(e.message, true); }
  loadDisks();
}
