// Server page → Network storage: NAS shares (NFS: backup copies, ISOs) and
// SAN disks (iSCSI: VM disks). Uses the dashboard helpers ($, esc, call,
// toast, armed) and ndGB from node.js.
"use strict";

let netStores = null;

async function loadNetStorage(){
  try { netStores = await call("GET", "/storages"); }
  catch (e) { $("#ndNet").innerHTML = `<div class="sub">${esc(e.message)}</div>`; return; }
  renderNetStorage();
}

function netRow(s){
  const where = s.type === "nfs" ? `${esc(s.server)}:${esc(s.export)}` : `${esc(s.portal)} · ${esc(s.target)} · LUN ${s.lun}`;
  const state = s.online ? `<span style="color:var(--ok)">● online</span> · ${ndGB(s.free_bytes)} free of ${ndGB(s.size_bytes)}`
    : `<span style="color:var(--bad)">● ${esc(s.error || "offline")}</span>`;
  const vms = (s.vms || []).length ? ` · VMs: ${s.vms.map(esc).join(", ")}` : "";
  return `<tr data-n="${esc(s.name)}"><td><b>${esc(s.name)}</b> <span class="sub">${s.type === "nfs" ? "NFS" : "iSCSI"}</span>
      <div class="sub mono">${where}</div><div class="sub">${state} · holds ${esc(s.uses)}${vms}</div></td>
    <td style="text-align:right;white-space:nowrap"><button class="btn" data-ns="connect">Connect</button>
      <button class="btn danger" data-ns="rm">Remove</button></td></tr>`;
}

function renderNetStorage(){
  const l = netStores?.storages || [];
  $("#ndNet").innerHTML = `
    <div class="sub">A NAS share (NFS) keeps copies of every backup and your ISO collection. A SAN disk (iSCSI) holds VM disks, with
      snapshots and backups like local ones; one server uses a disk at a time.${netStores?.initiator ? ` This server's iSCSI name (allow it on the SAN): <span class="mono">${esc(netStores.initiator)}</span>` : ""}</div>
    <table class="snaps"><tbody>${l.length ? l.map(netRow).join("") : `<tr><td class="sub">None yet.</td></tr>`}</tbody></table>
    <details id="nsAddBox"><summary class="btn" style="display:inline-block">Add network storage</summary>
      <div class="row" style="margin-top:8px">
        <div><label for="nsType">Type</label><select id="nsType"><option value="nfs">NAS share (NFS)</option><option value="iscsi">SAN disk (iSCSI)</option></select></div>
        <div><label for="nsName">Name</label><input id="nsName" placeholder="nas" maxlength="20" autocomplete="off"></div>
      </div>
      <div id="nsNFS" class="row">
        <div><label for="nsServer">NAS address</label><input id="nsServer" placeholder="192.168.0.20" autocomplete="off"></div>
        <div><label for="nsExport">Shared folder</label><input id="nsExport" placeholder="/mnt/tank/p8s" autocomplete="off"></div>
        <div><label for="nsOpts">Options <span class="muted">(optional)</span></label><input id="nsOpts" placeholder="vers=3" autocomplete="off"></div>
      </div>
      <label id="nsBackupsL" class="check"><input id="nsBackups" type="checkbox" checked> Copy every backup here (keeps 7 per VM)</label>
      <div id="nsISCSI" hidden>
        <div class="row">
          <div><label for="nsPortal">SAN address</label><input id="nsPortal" placeholder="192.168.0.20" autocomplete="off"></div>
          <div><label>&nbsp;</label><button class="btn" id="nsFind" type="button">Find disks</button></div>
        </div>
        <div class="row">
          <div><label for="nsTarget">Target</label><input id="nsTarget" list="nsTargets" placeholder="iqn.2005-10.org.freenas.ctl:p8s" autocomplete="off"><datalist id="nsTargets"></datalist></div>
          <div><label for="nsLun">LUN</label><input id="nsLun" type="number" min="0" max="255" value="0"></div>
        </div>
        <div class="row">
          <div><label for="nsChapU">CHAP user <span class="muted">(optional)</span></label><input id="nsChapU" autocomplete="off"></div>
          <div><label for="nsChapP">CHAP password</label><input id="nsChapP" type="password" autocomplete="new-password"></div>
        </div>
        <label class="check"><input id="nsFormat" type="checkbox"> Format the disk for VMs (erases everything on it)</label>
      </div>
      <div class="inline"><span class="spacer"></span><button class="btn primary" id="nsAdd" type="button">Connect</button></div>
    </details>`;
  const type = () => $("#nsType").value;
  $("#nsType").onchange = () => {
    $("#nsNFS").hidden = $("#nsBackupsL").hidden = type() !== "nfs";
    $("#nsISCSI").hidden = type() !== "iscsi";
  };
  $("#nsFind").onclick = async () => {
    try {
      const r = await call("POST", "/storages/discover", {portal: $("#nsPortal").value.trim()});
      $("#nsTargets").innerHTML = r.targets.map(t => `<option value="${esc(t)}">`).join("");
      if (r.targets.length === 1) $("#nsTarget").value = r.targets[0];
      toast(r.targets.length ? `${r.targets.length} target(s) found` : "The SAN answered but offers no targets to this server", !r.targets.length);
    } catch (e) { toast(e.message, true); }
  };
  $("#nsAdd").onclick = async () => {
    const go = $("#nsAdd"), body = {name: $("#nsName").value.trim(), type: type()};
    if (type() === "nfs") Object.assign(body, {server: $("#nsServer").value.trim(), export: $("#nsExport").value.trim(),
      options: $("#nsOpts").value.trim(), backups: $("#nsBackups").checked});
    else Object.assign(body, {portal: $("#nsPortal").value.trim(), target: $("#nsTarget").value.trim(), lun: +$("#nsLun").value,
      chap_user: $("#nsChapU").value.trim(), chap_password: $("#nsChapP").value, format: $("#nsFormat").checked});
    go.disabled = true; go.textContent = "Connecting…";
    try {
      const r = await call("POST", "/storages", body);
      toast(`${body.name} connected. ${r.note || ""}`);
      await loadNetStorage();
      if (typeof refresh === "function") refresh();
    } catch (e) {
      toast(e.message, true);
      if (/confirm with format/.test(e.message)) $("#nsFormat").focus();
    }
    go.disabled = false; go.textContent = "Connect";
  };
  for (const b of $("#ndNet").querySelectorAll("button[data-ns]")) b.onclick = async () => {
    const name = b.closest("tr").dataset.n;
    try {
      if (b.dataset.ns === "rm") {
        if (!armed(b, "Disconnect — sure?")) return;
        await call("DELETE", `/storages/${encodeURIComponent(name)}`);
        toast(`${name} disconnected (its data stays on the NAS / SAN)`);
      } else {
        b.disabled = true;
        await call("POST", `/storages/${encodeURIComponent(name)}/connect`);
        toast(`${name} connected`);
      }
    } catch (e) { toast(e.message, true); }
    await loadNetStorage();
  };
}
