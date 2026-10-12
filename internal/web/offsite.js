"use strict";
/* All backups: container backups and off-site copies (admins). Uses index.html's
   helpers: $, call, esc, toast, trackJob, armed, fmtTime, fmtBytes, isAdmin. */

document.querySelector("#dlgBackups .dlg-body > .dlg-actions").insertAdjacentHTML("beforebegin", `
<div id="gCTBox" hidden>
  <h3>Container backups</h3>
  <table class="snaps"><tbody id="gCTBackups"></tbody></table>
</div>
<div id="gOffBox" hidden>
  <h3>Off-site copies</h3>
  <div class="sub">Every new backup is also copied to the places below, so a broken disk or a stolen server doesn't take the backups with it.
    Each place keeps the newest copies per VM / container.</div>
  <div id="oNotice" class="hint" hidden></div>
  <table class="snaps"><tbody id="oTargets"></tbody></table>
  <div class="inline"><span class="spacer"></span><button class="btn" id="oAdd" type="button">+ Add a place</button></div>
  <form id="oForm" hidden style="border:1px solid var(--line);border-radius:8px;padding:10px;margin-top:8px">
    <div class="row">
      <div><label for="oName">Name</label><input id="oName" placeholder="e.g. usb, b2, office" pattern="[a-z][a-z0-9-]{0,30}" required></div>
      <div><label for="oType">Where</label><select id="oType">
        <option value="dir">A folder (USB disk, NAS share)</option>
        <option value="s3">Cloud storage (S3, Backblaze B2, R2, Wasabi)</option>
        <option value="ssh">Another server (SSH)</option></select></div>
    </div>
    <div data-t="dir"><label for="oPath">Folder on this server</label><input id="oPath" placeholder="/mnt/usb-backup">
      <div class="hint">Mount the disk there first (it must stay mounted; copies wait while it isn't).</div></div>
    <div data-t="s3">
      <div class="row">
        <div><label for="oEndpoint">Endpoint</label><input id="oEndpoint" placeholder="https://s3.ap-south-1.amazonaws.com"></div>
        <div><label for="oRegion">Region</label><input id="oRegion" placeholder="ap-south-1 (R2: auto)"></div>
      </div>
      <div class="row">
        <div><label for="oBucket">Bucket</label><input id="oBucket"></div>
        <div><label for="oPrefix">Folder in the bucket</label><input id="oPrefix" placeholder="optional, e.g. server1"></div>
      </div>
      <div class="row">
        <div><label for="oAK">Access key</label><input id="oAK" autocomplete="off"></div>
        <div><label for="oSK">Secret key</label><input id="oSK" type="password" autocomplete="new-password"></div>
      </div>
      <label class="check"><input id="oVH" type="checkbox"> Bucket in the host name (AWS buckets made after 2020)</label>
    </div>
    <div data-t="ssh">
      <div class="row">
        <div><label for="oHost">Server</label><input id="oHost" placeholder="backup.example.com or 192.168.0.20"></div>
        <div><label for="oUser">User</label><input id="oUser" placeholder="root"></div>
        <div><label for="oPort">Port</label><input id="oPort" type="number" placeholder="22"></div>
      </div>
      <label for="oRDir">Folder there</label><input id="oRDir" placeholder="/var/lib/p8s-offsite">
      <div class="hint">Add this server's key to <span class="mono">~/.ssh/authorized_keys</span> of that user there:</div>
      <pre id="oKey" class="mono copy" title="Click to copy" style="white-space:pre-wrap;word-break:break-all;font-size:12px;background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:8px"></pre>
    </div>
    <div class="row">
      <div><label for="oKeep">Keep per VM / container</label><input id="oKeep" type="number" min="1" max="365" value="7"></div>
      <div><label for="oPass">Encryption passphrase</label><input id="oPass" type="password" autocomplete="new-password" placeholder="12+ characters, optional"></div>
    </div>
    <div class="hint" id="oPassHint">With a passphrase, only someone who knows it can read the copies. Write it down: without it they can't be restored.</div>
    <label class="check"><input id="oOn" type="checkbox" checked> Copy every new backup here</label>
    <div class="dlg-actions"><button class="btn" type="button" id="oCancel">Cancel</button><button class="btn primary" id="oSave">Save and test</button></div>
  </form>
  <div id="oStoredBox" hidden>
    <h3 id="oStoredTitle"></h3>
    <table class="snaps"><tbody id="oStored"></tbody></table>
  </div>
</div>`);

let oTargets = [], oEditing = null, oAllowed = true;

function oShowType(){
  for (const el of document.querySelectorAll("#oForm [data-t]")) el.hidden = el.dataset.t !== $("#oType").value;
}
$("#oType").onchange = oShowType;

function oStatus(t){
  const s = t.status || {};
  if (s.running) return `<span class="state starting"><span class="dot"></span>copying ${esc(s.running)}</span>`;
  if (s.last_error) return `<span class="state stopped" title="${esc(s.last_error)}"><span class="dot"></span>problem</span> <span class="sub">${esc(s.last_error)}</span>`;
  if (s.last_ok) return `<span class="state running"><span class="dot"></span>ok</span> <span class="sub">last copy ${esc(fmtTime(s.last_ok))}</span>`;
  return `<span class="sub">no copies yet</span>`;
}
function oWhere(t){
  if (t.type === "dir") return "folder " + t.path;
  if (t.type === "s3") return `bucket ${t.bucket}${t.prefix ? "/" + t.prefix : ""} at ${t.endpoint.replace(/^https?:\/\//, "")}`;
  return `${t.user}@${t.host}:${t.remote_dir}`;
}

async function loadOffsite(){
  if (!isAdmin()) { $("#gOffBox").hidden = true; return; }
  let r;
  try { r = await call("GET", "/offsite"); } catch { $("#gOffBox").hidden = true; return; }
  $("#gOffBox").hidden = false;
  oTargets = r.targets || []; oAllowed = r.allowed !== false;
  $("#oKey").textContent = r.ssh_public_key || "";
  $("#oNotice").hidden = oAllowed;
  $("#oNotice").textContent = "Off-site copies need a P8S OS subscription (Standard or higher).";
  $("#oTargets").innerHTML = oTargets.length ? oTargets.map(t => `<tr data-o="${esc(t.name)}">
      <td colspan="2" style="text-align:left"><b>${esc(t.name)}</b>${t.enabled ? "" : ' <span class="pill">paused</span>'}${t.has_passphrase ? ' <span class="pill">encrypted</span>' : ""}
        <div class="sub" style="word-break:break-all">${esc(oWhere(t))} · keeps ${t.keep}</div><div>${oStatus(t)}</div></td>
      </tr><tr data-o="${esc(t.name)}"><td colspan="2" style="border-top:0;padding-top:0;text-align:left"><div class="inline" style="flex-wrap:wrap;gap:6px;margin:0">
        <button class="btn" data-oa="sync" type="button">Copy now</button><button class="btn" data-oa="list" type="button">Stored</button>
        <button class="btn" data-oa="test" type="button">Test</button><button class="btn" data-oa="edit" type="button">Edit</button>
        <button class="btn danger" data-oa="del" type="button">Remove</button></div></td></tr>`).join("")
    : `<tr><td class="muted">No off-site place yet.</td></tr>`;
}

$("#oAdd").onclick = () => oEdit(null);
$("#oCancel").onclick = () => { $("#oForm").hidden = true; };
function oEdit(t){
  oEditing = t;
  $("#oForm").hidden = false; $("#oForm").reset();
  $("#oName").disabled = !!t;
  const v = t || {type: "dir", keep: 7, enabled: true};
  $("#oName").value = v.name || ""; $("#oType").value = v.type; $("#oPath").value = v.path || "";
  $("#oEndpoint").value = v.endpoint || ""; $("#oRegion").value = v.region || ""; $("#oBucket").value = v.bucket || "";
  $("#oPrefix").value = v.prefix || ""; $("#oAK").value = v.access_key || ""; $("#oVH").checked = !!v.virtual_host;
  $("#oSK").placeholder = t?.has_secret ? "unchanged" : "";
  $("#oHost").value = v.host || ""; $("#oUser").value = v.user || ""; $("#oPort").value = v.port || ""; $("#oRDir").value = v.remote_dir || "";
  $("#oKeep").value = v.keep || 7; $("#oOn").checked = v.enabled !== false;
  $("#oPass").placeholder = t?.has_passphrase ? "unchanged" : "12+ characters, optional";
  oShowType();
  $("#oName").focus();
}

$("#oForm").addEventListener("submit", async e => {
  e.preventDefault();
  const name = oEditing?.name || $("#oName").value.trim();
  const body = {type: $("#oType").value, enabled: $("#oOn").checked, keep: +$("#oKeep").value || 7, passphrase: $("#oPass").value,
    path: $("#oPath").value.trim(), endpoint: $("#oEndpoint").value.trim(), region: $("#oRegion").value.trim(), bucket: $("#oBucket").value.trim(),
    prefix: $("#oPrefix").value.trim(), access_key: $("#oAK").value.trim(), secret_key: $("#oSK").value, virtual_host: $("#oVH").checked,
    host: $("#oHost").value.trim(), user: $("#oUser").value.trim(), port: +$("#oPort").value || 0, remote_dir: $("#oRDir").value.trim()};
  const go = $("#oSave"); go.disabled = true;
  try {
    await call("PUT", `/offsite/targets/${encodeURIComponent(name)}`, body);
    $("#oForm").hidden = true;
    try { await call("POST", `/offsite/targets/${encodeURIComponent(name)}/test`); toast(`${name}: saved, test passed`); }
    catch (err) { toast(`${name} saved, but the test failed: ${err.message}`, true); }
    loadOffsite();
  } catch (err) { toast(err.message, true); }
  go.disabled = false;
});

$("#oTargets").addEventListener("click", async e => {
  const b = e.target.closest("[data-oa]"); if (!b) return;
  const name = b.closest("tr").dataset.o, t = oTargets.find(x => x.name === name), path = `/offsite/targets/${encodeURIComponent(name)}`;
  try {
    switch (b.dataset.oa) {
    case "edit": return oEdit(t);
    case "test":
      b.disabled = true;
      try { await call("POST", path + "/test"); toast(`${name}: test passed (write, read, delete)`); }
      finally { b.disabled = false; }
      return;
    case "sync": return trackJob(await call("POST", path + "/sync"), loadOffsite);
    case "del":
      if (!armed(b, "Forget?")) return;
      await call("DELETE", path); toast(`${name} removed (copies stored there stay)`); return loadOffsite();
    case "list": return oStored(name);
    }
  } catch (err) { toast(err.message, true); }
});

let oStoredName = "";
async function oStored(name){
  oStoredName = name;
  $("#oStoredBox").hidden = false;
  $("#oStoredTitle").textContent = "Stored at " + name;
  $("#oStored").innerHTML = `<tr><td class="muted">Loading…</td></tr>`;
  try {
    const l = (await call("GET", `/offsite/targets/${encodeURIComponent(name)}/backups`)).backups;
    $("#oStored").innerHTML = l.length ? l.map(r => `<tr data-k="${esc(r.kind)}" data-g="${esc(r.guest)}" data-i="${esc(r.id)}">
      <td><b>${esc(r.guest)}</b> <span class="sub">${r.kind === "ct" ? "container" : "VM"}</span>
        <div class="sub">${esc(r.created_at ? fmtTime(r.created_at) : r.id)} · ${fmtBytes(r.bytes)}${r.encrypted ? " · encrypted" : ""}${r.owner ? " · " + esc(r.owner) : ""}</div></td>
      <td style="text-align:right">${r.local ? '<span class="sub">also on this server</span>' : '<button class="btn" data-fetch type="button">Bring back</button>'}</td></tr>`).join("")
      : `<tr><td class="muted">Nothing stored there yet.</td></tr>`;
  } catch (err) { $("#oStored").innerHTML = `<tr><td class="sub">${esc(err.message)}</td></tr>`; }
}
$("#oStored").addEventListener("click", async e => {
  if (!e.target.closest("[data-fetch]")) return;
  const tr = e.target.closest("tr");
  try {
    trackJob(await call("POST", `/offsite/targets/${encodeURIComponent(oStoredName)}/fetch`, {kind: tr.dataset.k, guest: tr.dataset.g, id: tr.dataset.i}),
      () => { if ($("#dlgBackups").open) { loadAllBackups(); loadCTAllBackups(); oStored(oStoredName); } });
  } catch (err) { toast(err.message, true); }
});
$("#oKey").onclick = () => { navigator.clipboard?.writeText($("#oKey").textContent).then(() => toast("Key copied")); };

/* container backups of every container (also deleted ones) */
async function loadCTAllBackups(){
  let l;
  try { l = (await call("GET", "/ct-backups")).backups; } catch { $("#gCTBox").hidden = true; return; }
  $("#gCTBox").hidden = !l.length;
  $("#gCTBackups").innerHTML = l.map(b => `<tr data-vm="${esc(b.vm)}" data-id="${esc(b.id)}">
    <td><b>${esc(fmtTime(b.created_at))}</b><div class="sub">${esc(b.vm)} · ${fmtBytes(b.bytes || 0)}${b.public_ip ? " · " + esc(b.public_ip) : ""}</div></td>
    <td style="text-align:right"><button class="btn" data-cr type="button">Restore…</button><button class="btn danger" data-cd type="button">Delete</button></td></tr>`).join("");
}
$("#gCTBackups").addEventListener("click", async e => {
  const tr = e.target.closest("tr[data-id]"); if (!tr) return;
  const path = `/ct-backups/${encodeURIComponent(tr.dataset.vm)}/${encodeURIComponent(tr.dataset.id)}`;
  try {
    if (e.target.closest("[data-cr]")) {
      const taken = (typeof cts !== "undefined" ? cts : []).some(c => c.name === tr.dataset.vm);
      const n = prompt("Name for the restored container:", taken ? tr.dataset.vm + "-restored" : tr.dataset.vm);
      if (!n) return;
      trackJob(await call("POST", path + "/restore", {name: n.trim()}), loadCTAllBackups);
    } else if (e.target.closest("[data-cd]")) {
      if (!armed(e.target.closest("[data-cd]"), "Delete — sure?")) return;
      await call("DELETE", path); loadCTAllBackups();
    }
  } catch (err) { toast(err.message, true); }
});

// open: also load these sections
const _openAllBackups = openAllBackups;
openAllBackups = async function(){
  $("#oForm").hidden = true; $("#oStoredBox").hidden = true;
  loadOffsite(); loadCTAllBackups();
  return _openAllBackups();
};
