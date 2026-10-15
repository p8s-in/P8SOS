"use strict";
/* ---------- Settings → Virtual networks (SDN across the servers) ---------- */
// Networks that span every server of the cluster (VXLAN inside the tunnel).
// Uses the dashboard's globals: $, esc, call, toast, me, armed.

$("#dfBox").insertAdjacentHTML("afterend", `<div id="vnBox" hidden>
  <h3>Virtual networks</h3>
  <div class="sub">A switch that spans every server: VMs on the same virtual network talk to each other wherever they run,
    and keep doing so when they move. With a <b>subnet</b>, the master hands out addresses (DHCP), answers DNS and is the
    way to the internet; without one, set addresses inside the VMs yourself.</div>
  <table class="snaps"><tbody id="vnList"></tbody></table>
  <div class="inline" style="flex-wrap:wrap;gap:6px">
    <input id="vnName" placeholder="name, e.g. office" style="flex:1 1 140px" aria-label="Name">
    <input id="vnSubnet" placeholder="subnet, e.g. 10.60.0.0/24 (optional)" style="flex:1 1 200px" aria-label="Subnet">
    <label class="inline" style="margin:0;gap:4px"><input type="checkbox" id="vnNAT" checked> internet</label>
    <input id="vnComment" placeholder="what it is for" style="flex:2 1 160px" aria-label="Comment">
    <button class="btn primary" id="vnAdd" type="button">Add</button></div>
  <div class="hint" id="vnNote"></div>
</div>`);

let vnets = [];
const vnGateway = sub => { const p = sub.split("/")[0].split(".").map(Number); p[3] += 1; return p.join("."); };
function vnRender(admin){
  $("#vnList").innerHTML = vnets.length ? vnets.map((v, i) => `<tr data-i="${i}"><td><b>${esc(v.name)}</b> <span class="sub mono">vxb${v.vni}</span>
      ${v.comment ? `<div class="sub">${esc(v.comment)}</div>` : ""}</td>
    <td>${v.subnet ? `${esc(v.subnet)} <span class="sub">· gateway ${esc(vnGateway(v.subnet))} · DHCP${v.nat === false ? " · no internet" : " · internet"}</span>` : `<span class="sub">switch only (static addresses)</span>`}</td>
    <td style="text-align:right">${admin ? `<button class="btn danger" data-vnrm type="button">Remove</button>` : ""}</td></tr>`).join("")
    : `<tr><td class="sub">No virtual networks yet.</td></tr>`;
}
async function loadVNets(){
  if (me?.role !== "admin" && me?.role !== "operator" && me?.role !== "auditor") { $("#vnBox").hidden = true; return; }
  let r;
  try { r = await call("GET", "/sdn"); } catch { $("#vnBox").hidden = true; return; }
  $("#vnBox").hidden = false;
  vnets = r.vnets || [];
  const admin = me?.role === "admin";
  for (const id of ["#vnName", "#vnSubnet", "#vnNAT", "#vnComment", "#vnAdd"]) $(id).disabled = !admin;
  $("#vnNote").textContent = `VM cards on a virtual network use MTU ${r.mtu || 1230} (its frames travel inside the cluster tunnel).` +
    (r.gateway_here ? "" : " Change them on the master.");
  vnRender(admin);
}
async function vnSave(list, what){
  try {
    const r = await call("PUT", "/sdn", {vnets: list});
    vnets = r.vnets || [];
    vnRender(me?.role === "admin");
    toast(what);
    if (r.note) $("#vnNote").textContent = r.note;
    return true;
  } catch (e) { toast(e.message, true); return false; }
}
$("#vnAdd").onclick = async () => {
  const name = $("#vnName").value.trim().toLowerCase(), subnet = $("#vnSubnet").value.trim();
  if (!name) return toast("give it a name", true);
  const v = {name, subnet, comment: $("#vnComment").value.trim(), ...(subnet ? {nat: $("#vnNAT").checked} : {})};
  if (await vnSave([...vnets, v], `virtual network ${name} added on every server`)) {
    $("#vnName").value = $("#vnSubnet").value = $("#vnComment").value = "";
  }
};
$("#vnList").addEventListener("click", async e => {
  const b = e.target.closest("[data-vnrm]"); if (!b) return;
  if (!armed(b, "Remove — sure?")) return;
  const i = +b.closest("tr").dataset.i, name = vnets[i].name;
  await vnSave(vnets.filter((_, j) => j !== i), `virtual network ${name} removed`);
});
{
  const _openSettingsVN = openSettings;
  openSettings = async function(...a){ const r = await _openSettingsVN(...a); loadVNets(); return r; };
}
// for the create dialog and the network-card list
async function vnetOptions(sel, label){
  for (const o of sel.querySelectorAll("option[data-vnet]")) o.remove();
  const l = (await call("GET", "/sdn").catch(() => ({vnets: []}))).vnets || [];
  for (const n of l) {
    const o = document.createElement("option");
    o.value = "vnet:" + n.name; o.dataset.vnet = n.name;
    o.textContent = `${label} — ${n.name}${n.subnet ? " (" + n.subnet + ")" : ""}`;
    sel.append(o);
  }
}
