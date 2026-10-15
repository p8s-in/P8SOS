"use strict";
/* Security settings (admins): allowed networks, password policy, 2FA for staff,
   company directory (LDAP / Active Directory); storage encryption on the
   Server page. Uses index.html's $, call, esc, toast, armed, isFullAdmin, me, openAccount. */

/* ---------- Settings → Security ---------- */
$("#ssoBox").parentElement.querySelector("#tClose").closest(".dlg-actions").insertAdjacentHTML("beforebegin", `
  <h3>Security</h3>
  <div id="secBox">
    <label for="secNets">Who may open this dashboard (addresses or networks, one per line; empty = everyone)</label>
    <textarea id="secNets" rows="3" placeholder="192.168.0.0/24&#10;203.0.113.7"></textarea>
    <div class="hint" id="secMe"></div>
    <div class="row" style="grid-template-columns:1fr 1fr">
      <div><label for="secLen">Shortest password</label><input id="secLen" type="number" min="10" max="64"></div>
      <div><label style="visibility:hidden">x</label><label class="check" style="margin:0"><input id="secMix" type="checkbox"> Passwords need letters and digits</label></div>
    </div>
    <label class="check"><input id="sec2fa" type="checkbox"> Admins, operators and auditors must use two-factor sign-in</label>
    <div class="hint">Company-login accounts use the provider's two-factor; directory accounts set it up here.</div>
    <div class="dlg-actions"><button class="btn primary" id="secSave">Save</button></div>
  </div>
  <h3>Company directory (LDAP / Active Directory)</h3>
  <div class="sub">People sign in with their Windows / directory user name and password. Their groups decide the role;
    a P8S account is made the first time. Local accounts keep signing in as before.</div>
  <details class="uedit" id="ldBox"><summary><b id="ldState">Set up</b></summary>
    <div class="row" style="grid-template-columns:1fr 1fr">
      <div><label for="ldURL">Server</label><input id="ldURL" placeholder="ldaps://dc1.corp.example.com" autocomplete="off"></div>
      <div><label for="ldBase">Base DN</label><input id="ldBase" placeholder="dc=corp,dc=example,dc=com" autocomplete="off"></div>
      <div><label for="ldBind">Service account (bind DN)</label><input id="ldBind" placeholder="cn=p8s,ou=service,dc=corp,dc=example,dc=com" autocomplete="off"></div>
      <div><label for="ldPw">Its password</label><input id="ldPw" type="password" autocomplete="off"></div>
      <div><label for="ldAdmins">Admin groups</label><input id="ldAdmins" placeholder="P8S Admins" autocomplete="off"></div>
      <div><label for="ldOps">Operator groups</label><input id="ldOps" autocomplete="off"></div>
      <div><label for="ldAud">Auditor groups</label><input id="ldAud" autocomplete="off"></div>
      <div><label for="ldCust">Customer groups</label><input id="ldCust" autocomplete="off"></div>
      <div><label for="ldDefault">People in none of these groups</label><select id="ldDefault"><option value="">can't sign in</option><option value="customer">become customers</option></select></div>
      <div><label for="ldStart">ldap:// with StartTLS</label><select id="ldStart"><option value="0">no (ldaps://)</option><option value="1">yes</option></select></div>
    </div>
    <details style="margin-top:6px"><summary class="sub">More: filters, certificate</summary>
      <label for="ldUF">User filter <span class="muted">({user} = what was typed)</span></label><input id="ldUF" class="mono" autocomplete="off">
      <label for="ldGF">Group filter <span class="muted">(optional; {dn} = the person; for directories without memberOf)</span></label>
      <input id="ldGF" class="mono" placeholder="(&(objectClass=groupOfNames)(member={dn}))" autocomplete="off">
      <label for="ldCA">Directory CA certificate (PEM; empty = the system's)</label><textarea id="ldCA" rows="3" class="mono"></textarea>
      <label class="check"><input id="ldSkip" type="checkbox"> Don't check the certificate (testing only)</label>
    </details>
    <label class="check"><input id="ldOn" type="checkbox"> Sign people in with the directory</label>
    <div class="dlg-actions"><span class="sub" id="ldTestOut" style="flex:1"></span><button class="btn" id="ldTest">Test</button><button class="btn primary" id="ldSave">Save</button></div>
  </details>`);

const secList = v => (v || []).join(", "), secSplit = id => $(id).value.split(/[,\n]+/).map(x => x.trim()).filter(Boolean);
async function loadSecurity(){
  let r; try { r = await call("GET", "/security"); } catch { return; }
  const s = r.settings;
  $("#secNets").value = (s.allow_nets || []).join("\n");
  $("#secMe").innerHTML = `You are connecting from <b class="mono">${esc(r.your_ip)}</b>. This server itself and the cluster tunnel always may.
    Locked out? On the server: <span class="mono">sudo rm /var/lib/p8s/security.json && sudo systemctl restart p8sd</span>`;
  $("#secLen").value = s.password?.min_length || 10; $("#secMix").checked = !!s.password?.mix; $("#sec2fa").checked = !!s.staff_2fa;
  let l; try { l = await call("GET", "/ldap"); } catch { $("#ldBox").hidden = true; return; }
  const d = l.settings;
  $("#ldState").textContent = d.enabled ? "On: " + d.url : "Set up";
  $("#ldURL").value = d.url || ""; $("#ldBase").value = d.base_dn || ""; $("#ldBind").value = d.bind_dn || "";
  $("#ldPw").value = ""; $("#ldPw").placeholder = l.has_password ? "unchanged" : "";
  $("#ldAdmins").value = secList(d.admin_groups); $("#ldOps").value = secList(d.operator_groups); $("#ldAud").value = secList(d.auditor_groups);
  $("#ldCust").value = secList(d.customer_groups); $("#ldDefault").value = d.default_role || ""; $("#ldStart").value = d.start_tls ? "1" : "0";
  $("#ldUF").value = d.user_filter || ""; $("#ldUF").placeholder = l.default_user_filter; $("#ldGF").value = d.group_filter || "";
  $("#ldCA").value = d.ca_cert || ""; $("#ldSkip").checked = !!d.skip_verify; $("#ldOn").checked = !!d.enabled; $("#ldTestOut").textContent = "";
}
const ldBody = () => ({enabled: $("#ldOn").checked, url: $("#ldURL").value.trim(), base_dn: $("#ldBase").value.trim(), bind_dn: $("#ldBind").value.trim(),
  bind_password: $("#ldPw").value, admin_groups: secSplit("#ldAdmins"), operator_groups: secSplit("#ldOps"), auditor_groups: secSplit("#ldAud"),
  customer_groups: secSplit("#ldCust"), default_role: $("#ldDefault").value, start_tls: $("#ldStart").value === "1",
  user_filter: $("#ldUF").value.trim(), group_filter: $("#ldGF").value.trim(), ca_cert: $("#ldCA").value.trim(), skip_verify: $("#ldSkip").checked});
$("#secSave").onclick = async () => {
  try {
    await call("PUT", "/security", {allow_nets: $("#secNets").value.split(/[\s,]+/).filter(Boolean), staff_2fa: $("#sec2fa").checked,
      password: {min_length: +$("#secLen").value, mix: $("#secMix").checked}});
    toast("Security settings saved"); loadSecurity();
  } catch (e) { toast(e.message, true); }
};
$("#ldTest").onclick = async () => {
  $("#ldTestOut").textContent = "Testing…";
  try { $("#ldTestOut").innerHTML = `<span style="color:var(--ok)">✔ ${esc((await call("POST", "/ldap/test", ldBody())).message)}</span>`; }
  catch (e) { $("#ldTestOut").innerHTML = `<span style="color:var(--bad)">✘ ${esc(e.message)}</span>`; }
};
$("#ldSave").onclick = async () => {
  try { await call("PUT", "/ldap", ldBody()); toast("Directory settings saved"); loadSecurity(); } catch (e) { toast(e.message, true); }
};
if (typeof openSettings === "function") {
  const _openSettingsSec = openSettings;
  openSettings = async function(){ loadSecurity(); return _openSettingsSec(); };
}

/* ---------- Server page → storage encryption ---------- */
$("#ndStorage").insertAdjacentHTML("afterend", `<div id="encBox" class="sub" style="margin-top:6px"></div>`);
async function loadEncryption(){
  let e; try { e = await call("GET", "/node/encryption"); } catch { return; }
  $("#encBox").innerHTML = e.on && e.passphrase
    ? `🔒 Everything is encrypted (${esc(e.algorithm)}): the system and the VM disks. The server asks for the passphrase at every start —
       on its screen, or over SSH once set up (<span class="mono">p8 boot-unlock add "ssh-ed25519 …"</span>). Change it: <span class="mono">sudo p8 disk-passphrase</span>.
       Without the passphrase nothing on the disks can be read.`
    : e.on
    ? `🔒 Encrypted at rest (${esc(e.algorithm)}, key ${esc(e.key_status)}). Keep a copy of the key somewhere safe:
       without it the VM disks can't be read. ${isFullAdmin() ? `<button class="btn" id="encKey">Show the key…</button>
       <span id="encAsk" hidden><input id="encPw" type="password" placeholder="your password" style="width:180px"> <input id="encOtp" placeholder="or 2FA code" style="width:110px"> <button class="btn" id="encGo">Show</button></span>
       <pre id="encOut" class="mono" hidden style="white-space:pre-wrap;word-break:break-all;font-size:12px;background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:8px"></pre>` : ""}`
    : `Not encrypted. Encryption at rest is chosen when the server is installed (boot menu "encrypted VM storage", or <span class="mono">install.sh --encrypt</span>).`;
  if ($("#encKey")) $("#encKey").onclick = () => { $("#encAsk").hidden = false; $("#encPw").focus(); };
  if ($("#encGo")) $("#encGo").onclick = async () => {
    try {
      const k = await call("POST", "/node/encryption-key", {password: $("#encPw").value, otp: $("#encOtp").value.trim()});
      $("#encPw").value = $("#encOtp").value = ""; $("#encAsk").hidden = true;
      $("#encOut").hidden = false;
      $("#encOut").textContent = `Key (hex): ${k.key_hex}\nDataset: ${k.dataset}\nTo put it back on a reinstalled server:\n  ${k.restore}`;
    } catch (err) { toast(err.message, true); }
  };
}
if (typeof openNode === "function") {
  const _openNodeEnc = openNode;
  openNode = async function(){ loadEncryption(); return _openNodeEnc(); };
}

/* ---------- 2FA required for staff ---------- */
async function check2FA(){
  if (me?.need_2fa && typeof openAccount === "function") {
    toast("This server requires two-factor sign-in for staff: set it up now", true);
    openAccount();
  }
}
