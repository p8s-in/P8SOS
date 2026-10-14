"use strict";
/* ---------- Settings → Datacenter firewall (admins) ---------- */
// IP sets, addresses blocked for every VM, and rules every VM with its
// firewall on gets. Uses the dashboard's globals: $, esc, call, toast, me.

$("#ssoBox").parentElement.querySelector("#tClose").closest(".dlg-actions").insertAdjacentHTML("beforebegin", `<div id="dfBox" hidden>
  <h3>Datacenter firewall</h3>
  <div class="sub">For every VM on every server of the cluster. <b>IP sets</b> are named address lists you can use as a rule's source
    (<span class="mono">+office</span>), here and in each VM's firewall.</div>
  <label>IP sets — one per line: <span class="mono">name: address, network, …</span></label>
  <textarea id="dfSets" rows="3" placeholder="office: 203.0.113.7, 198.51.100.0/24&#10;monitoring: 10.0.0.5"></textarea>
  <label for="dfBlock">Blocked for every VM (both ways) — addresses, networks or +sets, one per line</label>
  <textarea id="dfBlock" rows="2" placeholder="192.0.2.66&#10;+attackers"></textarea>
  <label>Allowed for every VM with its firewall on (before its own rules)</label>
  <table class="snaps"><tbody id="dfAllow"></tbody></table>
  <div class="inline"><button class="btn" id="dfAdd" type="button">+ Rule</button><span class="spacer"></span><button class="btn primary" id="dfSave" type="button">Save datacenter firewall</button></div>
  <div class="hint" id="dfNote"></div>
</div>`);

let dfRules = [];
function dfRow(r, i){
  return `<tr data-i="${i}"><td><select data-k="proto" style="min-width:80px">${["tcp", "udp", "icmp"].map(p => `<option ${p === r.proto ? "selected" : ""}>${p}</option>`).join("")}</select></td>
    <td><input data-k="ports" value="${esc(r.ports || "")}" placeholder="9100 or 80,443" aria-label="Ports"></td>
    <td><input data-k="source" value="${esc(r.source || "")}" placeholder="from anywhere (or +ipset)" aria-label="Source"></td>
    <td><input data-k="comment" value="${esc(r.comment || "")}" placeholder="what it is for" aria-label="Comment"></td>
    <td><button class="btn danger" data-dfrm type="button">✕</button></td></tr>`;
}
function dfRender(){ $("#dfAllow").innerHTML = dfRules.length ? dfRules.map(dfRow).join("") : `<tr><td class="sub">No datacenter-wide rules.</td></tr>`; }
function dfRead(){
  for (const tr of document.querySelectorAll("#dfAllow tr[data-i]")) {
    const r = dfRules[+tr.dataset.i];
    for (const el of tr.querySelectorAll("[data-k]")) r[el.dataset.k] = el.value.trim();
  }
}
async function loadDCFirewall(){
  if (me?.role !== "admin" && me?.role !== "operator" && me?.role !== "auditor") { $("#dfBox").hidden = true; return; }
  let g;
  try { g = await call("GET", "/firewall"); } catch { $("#dfBox").hidden = true; return; }
  $("#dfBox").hidden = false;
  $("#dfSets").value = Object.entries(g.sets || {}).map(([n, l]) => `${n}: ${l.join(", ")}`).join("\n");
  $("#dfBlock").value = (g.block || []).join("\n");
  dfRules = (g.allow || []).map(r => ({...r}));
  dfRender();
  const admin = me?.role === "admin";
  for (const id of ["#dfSets", "#dfBlock", "#dfAdd", "#dfSave"]) $(id).disabled = !admin;
}
$("#dfAdd").onclick = () => { dfRead(); dfRules.push({proto: "tcp", ports: "", source: "", comment: ""}); dfRender(); };
$("#dfAllow").addEventListener("click", e => {
  const b = e.target.closest("[data-dfrm]"); if (!b) return;
  dfRead(); dfRules.splice(+b.closest("tr").dataset.i, 1); dfRender();
});
$("#dfSave").onclick = async () => {
  dfRead();
  const sets = {};
  for (const line of $("#dfSets").value.split("\n")) {
    const [n, rest] = line.split(":", 2).length === 2 ? [line.slice(0, line.indexOf(":")), line.slice(line.indexOf(":") + 1)] : [line, ""];
    if (!n.trim()) continue;
    sets[n.trim()] = rest.split(/[\s,]+/).map(x => x.trim()).filter(Boolean);
  }
  const body = {sets, block: $("#dfBlock").value.split(/[\n,]+/).map(x => x.trim()).filter(Boolean), allow: dfRules.filter(r => r.proto)};
  try {
    const r = await call("PUT", "/firewall", body);
    $("#dfNote").textContent = r.note || "Saved and applied on every server.";
    toast("datacenter firewall saved");
  } catch (e) { toast(e.message, true); }
};
{
  const _openSettingsDF = openSettings;
  openSettings = async function(...a){ const r = await _openSettingsDF(...a); loadDCFirewall(); return r; };
}
