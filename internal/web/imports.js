// Import VMs (admins): Proxmox backups (vzdump .vma[.zst|.lzo|.gz]) and disk
// images (qcow2, vmdk, vhd/vhdx, vdi, raw) from other hypervisors.
// Uses the dashboard helpers ($, esc, call, toast, trackJob, fmtBytes, API, token).
"use strict";

document.body.insertAdjacentHTML("beforeend", `
<dialog id="dlgImp" class="wide"><div class="dlg-body">
  <h2>Import VMs</h2>
  <div class="sub">Bring virtual machines over from <b>Proxmox VE</b> (backup files made with <i>Backup now</i> / vzdump) or from
    <b>VMware, Hyper-V, VirtualBox</b> or anywhere else (a disk image). The original is not changed.</div>
  <h3>1. Put the file on this server</h3>
  <div class="inline"><input id="impFile" type="file" accept=".vma,.zst,.lzo,.gz,.qcow2,.vmdk,.vhd,.vhdx,.vdi,.img,.raw"><button class="btn primary" id="impUp">Upload</button></div>
  <div class="meter" id="impBar" hidden><i></i></div><div class="hint" id="impUpTxt"></div>
  <div class="hint" id="impScp"></div>
  <h3>2. Pick a file</h3>
  <table class="snaps"><tbody id="impList"></tbody></table>
  <div id="impPlanBox" hidden>
    <h3>3. Check the settings <span class="sub" style="font-weight:400" id="impPlanFile"></span></h3>
    <div id="impWarn"></div>
    <div class="row" style="grid-template-columns:1fr 1fr 1fr">
      <div><label for="impName">Name</label><input id="impName" autocomplete="off"></div>
      <div><label for="impCPU">CPUs</label><input id="impCPU" type="number" min="1"></div>
      <div><label for="impMem">RAM (MB)</label><input id="impMem" type="number" min="256" step="256"></div>
      <div><label for="impOS">Operating system</label><select id="impOS"><option value="linux">Linux / other</option><option value="windows">Windows (10, Server)</option><option value="windows11">Windows 11 (Secure Boot + TPM)</option></select></div>
      <div><label for="impFw">Firmware</label><select id="impFw"><option value="bios">BIOS</option><option value="uefi">UEFI</option></select></div>
      <div><label for="impDisk">Disk (GB)</label><input id="impDisk" type="number" min="1"></div>
      <div><label for="impBus">Disk connection</label><select id="impBus"><option value="virtio">virtio (fastest)</option><option value="scsi">virtio-scsi</option><option value="sata">SATA</option></select></div>
      <div><label for="impNic">Network card</label><select id="impNic"><option value="virtio">virtio (fastest)</option><option value="e1000e">Intel e1000e</option><option value="e1000">Intel e1000</option><option value="vmxnet3">VMware vmxnet3</option><option value="rtl8139">Realtek 8139</option></select></div>
      <div><label for="impNet">Network</label><select id="impNet"><option value="nat">Private (NAT)</option><option value="lan">Home network</option></select></div>
    </div>
    <label style="display:flex;gap:8px;align-items:center;font-weight:400;margin-top:8px"><input id="impMac" type="checkbox" style="width:auto"> Keep the old network card address (MAC) — only if the original VM is switched off for good</label>
    <label style="display:flex;gap:8px;align-items:center;font-weight:400"><input id="impStart" type="checkbox" style="width:auto" checked> Start it when the import is done</label>
    <div class="hint">Keep the disk connection and firmware the VM was installed with, or it may not boot. Windows especially: change them only after installing the virtio drivers.</div>
    <details style="margin-top:6px" id="impConfBox" hidden><summary class="sub">Proxmox settings in the backup</summary><pre class="mono" id="impConf" style="white-space:pre-wrap;font-size:12px;max-height:220px;overflow:auto"></pre></details>
    <div class="inline" style="margin-top:8px"><span class="spacer"></span><button class="btn primary" id="impGo">Import</button></div>
  </div>
  <div class="dlg-actions"><button class="btn" id="impClose">Close</button></div>
</div></dialog>`);

let impPlan = null;
const impKind = {"proxmox-vma": "Proxmox backup", "disk-image": "disk image"};

async function openImports(){
  impPlan = null; $("#impPlanBox").hidden = true;
  $("#dlgImp").showModal();
  await loadImports();
}

async function loadImports(){
  let r;
  try { r = await call("GET", "/imports"); } catch (e) { $("#impList").innerHTML = `<tr><td class="sub">${esc(e.message)}</td></tr>`; return; }
  const host = location.hostname;
  $("#impScp").innerHTML = r.folder ? `Big files are faster with <b>scp</b> from the old server, e.g.
    <span class="mono copy" data-copy="scp /var/lib/vz/dump/vzdump-qemu-100-*.vma.zst ${esc("<user>@" + host + ":" + r.folder + "/")}">scp /var/lib/vz/dump/vzdump-qemu-100-….vma.zst &lt;user&gt;@${esc(host)}:${esc(r.folder)}/</span> — then press Refresh.` : "";
  const files = r.files || [];
  $("#impList").innerHTML = (files.length ? files.map(f => `<tr data-f="${esc(f.name)}"><td><b class="mono">${esc(f.name)}</b>
      <div class="sub">${esc(impKind[f.kind] || f.kind)} · ${fmtBytes(f.bytes)}</div></td>
      <td style="text-align:right;white-space:nowrap"><button class="btn primary" data-a="plan">Check</button> <button class="btn danger" data-a="del">Delete</button></td></tr>`).join("")
    : `<tr><td class="sub">No files yet. Upload one above or copy it into the import folder.</td></tr>`) +
    `<tr><td colspan="2" style="text-align:right"><button class="btn" id="impRefresh">Refresh</button></td></tr>`;
  $("#impRefresh").onclick = loadImports;
}

$("#impList").addEventListener("click", async e => {
  const b = e.target.closest("button[data-a]"); if (!b) return;
  const f = b.closest("tr").dataset.f;
  if (b.dataset.a === "del") {
    if (!confirm(`Delete ${f} from the import folder? (VMs already imported from it stay.)`)) return;
    try { await call("DELETE", "/imports/" + encodeURIComponent(f)); toast(`${f} deleted`); if (impPlan?.file === f) $("#impPlanBox").hidden = true; loadImports(); }
    catch (err) { toast(err.message, true); }
    return;
  }
  b.disabled = true; b.textContent = "Reading…";
  try { showImportPlan(await call("GET", `/imports/${encodeURIComponent(f)}/plan`)); }
  catch (err) { toast(err.message, true); }
  b.disabled = false; b.textContent = "Check";
});

function showImportPlan(p){
  impPlan = p;
  $("#impPlanBox").hidden = false;
  $("#impPlanFile").textContent = "· " + p.file + (p.source_info ? " · " + p.source_info : "");
  $("#impName").value = p.name; $("#impCPU").value = p.vcpus; $("#impMem").value = p.memory_mib; $("#impDisk").value = p.disk_gb;
  $("#impDisk").min = p.disk_gb;
  $("#impOS").value = p.os_type; $("#impFw").value = p.firmware; $("#impBus").value = p.disk_bus; $("#impNic").value = p.nic_model;
  $("#impMac").checked = false; $("#impMac").disabled = !p.mac;
  $("#impMac").parentElement.title = p.mac ? "Old address: " + p.mac : "The file has no network card address";
  $("#impWarn").innerHTML = (p.warnings || []).map(w => `<div class="toast" style="max-width:none;box-shadow:none;margin:6px 0;border-left-color:var(--warn)">${esc(w)}</div>`).join("");
  $("#impConfBox").hidden = !p.config; $("#impConf").textContent = p.config || "";
  $("#impPlanBox").scrollIntoView({behavior: "smooth", block: "start"});
}

$("#impGo").onclick = async () => {
  if (!impPlan) return;
  const body = {name: $("#impName").value.trim(), vcpus: +$("#impCPU").value, memory_mib: +$("#impMem").value, disk_gb: +$("#impDisk").value,
    os_type: $("#impOS").value, firmware: $("#impFw").value, disk_bus: $("#impBus").value, nic_model: $("#impNic").value,
    network: $("#impNet").value, keep_mac: $("#impMac").checked, start: $("#impStart").checked};
  if ($("#impOS").value === "windows11" && $("#impFw").value !== "uefi") return toast("Windows 11 needs UEFI firmware", true);
  $("#impGo").disabled = true;
  try {
    const j = await call("POST", `/imports/${encodeURIComponent(impPlan.file)}/start`, body);
    $("#dlgImp").close();
    trackJob(j);
  } catch (err) { toast(err.message, true); }
  $("#impGo").disabled = false;
};

$("#impUp").onclick = () => {
  const f = $("#impFile").files[0];
  if (!f) return toast("Choose a file first", true);
  const name = f.name.replace(/[^A-Za-z0-9._+ -]/g, "-");
  if (!/\.(vma(\.(zst|lzo|gz))?|qcow2|vmdk|vhdx?|vdi|img|raw)$/i.test(name)) return toast("Use a Proxmox backup (.vma, .vma.zst, .vma.lzo, .vma.gz) or a disk image (.qcow2, .vmdk, .vhd, .vhdx, .vdi, .img, .raw)", true);
  // XMLHttpRequest, because fetch() can't report upload progress.
  const x = new XMLHttpRequest(), bar = $("#impBar"), txt = $("#impUpTxt"), up = $("#impUp");
  x.open("PUT", API + "/imports/" + encodeURIComponent(name));
  x.setRequestHeader("Authorization", "Bearer " + token);
  x.upload.onprogress = ev => { if (ev.lengthComputable) {
    bar.firstElementChild.style.width = (ev.loaded / ev.total * 100).toFixed(1) + "%";
    txt.textContent = `Uploading ${name}: ${fmtBytes(ev.loaded)} of ${fmtBytes(ev.total)}`; } };
  x.onload = () => {
    up.disabled = false; bar.hidden = true;
    if (x.status === 201) { txt.textContent = ""; toast(`${name} uploaded`); loadImports(); }
    else { let m = "upload failed"; try { m = JSON.parse(x.responseText).error.message; } catch {} txt.textContent = m; toast(m, true); }
  };
  x.onerror = () => { up.disabled = false; bar.hidden = true; toast("Upload failed (connection lost)", true); };
  bar.hidden = false; bar.firstElementChild.style.width = "0"; up.disabled = true;
  x.send(f);
};
$("#impClose").onclick = () => $("#dlgImp").close();
if ($("#bImports")) $("#bImports").onclick = openImports;
