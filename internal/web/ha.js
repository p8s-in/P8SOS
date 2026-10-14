"use strict";
/* High availability in the dashboard (admins): the box in VM Manage → Options
   and the High availability section on the Server page.
   Uses index.html's $, call, esc, toast, armed, isAdmin, mName, fmtBytes, refresh. */

const haWhen = s => { if (!s) return "never"; const d = new Date(s); if (isNaN(d)) return s;
  const m = Math.round((Date.now() - d) / 60000); return m < 1 ? "just now" : m < 60 ? m + " min ago" : d.toLocaleString(); };

/* ---------- VM Manage → Options ---------- */
$("#mMoveBox").insertAdjacentHTML("afterend", `<div id="mHABox" hidden><h3>High availability</h3>
  <div id="mHA"></div></div>`);

async function loadHA(){
  const box = $("#mHABox");
  if (!isAdmin()) { box.hidden = true; return; }
  const name = mName;
  let info;
  try { info = await call("GET", `/vms/${encodeURIComponent(name)}/ha`); } catch { box.hidden = true; return; }
  if (name !== mName) return;
  if (!info.targets.length && !info.protected) { box.hidden = true; return; } // no cluster
  if (box.contains(document.activeElement)) return;
  box.hidden = false;
  const opts = info.targets.map(t => `<option value="${esc(t.id)}" ${t.online ? "" : "disabled"}>${esc(t.name)}${t.online ? "" : " (offline)"}</option>`).join("");
  if (!info.protected) {
    $("#mHA").innerHTML = info.why ? `<div class="sub">Not possible: ${esc(info.why)}</div>` : `
      <div class="sub">Keeps a copy of this VM on another server, updated every few minutes (also while it runs). If this VM's server dies,
        the copy is started there automatically. Changes since the last copy are lost then, like after a power cut.</div>
      <div class="row" style="grid-template-columns:1fr 1fr">
        <div><label for="haTo">Copy to</label><select id="haTo">${opts}</select></div>
        <div><label for="haEvery">Copy every (minutes)</label><input id="haEvery" type="number" min="1" max="1440" value="15"></div>
      </div>
      <div class="inline" style="margin-top:6px"><span class="spacer"></span><button class="btn primary" id="haOn">Protect</button></div>`;
    if ($("#haOn")) $("#haOn").onclick = async () => {
      const b = $("#haOn"); b.disabled = true;
      try { await call("PUT", `/vms/${encodeURIComponent(name)}/ha`, {enabled: true, target: $("#haTo").value, every_min: +$("#haEvery").value});
        toast(`${name} is protected; the first copy is being made`); setTimeout(loadHA, 1500); }
      catch (e) { toast(e.message, true); b.disabled = false; }
    };
    return;
  }
  const c = info.config;
  const lines = [
    `Copies to <b>${esc(c.target_name)}</b> every ${c.every_min} min · last copy <b>${esc(haWhen(c.last_copy))}</b>${c.copy_bytes ? " (" + fmtBytes(c.copy_bytes) + ")" : ""}${c.copying ? " · copying now…" : ""}`,
    c.auto ? `<span style="color:var(--ok)">✔ Automatic failover</span> if ${esc(c.node_name)} goes offline`
      : `<span style="color:var(--warn)">Failover by hand:</span> ${esc(c.why || "")}`,
  ];
  if (c.error) lines.push(`<span style="color:var(--bad)">Last copy failed: ${esc(c.error)}</span>`);
  if (c.failed_over_at) lines.push(`Started here from its copy ${esc(haWhen(c.failed_over_at))} (${esc(c.failed_over_from)} was offline)${c.retire?.length ? " — the old one is switched off when that server is back" : ""}`);
  $("#mHA").innerHTML = lines.map(l => `<div class="sub">${l}</div>`).join("") + `
    <div class="inline" style="margin-top:6px;flex-wrap:wrap">
      ${!c.node_online ? `<button class="btn danger" id="haFail" title="Only when ${esc(c.node_name)} is really off">Start the copy on ${esc(c.target_name)}</button>` : ""}
      <span class="spacer"></span><button class="btn" id="haOff">Turn off</button></div>`;
  $("#haOff").onclick = async e => {
    if (!armed(e.target, "Delete the copy?")) return;
    try { await call("PUT", `/vms/${encodeURIComponent(name)}/ha`, {enabled: false}); toast(`${name} is no longer protected`); loadHA(); }
    catch (err) { toast(err.message, true); }
  };
  if ($("#haFail")) $("#haFail").onclick = async e => {
    if (!armed(e.target, `Sure ${c.node_name} is off?`)) return;
    e.target.disabled = true;
    try { const r = await call("POST", `/vms/${encodeURIComponent(name)}/ha/failover`); toast(r.message); refresh(); loadHA(); }
    catch (err) { toast(err.message, true); e.target.disabled = false; }
  };
}

/* ---------- Server page ---------- */
$("#clBox").insertAdjacentHTML("afterend", `<div id="haBox" hidden>
  <h3>High availability</h3>
  <div id="haSrv"></div>
</div>`);

async function loadHASrv(){
  const box = $("#haBox");
  if (box.contains(document.activeElement)) return;
  let st;
  try { st = await call("GET", "/ha"); } catch { box.hidden = true; return; }
  st.replicas = st.replicas || [];
  if (!st.role && !st.replicas.length) { box.hidden = true; return; }
  box.hidden = false;
  const wasOpen = [...box.querySelectorAll("details")].map(d => d.open);
  const sev = {critical: "var(--bad)", warning: "var(--warn)", info: "var(--muted)"};
  const vmsRows = st.vms.map(v => `<tr><td><b>${esc(v.name)}</b><div class="sub">on ${esc(v.node_name)}${v.node_online ? "" : " (offline)"} → copy on ${esc(v.target_name)}</div></td>
      <td class="sub">${v.error ? `<span style="color:var(--bad)">${esc(v.error)}</span>` : "last copy " + esc(haWhen(v.last_copy))}<br>${v.auto ? "automatic failover" : "failover by hand"}</td></tr>`).join("");
  const copies = st.replicas.map(r => `<tr data-copy="${esc(r.name)}"><td><b>${esc(r.name)}</b><div class="sub">from ${esc(r.from || "?")} · ${esc(haWhen(r.at))} · ${fmtBytes(r.bytes || 0)}</div></td>
      <td style="text-align:right;white-space:nowrap"><button class="btn danger" data-ha="start" title="Only when the VM's own server is really off">Start here</button> <button class="btn" data-ha="drop">Delete</button></td></tr>`).join("");
  const ev = st.events.slice(0, 20).map(e => `<tr><td class="sub" style="white-space:nowrap">${esc(new Date(e.time).toLocaleString())}</td><td><span style="color:${sev[e.severity] || "inherit"}">●</span> ${esc(e.text)}</td></tr>`).join("");
  $("#haSrv").innerHTML = (st.role === "master" ? `<div class="sub">Protect a VM in its Manage → Options. ${st.vms.length ? "" : "No VM is protected yet."}</div>
      ${vmsRows ? `<table class="snaps"><tbody>${vmsRows}</tbody></table>` : ""}` :
      st.role === "worker" ? `<div class="sub">The master runs high availability. Last word from it: ${esc(haWhen(st.lease))}.</div>` : "") +
    (copies ? `<div class="sub" style="margin-top:6px"><b>Copies of VMs kept on this server.</b> If a VM's server (or the master) is gone for good, start its copy here.</div>
      <table class="snaps"><tbody id="haCopies">${copies}</tbody></table>` : "") +
    (st.role === "master" ? `<details style="margin-top:4px"><summary class="sub">Timings</summary>
      <div class="row" style="grid-template-columns:1fr 1fr">
        <div><label for="haFence">A worker cut off from the master and its network switches its protected VMs off after (s)</label><input id="haFence" type="number" min="20" value="${st.settings.fence_after_s}"></div>
        <div><label for="haFail2">The master starts the copies of an unreachable server's VMs after (s)</label><input id="haFail2" type="number" min="80" value="${st.settings.failover_after_s}"></div>
      </div>
      <div class="inline"><span class="spacer"></span><button class="btn" id="haSave">Save</button></div></details>` : "") +
    (ev ? `<details style="margin-top:4px"><summary class="sub">Log</summary><table class="snaps"><tbody>${ev}</tbody></table></details>` : "");
  if (wasOpen.length) box.querySelectorAll("details").forEach((d, i) => { if (i < wasOpen.length) d.open = wasOpen[i]; });
  if ($("#haSave")) $("#haSave").onclick = async () => {
    try { await call("PUT", "/ha/settings", {fence_after_s: +$("#haFence").value, failover_after_s: +$("#haFail2").value}); toast("Saved"); loadHASrv(); }
    catch (e) { toast(e.message, true); }
  };
  if ($("#haCopies")) $("#haCopies").onclick = async e => {
    const b = e.target.closest("button[data-ha]"); if (!b) return;
    const n = b.closest("tr").dataset.copy;
    try {
      if (b.dataset.ha === "drop") {
        if (!armed(b, "Delete this copy?")) return;
        await call("DELETE", `/ha/replicas/${encodeURIComponent(n)}`); toast(`Copy of ${n} deleted`);
      } else {
        if (!armed(b, "Sure its server is off?")) return;
        b.disabled = true;
        const r = await call("POST", `/ha/replicas/${encodeURIComponent(n)}/start`, {start: true});
        toast(`${n} started here${r.note ? " — " + r.note : ""}`); refresh();
      }
      loadHASrv();
    } catch (err) { toast(err.message, true); b.disabled = false; }
  };
}

if (typeof openNode === "function") {
  const _openNodeHA = openNode;
  openNode = async function(){ loadHASrv(); return _openNodeHA(); };
}
