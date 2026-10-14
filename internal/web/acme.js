"use strict";
/* ---------- Server page → Network: HTTPS certificate (Let's Encrypt) ---------- */
// Uses the dashboard's globals: $, esc, call, toast, armed; node.js's showTask, loadNode.

$("#raBox").insertAdjacentHTML("afterend", `<div id="acBox">
  <h3>HTTPS certificate</h3>
  <div class="sub" id="acSub"></div>
  <div id="acForm">
    <div class="row">
      <div><label for="acDomain">Domain of this server</label><input id="acDomain" placeholder="p8s.example.com" autocomplete="off"></div>
      <div><label for="acEmail">Email for expiry warnings (optional)</label><input id="acEmail" type="email" autocomplete="off"></div>
    </div>
    <label for="acMethod">How Let's Encrypt checks the domain is yours</label>
    <select id="acMethod"><option value="http">Port 80 — forward TCP 80 from your router to this server</option>
      <option value="cloudflare">Cloudflare DNS — no ports; the domain's DNS is at Cloudflare (works behind CGNAT)</option></select>
    <div id="acCFBox" hidden><label for="acCF">Cloudflare API token (Zone → DNS → Edit, for this domain)</label><input id="acCF" type="password" autocomplete="off"></div>
    <div class="inline"><span class="hint" style="flex:1">The domain must point to this server (an A record). The certificate renews by itself every 2 months.
      Cluster servers keep talking to each other with the server's own certificate.</span><button class="btn primary" id="acGo">Get certificate</button></div>
  </div>
</div>`);
$("#acMethod").onchange = () => { $("#acCFBox").hidden = $("#acMethod").value !== "cloudflare"; };

async function loadACME(){
  let a;
  try { a = await call("GET", "/node/acme"); } catch { $("#acBox").hidden = true; return; }
  $("#acBox").hidden = false;
  const c = a.cert;
  if (c) {
    const days = Math.round((new Date(c.not_after) - Date.now()) / 864e5);
    $("#acSub").innerHTML = `✔ <b>${esc(c.names.join(", "))}</b> has a certificate from ${esc(c.issuer)}, valid ${days} more days (renews by itself).
      Open the dashboard at <a href="https://${esc(c.names[0])}:${esc(location.port || "443")}/">https://${esc(c.names[0])}:${esc(location.port || "443")}</a> — no browser warning.
      <button class="btn danger" id="acRm" type="button">Remove</button>`;
    $("#acRm").onclick = async e => {
      if (!armed(e.target, "Back to the own certificate?")) return;
      try { const t = await call("DELETE", "/node/acme"); toast(t.title + " started"); showTask?.(t.id); } catch (err) { toast(err.message, true); }
    };
  } else {
    $("#acSub").textContent = "The dashboard uses the server's own certificate, so browsers warn once. With a domain, get a free Let's Encrypt certificate instead.";
  }
  $("#acForm").hidden = !!c;
  if (a.request && !$("#acDomain").value) { $("#acDomain").value = a.request.domain; $("#acEmail").value = a.request.email || ""; $("#acMethod").value = a.request.method; $("#acMethod").onchange(); }
}
$("#acGo").onclick = async () => {
  const body = {domain: $("#acDomain").value.trim(), email: $("#acEmail").value.trim(), method: $("#acMethod").value};
  if ($("#acCF").value.trim()) body.cf_token = $("#acCF").value.trim();
  try { const t = await call("POST", "/node/acme", body); $("#acCF").value = ""; toast(t.title + " started"); showTask?.(t.id); }
  catch (e) { toast(e.message, true); }
};
if (typeof openNode === "function") {
  const _openNodeAC = openNode;
  openNode = async function(){ loadACME(); return _openNodeAC(); };
}
