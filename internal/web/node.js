// The Server page (admins): storage, OS templates, home network, Debian
// updates, reboot and the task log. Uses the dashboard helpers
// ($, esc, call, toast, isAdmin, API, token).
"use strict";

let node = null, nodeTimer = null, taskOpen = "";

document.body.insertAdjacentHTML("beforeend", `
<dialog id="dlgNode" class="wide"><div class="dlg-body">
  <div class="inline" style="margin:0"><h2 style="margin:0">Server <span class="sub" id="ndHost"></span></h2><span class="spacer"></span>
    <button class="btn" id="ndShell" type="button" title="Root shell on this server, in a new window (like at its keyboard)">&gt;_ Shell</button></div>
  <div id="ndRunning" class="toast" style="max-width:none;box-shadow:none;margin:8px 0;border-left-color:var(--warn)" hidden></div>

  <h3>Health &amp; alerts</h3>
  <div id="ndHealth"></div>

  <h3>Memory</h3>
  <div id="ndMem"></div>

  <h3>VM storage</h3>
  <div id="ndStorage"></div>

  <h3>Network storage</h3>
  <div id="ndNet"></div>

  <h3>Ceph</h3>
  <div id="ndCeph"></div>

  <h3>OS templates</h3>
  <div class="sub">Images new VMs are made from. Adding one downloads it from the publisher (a few hundred MB) and checks its checksum.</div>
  <table class="snaps"><tbody id="ndTpl"></tbody></table>

  <div id="ndCTBox" hidden>
    <h3>Container templates</h3>
    <div class="sub">Official linuxcontainers.org images (50-200 MB each), checked against their checksums. Every container is an instant copy of one.</div>
    <table class="snaps"><tbody id="ndCTTpl"></tbody></table>
  </div>

  <h3>Home network</h3>
  <div id="ndLan"></div>
  <div class="inline" style="margin-top:6px"><button class="btn" id="ndNetCheck">Check guest internet</button><button class="btn" id="ndNetFix">Repair</button>
    <span class="sub">VMs or containers can't reach the internet? Check finds why (NAT rules, forwarding, DHCP, firewall); Repair fixes it while they keep running.</span></div>

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
  if (!isAdmin() || !$("#bNode")) return;
  $("#bNode").hidden = false;
  $("#bNode").onclick = () => openNode();
  alertBadge(); setInterval(alertBadge, 60000);
}

// red/orange count on the server button while something is wrong
async function alertBadge(){
  let st; try { st = await call("GET", "/alerts"); } catch { return; }
  const b = $("#bNode"); if (!b) return;
  b.querySelector(".alert-badge")?.remove();
  const n = st.active.length;
  if (!n) return;
  const crit = st.active.some(a => a.severity === "critical");
  b.insertAdjacentHTML("beforeend", `<span class="badge alert-badge" style="background:var(${crit ? "--bad" : "--warn"})" title="${esc(st.active.map(a => a.title).join("\n"))}">${n}</span>`);
}

// Health & alerts: what is wrong now, the checks, where alerts go
async function loadHealth(){
  const box = $("#ndHealth");
  if (box.contains(document.activeElement)) return; // don't wipe what is being typed
  const wasOpen = [...box.querySelectorAll("details")].map(d => d.open);
  let st;
  try { st = await call("GET", "/alerts"); } catch (e) { $("#ndHealth").innerHTML = `<div class="sub">${esc(e.message)}</div>`; return; }
  const s = st.settings, col = {critical: "var(--bad)", warning: "var(--warn)", ok: "var(--ok)"};
  const icon = {critical: "✘", warning: "!", ok: "✔"};
  const act = st.active.length ? st.active.map(a => `<div class="toast" style="max-width:none;box-shadow:none;margin:6px 0;border-left-color:${col[a.severity]}">
      <b>${esc(a.title)}</b><div class="sub" style="white-space:pre-wrap">${esc(a.detail)}</div><div class="sub">since ${esc(ndWhen(a.since))}</div></div>`).join("")
    : `<div class="sub" style="color:var(--ok)">✔ Everything looks fine.</div>`;
  const checks = st.checks.map(c => `<tr><td><b style="color:${col[c.status] || "inherit"}">${icon[c.status] || "·"}</b> ${esc(c.name)}</td><td class="sub" style="text-align:right">${esc(c.value)}</td></tr>`).join("");
  const hist = st.history.slice(0, 15).map(e => `<tr><td class="sub" style="white-space:nowrap">${esc(ndWhen(e.time))}</td><td>${{raised: "▲", worse: "▲▲", resolved: "✔", action: "⏻", test: "✉"}[e.what] || ""} ${esc(e.title)}</td></tr>`).join("");
  const none = !s.emails.length && !s.webhook && !s.ntfy;
  $("#ndHealth").innerHTML = act +
    (none ? `<div class="sub" style="color:var(--warn)">Alerts aren't sent anywhere yet: add an email, a webhook or an ntfy topic below.</div>` : "") +
    `<details style="margin-top:6px"><summary class="sub">What is watched (${st.checks.length})</summary><table class="snaps"><tbody>${checks || '<tr><td class="sub">First check runs about a minute after start.</td></tr>'}</tbody></table></details>
    <details style="margin-top:4px" ${none ? "open" : ""}><summary class="sub">Where alerts go, and power / heat protection</summary>
      <label for="alEmails">Email addresses <span class="muted">(comma separated; needs email set up under Settings)</span></label><input id="alEmails" value="${esc(s.emails.join(", "))}" placeholder="ops@example.com">
      <label for="alHook">Webhook <span class="muted">(Slack, Discord, Teams, Mattermost… gets a JSON POST)</span></label><input id="alHook" value="${esc(s.webhook || "")}" placeholder="https://hooks.slack.com/services/…">
      <label for="alNtfy">Phone push via ntfy <span class="muted">(install the ntfy app, subscribe to the same topic)</span></label><input id="alNtfy" value="${esc(s.ntfy || "")}" placeholder="https://ntfy.sh/p8s-some-secret-name">
      <div class="row" style="grid-template-columns:1fr 1fr 1fr">
        <div><label for="alPoolW">Pool full: warn at %</label><input id="alPoolW" type="number" min="50" max="99" value="${s.pool_warn_pct}"></div>
        <div><label for="alPoolC">Pool full: critical at %</label><input id="alPoolC" type="number" min="50" max="99" value="${s.pool_crit_pct}"></div>
        <div><label for="alDisk">System disk: warn at %</label><input id="alDisk" type="number" min="50" max="99" value="${s.disk_warn_pct}"></div>
        <div><label for="alMem">RAM: warn at %</label><input id="alMem" type="number" min="50" max="99" value="${s.mem_warn_pct}"></div>
        <div><label for="alCPU">CPU temp: warn at °C</label><input id="alCPU" type="number" min="50" max="105" value="${s.cpu_temp_warn_c}"></div>
        <div><label for="alDT">Disk temp: warn at °C</label><input id="alDT" type="number" min="35" max="80" value="${s.disk_temp_warn_c}"></div>
      </div>
      <label style="display:flex;gap:8px;align-items:center;font-weight:400;margin-top:8px"><input id="alPower" type="checkbox" style="width:auto" ${s.power_shutdown ? "checked" : ""}>
        On a power cut, shut the VMs and containers down cleanly and power off before the battery runs out</label>
      <div class="row" style="grid-template-columns:1fr 1fr 1fr">
        <div><label for="alBat">…when the battery is at %</label><input id="alBat" type="number" min="1" max="50" value="${s.battery_shutdown_pct}"></div>
        <div><label for="alUPS">UPS (NUT name)</label><input id="alUPS" value="${esc(s.ups || "")}" placeholder="ups@localhost (empty: laptop battery)"></div>
        <div><label for="alHot">Overheat shutdown at °C</label><input id="alHot" type="number" min="0" max="110" value="${s.thermal_shutdown_c || 0}" title="0 = never"></div>
      </div>
      <div class="inline" style="margin-top:8px"><button class="btn" id="alTest">Send a test</button><span class="spacer"></span><button class="btn primary" id="alSave">Save</button></div>
    </details>
    ${hist ? `<details style="margin-top:4px"><summary class="sub">Recent alerts</summary><table class="snaps"><tbody>${hist}</tbody></table></details>` : ""}`;
  if (wasOpen.length) box.querySelectorAll("details").forEach((d, i) => { if (i < wasOpen.length) d.open = wasOpen[i]; });
  $("#alSave").onclick = async () => {
    const body = {emails: $("#alEmails").value.split(/[,;\s]+/).filter(Boolean), webhook: $("#alHook").value.trim(), ntfy: $("#alNtfy").value.trim(),
      pool_warn_pct: +$("#alPoolW").value, pool_crit_pct: +$("#alPoolC").value, disk_warn_pct: +$("#alDisk").value, mem_warn_pct: +$("#alMem").value,
      cpu_temp_warn_c: +$("#alCPU").value, disk_temp_warn_c: +$("#alDT").value, power_shutdown: $("#alPower").checked,
      battery_shutdown_pct: +$("#alBat").value, ups: $("#alUPS").value.trim(), thermal_shutdown_c: +$("#alHot").value};
    try { await call("PUT", "/alerts/settings", body); toast("Alert settings saved"); loadHealth(); } catch (e) { toast(e.message, true); }
  };
  $("#alTest").onclick = async () => {
    try { await call("POST", "/alerts/test"); toast("Test sent — check your email / chat / phone"); } catch (e) { toast(e.message, true); }
  };
  alertBadge();
}

async function openNode(){
  taskOpen = "";
  $("#ndShell").hidden = !isFullAdmin(); // operators and auditors: no root shell
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
  loadMem(); loadHealth();
  if (!quiet && typeof loadNetStorage === "function") loadNetStorage();
  if (!quiet && typeof loadCeph === "function") loadCeph();
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

  $("#ndCTBox").hidden = false;
  if (!n.containers) {
    $("#ndCTTpl").innerHTML = `<tr><td class="sub">Containers are off: LXC isn't installed on this server. Run <code>sudo bash update.sh</code> (needs internet), then reload this page.</td></tr>`;
  } else if (!(n.ct_catalog || []).length) {
    $("#ndCTTpl").innerHTML = `<tr><td class="sub">The container template list isn't available (make-ct-template.sh missing — run <code>sudo bash update.sh</code>).</td></tr>`;
  } else
  $("#ndCTTpl").innerHTML = (n.ct_catalog || []).map(c => `<tr><td><b>${esc(c.label)}</b><div class="sub mono">${esc(c.name)}</div></td>
    <td style="text-align:right">${c.installed ? `<span class="sub">✔ added</span>` : `<button class="btn" data-cttpl="${esc(c.name)}" ${dis}>Add</button>`}</td></tr>`).join("");
  for (const b of $("#ndCTTpl").querySelectorAll("[data-cttpl]")) b.onclick = () => runTask("ct-template", b.dataset.cttpl,
    "Add the " + b.dataset.cttpl + " container template? It downloads 50-200 MB.");

  $("#ndLan").innerHTML = n.lan
    ? `<div class="sub">✔ VMs can join your home network (choose "Home network" when creating a VM).</div>`
    : `<div class="inline"><button class="btn" id="ndLanGo" ${dis}>Set up</button><span class="sub">Lets VMs get addresses from your router, like other devices. Needs an Ethernet cable;
       the network drops for a few seconds and the old settings come back by themselves if anything goes wrong.</span></div>`;
  const nw = n.network || {ports: []};
  const portRow = p => `<tr><td><b class="mono">${esc(p.name)}</b>${p.wifi ? ' <span class="sub">Wi-Fi</span>' : ""}</td>
    <td class="sub">${p.carrier ? `<span style="color:var(--ok)">● cable in</span>${p.speed ? " · " + (p.speed >= 1000 ? p.speed / 1000 + " Gbit/s" : p.speed + " Mbit/s") : ""}` : "○ no cable"}
      ${p.in ? " · part of " + esc(p.in) : ""}${p.active ? " · <b>active</b>" : ""}</td></tr>`;
  const free = nw.ports.filter(p => !p.wifi && !p.in && p.name !== nw.uplink);
  $("#ndLan").insertAdjacentHTML("beforeend", (nw.ports.length ? `<table class="snaps" style="margin-top:6px"><tbody>${nw.ports.map(portRow).join("")}</tbody></table>` : "") +
    (n.lan && nw.bond_mode ? `<div class="sub">The home network runs on <b>${esc(nw.uplink)}</b> (${esc(nw.bond_mode)}): if one cable fails, the other takes over.</div>` : "") +
    (n.lan && !nw.bond_mode && free.length ? `<div class="inline" style="margin-top:6px"><select id="ndBondPort" style="width:auto">${free.map(p => `<option>${esc(p.name)}</option>`).join("")}</select>
      <button class="btn" id="ndBondGo" ${dis}>Add as second cable</button><span class="sub">Failover: the server and VMs stay online if one cable or switch port dies (both cables to the same network).</span></div>` : "") +
    `<div class="inline" style="margin-top:6px">${nw.ipv6_nat
      ? `<span class="sub">IPv6 on the private network: <b class="mono">${esc(nw.ipv6_nat)}</b></span><button class="btn" id="ndV6Off" ${dis}>Turn off</button>`
      : `<button class="btn" id="ndV6On" ${dis || (nw.host_ipv6 ? "" : "disabled")}>Turn on IPv6 for the private network</button>
         <span class="sub">${nw.host_ipv6 ? "VMs on the private network get IPv6 internet too (home-network VMs get it from the router already)." : "This server has no IPv6 internet."}</span>`}</div>`);
  if ($("#ndBondGo")) $("#ndBondGo").onclick = () => runTask("bond-setup", $("#ndBondPort").value,
    `Add ${$("#ndBondPort").value} as a second cable? The network drops for a few seconds; if it doesn't come back within 60 s the old settings are restored.`);
  if ($("#ndV6On")) $("#ndV6On").onclick = () => runTask("ipv6-on", "", "Turn on IPv6 for the private network? Its network restarts for a moment; VMs stay connected.");
  if ($("#ndV6Off")) $("#ndV6Off").onclick = () => runTask("ipv6-off", "", "Turn off IPv6 for the private network?");
  $("#ndNetCheck").disabled = $("#ndNetFix").disabled = busy;
  $("#ndNetCheck").onclick = () => runTask("net-check", "");
  $("#ndNetFix").onclick = () => runTask("net-repair", "", "Repair the guest network now? Running VMs and containers keep running; the dashboard may restart for a moment.");
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

// Memory: where the RAM goes, the biggest programs, and the ZFS cache limit.
const ndMiB = m => m >= 1024 ? (m / 1024).toFixed(m >= 10240 ? 0 : 1) + " GB" : m + " MB";
async function loadMem(){
  let d;
  try { d = await call("GET", "/node/memory"); } catch { return; }
  const m = d.mem, t = m.total_mib || 1;
  const parts = [
    ["VMs + containers", m.guests_mib, "var(--series-1)", "running guests"],
    ["System", m.system_mib, "var(--series-2)", "Debian, p8sd, libvirt and other programs"],
    ["ZFS cache", Math.max(0, t - m.used_mib - m.cache_mib - m.free_mib), "var(--series-3)", "ZFS keeps recently read disk blocks here and gives the RAM back when VMs need it"],
    ["File cache", Math.max(0, m.cache_mib), "var(--series-4)", "Linux's normal disk cache, given back when needed"],
  ];
  const free = Math.max(0, t - parts.reduce((a, p) => a + p[1], 0));
  const bar = parts.concat([["Free", free, "var(--line)", "not used at all"]]).filter(p => p[1] > 0)
    .map(p => `<i title="${esc(p[0])}: ${ndMiB(p[1])} — ${esc(p[3])}" style="width:${(p[1] / t * 100).toFixed(2)}%;background:${p[2]};display:inline-block;height:100%"></i>`).join("");
  const lim = d.zfs_cache_max_mib;
  $("#ndMem").innerHTML = `<div class="inline"><b>${ndMiB(m.used_mib)} really used</b><span class="sub">of ${ndMiB(m.total_mib)}${m.swap_used_mib ? ` · swap ${ndMiB(m.swap_used_mib)}` : ""}${m.ksm_saved_mib > 64 ? ` · KSM saves ${ndMiB(m.ksm_saved_mib)}` : ""}</span></div>
    <div style="height:14px;border-radius:7px;overflow:hidden;background:var(--bg);border:1px solid var(--line);margin:6px 0;white-space:nowrap;font-size:0">${bar}</div>
    <div class="sub">${parts.map(p => `<span title="${esc(p[3])}"><b style="color:${p[2]}">■</b> ${esc(p[0])} ${ndMiB(p[1])}</span>`).join(" · ")} · free ${ndMiB(free)}</div>
    <div class="sub" style="margin-top:4px">The ZFS cache and file cache are not counted as used: they speed up disks and shrink by themselves when VMs start.</div>
    ${m.zfs_cache_mib || lim ? `<div class="inline" style="margin-top:6px"><span class="sub">ZFS cache limit</span>
      <input id="ndArc" type="number" min="256" step="256" style="width:110px;max-width:110px;flex:none" value="${lim || ""}" placeholder="default"> <span class="sub">MB${lim ? "" : " (ZFS default: half the RAM)"}</span>
      <button class="btn" id="ndArcSet">Set</button><button class="btn" id="ndArcAuto" title="10% of RAM, 1-16 GB">Auto</button></div>` : ""}
    <details style="margin-top:6px"><summary class="sub">Biggest programs</summary><table class="snaps"><tbody>${(d.top || []).map(p =>
      `<tr><td>${esc(p.name)}${p.count > 1 ? ` <span class="sub">×${p.count}</span>` : ""}</td><td style="text-align:right" class="mono">${ndMiB(p.rss_mib)}</td></tr>`).join("")}</tbody></table></details>`;
  if ($("#ndArcSet")) $("#ndArcSet").onclick = () => {
    const v = parseInt($("#ndArc").value, 10);
    if (!(v >= 256)) return toast("Give the limit in MB, 256 or more", true);
    runTask("zfs-cache", String(v), `Limit the ZFS cache to ${ndMiB(v)}? It shrinks within a minute; VMs keep running.`);
  };
  if ($("#ndArcAuto")) $("#ndArcAuto").onclick = () => runTask("zfs-cache", "auto", "Set the ZFS cache limit to 10% of RAM (1-16 GB)?");
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

$("#ndShell").onclick = () => window.open("/terminal?host=1", "p8s-host-shell");
$("#ndClose").onclick = () => { clearTimeout(nodeTimer); $("#dlgNode").close(); };
$("#ndLogClose").onclick = () => { taskOpen = ""; $("#ndLogBox").hidden = true; };
