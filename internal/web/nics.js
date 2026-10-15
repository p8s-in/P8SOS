/* ---------- VM network cards after the first: Manage → Network ---------- */
// Uses the dashboard's globals: $, esc, call, toast, armed, refresh, mName, me, host.

document.querySelector('[data-tab="network"]').insertAdjacentHTML("afterbegin", `<div id="mNicBox" hidden>
  <h3>Network cards</h3>
  <div class="sub" id="mNicSub"></div>
  <table class="snaps"><tbody id="mNics"></tbody></table>
  <div class="inline" style="flex-wrap:wrap">
    <select id="mNicNet" style="flex:0 1 230px" aria-label="Network of the new card">
      <option value="nat">Private network (NAT)</option><option value="lan">Home network</option></select>
    <input id="mNicVlan" type="number" min="1" max="4094" placeholder="VLAN (optional)" style="flex:0 0 150px" aria-label="VLAN" hidden>
    <span class="spacer"></span><button class="btn" id="mNicAdd">Add network card</button></div>
  <div class="hint">A second card puts the VM on another network or VLAN at the same time (a router or firewall VM, a database on a private VLAN).
    It is plugged in at once while the VM runs; the VM's firewall covers every card.</div>
</div>`);

const nicNet = n => n.network === "lan" ? (n.vlan ? `VLAN ${n.vlan}` : "Home network") : n.network === "public" ? "Public IP"
  : (n.network || "").startsWith("vnet:") ? "Virtual network " + n.network.slice(5) : "Private network (NAT)";

function renderNICs(v){
  const box = $("#mNicBox"), staff = me?.role === "admin" || me?.role === "operator";
  box.hidden = !staff;
  if (!staff) return;
  const nics = v.nics || [];
  const key = v.name + "|" + JSON.stringify(nics) + "|" + v.mac + "|" + v.network + "|" + v.vlan;
  if ($("#mNics").dataset.key === key) return; // keep half-confirmed buttons
  $("#mNics").dataset.key = key;
  const lan = !!host?.networks?.includes("lan");
  if (typeof vnetOptions === "function" && !$("#mNicNet").dataset.vn) { $("#mNicNet").dataset.vn = "1"; vnetOptions($("#mNicNet"), "Virtual network"); }
  $("#mNicNet").querySelector('[value="lan"]').disabled = !lan;
  if (!lan) $("#mNicNet").value = "nat";
  $("#mNicVlan").hidden = $("#mNicNet").value !== "lan";
  $("#mNicSub").textContent = nics.length
    ? `${nics.length + 1} network cards.`
    : "One network card. Add another to put this VM on a second network or VLAN too.";
  $("#mNics").innerHTML = `<tr><td><b>card 1</b> <span class="sub mono">${esc(v.mac || "")}</span></td><td>${esc(nicNet(v))}</td><td style="text-align:right" class="sub">first card</td></tr>` +
    nics.map(n => `<tr data-mac="${esc(n.mac)}"><td><b>${esc(n.device)}</b> <span class="sub mono">${esc(n.mac)}</span></td><td>${esc(nicNet(n))}</td>
      <td style="text-align:right"><button class="btn danger" data-nic="rm">Remove</button></td></tr>`).join("");
  $("#mNicAdd").disabled = nics.length >= 7;
}

$("#mNicNet").onchange = () => { $("#mNicVlan").hidden = $("#mNicNet").value !== "lan"; };

$("#mNics").addEventListener("click", async e => {
  const b = e.target.closest("button[data-nic]");
  if (!b) return;
  const mac = b.closest("tr").dataset.mac;
  if (!armed(b, "Unplug it — sure?")) return;
  b.disabled = true;
  try { await call("DELETE", `/vms/${encodeURIComponent(mName)}/nics/${encodeURIComponent(mac)}`); toast("network card removed"); }
  catch (err) { toast(err.message, true); b.disabled = false; }
  $("#mNics").dataset.key = "";
  await refresh();
});

$("#mNicAdd").onclick = async () => {
  const go = $("#mNicAdd"), network = $("#mNicNet").value, vlan = network === "lan" ? +($("#mNicVlan").value || 0) : 0;
  if (vlan && (vlan < 1 || vlan > 4094)) return toast("VLAN must be 1-4094", true);
  go.disabled = true; go.textContent = "Adding…";
  try { const r = await call("POST", `/vms/${encodeURIComponent(mName)}/nics`, {network, vlan}); toast(r.note || "network card added"); }
  catch (err) { toast(err.message, true); }
  go.textContent = "Add network card"; go.disabled = false;
  $("#mNics").dataset.key = "";
  await refresh();
};
