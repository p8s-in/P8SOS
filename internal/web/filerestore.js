"use strict";
/* ---------- file-level restore: Backups → Files ---------- */
// Uses the dashboard's globals: $, esc, call, toast, token, API, fmtBytes, fmtTime.

document.body.insertAdjacentHTML("beforeend", `<dialog id="dlgFiles" class="wide"><div class="dlg-body">
  <h2 style="margin:0">Files in backup <span class="sub" id="frTitle"></span></h2>
  <div class="sub" id="frState"></div>
  <nav class="tabs" id="frParts"></nav>
  <div class="inline" style="margin:6px 0"><span class="mono" id="frPath" style="flex:1;overflow-wrap:anywhere"></span>
    <button class="btn" id="frUp" type="button">↑ Up</button></div>
  <div style="max-height:55vh;overflow:auto"><table class="snaps"><tbody id="frList"></tbody></table></div>
  <div class="hint">Read-only: nothing in the backup changes. Download a file, then put it back in the VM (e.g. with scp). The backup is closed after an hour.</div>
  <div class="dlg-actions"><button class="btn" id="frClose" type="button">Close</button></div>
</div></dialog>`);

let fr = null, frPart = "", frDir = "/", frPoll = 0;

async function openFiles(vm, id){
  $("#frTitle").textContent = `${id} of ${vm}`;
  $("#frParts").innerHTML = ""; $("#frList").innerHTML = ""; $("#frPath").textContent = "";
  $("#frState").textContent = "Opening the backup… (it is restored into a scratch disk; large disks take a few minutes)";
  $("#dlgFiles").showModal();
  try { fr = await call("POST", `/backups/${encodeURIComponent(vm)}/${encodeURIComponent(id)}/browse`); }
  catch (e) { $("#frState").textContent = "✘ " + e.message; return; }
  frWait();
}
async function frWait(){
  clearTimeout(frPoll);
  if (!fr || !$("#dlgFiles").open) return;
  try { fr = await call("GET", `/browse/${fr.id}`); } catch (e) { $("#frState").textContent = "✘ " + e.message; return; }
  if (fr.state === "preparing") { frPoll = setTimeout(frWait, 2000); return; }
  if (fr.state === "failed") { $("#frState").textContent = "✘ " + fr.error; return; }
  const ok = fr.parts.filter(p => !p.error);
  $("#frState").innerHTML = ok.length ? `${fr.parts.length} filesystem(s) found.` + fr.parts.filter(p => p.error).map(p => `<div class="sub">${esc(p.id)} (${esc(p.fs)}): ${esc(p.error)}</div>`).join("")
    : "No filesystem could be opened." + fr.parts.map(p => `<div class="sub">${esc(p.id)} (${esc(p.fs)}): ${esc(p.error)}</div>`).join("");
  $("#frParts").innerHTML = ok.map(p => `<button type="button" data-frp="${esc(p.id)}">${esc(p.label || p.fs)} <span class="sub">${esc(p.size)}</span></button>`).join("");
  if (ok.length) frOpen(ok.find(p => !/efi|boot/i.test(p.label || ""))?.id || ok[0].id, "/");
}
async function frOpen(part, dir){
  frPart = part; frDir = dir;
  for (const b of document.querySelectorAll("#frParts [data-frp]")) b.classList.toggle("on", b.dataset.frp === part);
  $("#frPath").textContent = dir;
  $("#frUp").disabled = dir === "/";
  let l;
  try { l = (await call("GET", `/browse/${fr.id}/ls?part=${encodeURIComponent(part)}&path=${encodeURIComponent(dir)}`)).entries; }
  catch (e) { $("#frList").innerHTML = `<tr><td class="sub">✘ ${esc(e.message)}</td></tr>`; return; }
  $("#frList").innerHTML = l.length ? l.map(e => `<tr data-frn="${esc(e.name)}" data-frd="${e.dir ? 1 : ""}">
      <td>${e.dir ? "📁" : e.link ? "↪" : "📄"} ${e.dir ? `<a href="#" data-fropen>${esc(e.name)}</a>` : esc(e.name)}</td>
      <td class="sub" style="white-space:nowrap">${e.dir ? "" : fmtBytes(e.size)}</td><td class="sub" style="white-space:nowrap">${esc(fmtTime(e.mod_time))}</td>
      <td style="text-align:right">${!e.dir && !e.link ? `<button class="btn" data-frget type="button">Download</button>` : ""}</td></tr>`).join("")
    : `<tr><td class="sub">Empty folder.</td></tr>`;
}
const frJoin = (d, n) => (d.endsWith("/") ? d : d + "/") + n;
$("#frParts").addEventListener("click", e => { const b = e.target.closest("[data-frp]"); if (b) frOpen(b.dataset.frp, "/"); });
$("#frUp").onclick = () => { const p = frDir.replace(/\/[^/]*\/?$/, "") || "/"; frOpen(frPart, p); };
$("#frList").addEventListener("click", async e => {
  const row = e.target.closest("tr[data-frn]"); if (!row) return;
  if (e.target.closest("[data-fropen]")) { e.preventDefault(); return frOpen(frPart, frJoin(frDir, row.dataset.frn)); }
  if (!e.target.closest("[data-frget]")) return;
  const name = row.dataset.frn, b = e.target.closest("button"); b.disabled = true;
  try { // with the sign-in token, so not a plain link
    const r = await fetch(`${API}/browse/${fr.id}/file?part=${encodeURIComponent(frPart)}&path=${encodeURIComponent(frJoin(frDir, name))}`, {headers: {"Authorization": "Bearer " + token}});
    if (!r.ok) throw new Error((await r.json().catch(() => ({})))?.error?.message || "HTTP " + r.status);
    const url = URL.createObjectURL(await r.blob()), a = document.createElement("a");
    a.href = url; a.download = name; document.body.appendChild(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 60000);
  } catch (err) { toast(err.message, true); }
  b.disabled = false;
});
$("#frClose").onclick = async () => {
  clearTimeout(frPoll);
  $("#dlgFiles").close();
  if (fr?.id) { try { await call("DELETE", `/browse/${fr.id}`); } catch {} }
  fr = null;
};
