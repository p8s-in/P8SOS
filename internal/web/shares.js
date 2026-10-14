"use strict";
/* ---------- per-VM permissions: Manage → Options → Shared with ---------- */
// Uses the dashboard's globals: $, esc, call, toast, armed, mName, me, vms.

document.querySelector('#dlgManage [data-tab="options"]').insertAdjacentHTML("beforeend", `<div id="mShBox" hidden>
  <h3>Shared with</h3>
  <div class="sub">Let another person use this VM without giving it away. <b>User</b>: start, stop, console, password, snapshots, backups.
    <b>Viewer</b>: look only. Deleting, resizing and hardware stay with the owner (and count against the owner's plan).</div>
  <table class="snaps"><tbody id="mShares"></tbody></table>
  <div class="inline"><input id="mShUser" placeholder="user name" autocomplete="off" style="flex:1 1 200px" list="mShUsers"><datalist id="mShUsers"></datalist>
    <select id="mShLevel" style="flex:0 0 130px"><option value="user">User</option><option value="viewer">Viewer</option></select>
    <button class="btn" id="mShGo">Share</button></div>
</div>`);
document.querySelector('#dlgManage [data-tab="summary"]').insertAdjacentHTML("afterbegin", `<div id="mShared" class="hint" hidden></div>`);

let shFor = "";
async function loadShares(v){
  const mine = me?.role === "admin" || me?.role === "operator" || (v.owner && v.owner === me?.name);
  $("#mShBox").hidden = !mine;
  $("#mShared").hidden = !v.access;
  if (v.access) $("#mShared").innerHTML = `${esc(v.owner || "An admin")} shares this VM with you as <b>${esc(v.access)}</b>: ${v.access === "viewer" ? "you can look but not change it." : "you can run it; deleting, resizing and hardware stay with the owner."}`;
  if (!mine || shFor === v.name) return;
  shFor = v.name;
  renderShares((await call("GET", `/vms/${encodeURIComponent(v.name)}/shares`).catch(() => ({shares: []}))).shares);
  if (me?.role === "admin" || me?.role === "operator")
    call("GET", "/users").then(r => { $("#mShUsers").innerHTML = (r.users || []).filter(u => u.role === "customer").map(u => `<option value="${esc(u.name)}">`).join(""); }).catch(() => {});
}
function renderShares(l){
  $("#mShares").innerHTML = l.length ? l.map(s => `<tr data-shu="${esc(s.user)}"><td><b>${esc(s.user)}</b></td><td>${esc(s.level)}</td>
    <td style="text-align:right"><button class="btn danger" data-shrm>Stop sharing</button></td></tr>`).join("") : `<tr><td class="sub">Not shared with anyone.</td></tr>`;
}
$("#mShGo").onclick = async () => {
  const user = $("#mShUser").value.trim();
  if (!user) return toast("Type the user's name", true);
  try { renderShares((await call("PUT", `/vms/${encodeURIComponent(mName)}/shares`, {user, level: $("#mShLevel").value})).shares); $("#mShUser").value = ""; toast(`shared with ${user}`); }
  catch (e) { toast(e.message, true); }
};
$("#mShares").addEventListener("click", async e => {
  const b = e.target.closest("[data-shrm]"); if (!b) return;
  if (!armed(b, "Sure?")) return;
  const user = b.closest("tr").dataset.shu;
  try { renderShares((await call("PUT", `/vms/${encodeURIComponent(mName)}/shares`, {user, level: ""})).shares); toast(`${user} can't use it any more`); }
  catch (err) { toast(err.message, true); }
});
