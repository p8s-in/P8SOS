"use strict";
/* Server page → Remote access (admins): reach this server from outside the
   home network. Uses index.html's $, call, esc, toast, armed, isFullAdmin and
   node.js's runTask / showTask. */

$("#haBox").insertAdjacentHTML("afterend", `<div id="raBox" hidden>
  <h3>Remote access <span class="sub" style="font-weight:400">— use this server from outside your home / office network</span></h3>
  <div id="raNet" class="sub"></div>
  <div id="raBody"></div>
</div>`);

let raConf = null;
async function loadRemote(refresh, force){
  const box = $("#raBox");
  if (box.contains(document.activeElement) && !refresh && !force) return;
  let st;
  try { st = await call("GET", "/remote" + (refresh ? "?refresh=1" : "")); } catch { box.hidden = true; return; }
  box.hidden = false;
  const r = st.router || {}, v = st.vpn, ts = st.tailscale || {}, admin = isFullAdmin();
  const cg = r.found ? (r.cgnat
      ? `<b style="color:var(--warn)">Your internet provider uses CGNAT</b> (the router's internet address is ${esc(r.external_ip)}): nothing on the internet can connect <i>in</i> to this server. Use <b>Tailscale</b> (or the Cloudflare Tunnel for websites).`
      : `Your router (${esc(r.name || "UPnP")}) has a real internet address${r.external_ip ? " (" + esc(r.external_ip) + ")" : ""}: every option below works.`)
    : `Couldn't ask the router (UPnP off). If its internet (WAN) address on its status page starts with 100.64–100.127 or 10., your provider uses CGNAT: use Tailscale.`;
  $("#raNet").innerHTML = `This network's public address: <b class="mono">${esc(st.public_ip || "unknown")}</b>. ${cg} <a href="#" id="raRe">Check again</a>`;
  const tsOn = ts.state === "Running";
  const devs = (v.devices || []).map(d => `<tr data-dev="${esc(d.id)}"><td><b>${esc(d.name)}</b><div class="sub">${esc(d.ip)} · ${d.last_seen ? "last connected " + esc(new Date(d.last_seen).toLocaleString()) : "never connected"}</div></td>
      <td style="text-align:right">${admin ? '<button class="btn danger" data-ra="del">Remove</button>' : ""}</td></tr>`).join("");
  $("#raBody").innerHTML = `
  <details class="uedit" ${tsOn ? "" : "open"}><summary><b>1. Tailscale</b> — easiest, works everywhere (also Jio / Airtel CGNAT) ${tsOn ? `<span style="color:var(--ok)">● connected ${esc((ts.ips || [])[0] || "")}</span>` : ""}</summary>
    <div class="sub">A free private network between your devices. Nothing to open on the router. Install Tailscale on your phone / laptop with the same account,
      then open <span class="mono">https://${esc((ts.ips || [])[0] || "<tailscale-ip>")}:8006</span>.</div>
    ${admin ? `<label class="check"><input id="raTsLan" type="checkbox"> Also reach my home network (VMs on it, the router…)</label>
    <div class="inline">${tsOn ? `<button class="btn" id="raTsOff">Disconnect</button>` : `<button class="btn primary" id="raTsOn">${ts.installed ? "Connect" : "Install and connect"}</button>`}
      <span class="sub">The task log shows a sign-in link; open it and sign in.</span></div>` : ""}
  </details>
  <details class="uedit" ${v.enabled ? "open" : ""}><summary><b>2. Built-in VPN (WireGuard)</b> — needs a real public address ${v.enabled ? `<span style="color:var(--ok)">● on, ${v.devices.length} device(s)</span>` : ""}</summary>
    <div class="sub">Your phone / laptop connects straight to this server. The router must forward <b>UDP ${v.port}</b> to it.
      Devices get the dashboard at <span class="mono">https://10.251.0.1:8006</span> and the VMs' SSH / RDP ports.</div>
    ${admin ? `<div class="row" style="grid-template-columns:2fr 1fr 2fr">
      <div><label for="raEp">Address devices dial</label><input id="raEp" value="${esc(v.endpoint || st.public_ip || "")}" placeholder="public IP or myhome.duckdns.org"></div>
      <div><label for="raPort">UDP port</label><input id="raPort" type="number" value="${v.port}"></div>
      <div><label for="raLanNet">Home network to reach <span class="muted">(optional)</span></label><input id="raLanNet" value="${esc(v.lan_net || "")}" placeholder="192.168.0.0/24"></div>
    </div>
    <label class="check"><input id="raOn" type="checkbox" ${v.enabled ? "checked" : ""}> VPN on</label>
    <div class="inline"><button class="btn" id="raFwd" ${r.found && !r.cgnat ? "" : "disabled"} title="Asks the router (UPnP) to forward the port">Open the port on my router</button>
      <span class="spacer"></span><button class="btn primary" id="raSave">Save</button></div>` : ""}
    ${v.enabled ? `<table class="snaps"><tbody id="raDevs">${devs || '<tr><td class="sub">No devices yet.</td></tr>'}</tbody></table>
      ${admin ? `<div class="inline"><input id="raDevName" placeholder="device name, e.g. Ravi's phone" style="flex:1"><button class="btn primary" id="raAdd">Add device</button></div>` : ""}
      <div id="raConfBox" hidden><div class="sub"><b>Shown once.</b> In the WireGuard app: + → Import from file (download below) or create from scratch and paste.</div>
        <pre id="raConf" class="mono" style="white-space:pre-wrap;word-break:break-all;font-size:12px;background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:8px"></pre>
        <div class="inline"><button class="btn" id="raDl">Download .conf</button><button class="btn" id="raCopy">Copy</button></div></div>` : ""}
  </details>
  <details class="uedit"><summary><b>3. Websites and the dashboard on the internet</b> (Cloudflare Tunnel) ${st.tunnel?.set_up ? `<span style="color:var(--ok)">● on${st.tunnel.dashboard ? ", dashboard at " + esc(st.tunnel.dashboard) : ""}</span>` : ""}</summary>
    <div class="sub">For your VMs' websites (and, if you want, this dashboard) at your own domain with HTTPS. Works with CGNAT too. Needs a domain on Cloudflare (free plan).
      Set it up under <b>Settings → Public access</b>. Use strong passwords and two-factor if the dashboard is on the internet.</div>
  </details>`;
  $("#raRe").onclick = e => { e.preventDefault(); $("#raNet").innerHTML = "Checking…"; loadRemote(true); };
  if ($("#raTsOn")) $("#raTsOn").onclick = () => runTask("tailscale", $("#raTsLan").checked ? "lan" : "", "");
  if ($("#raTsOff")) $("#raTsOff").onclick = e => { if (armed(e.target, "Disconnect?")) runTask("tailscale-off", "", ""); };
  if ($("#raSave")) $("#raSave").onclick = async () => {
    const lanNet = $("#raLanNet").value.trim();
    try { await call("PUT", "/remote/vpn", {enabled: $("#raOn").checked, endpoint: $("#raEp").value.trim(), port: +$("#raPort").value, lan: !!lanNet, lan_net: lanNet});
      toast("VPN settings saved"); loadRemote(false, true); }
    catch (e) { toast(e.message, true); }
  };
  if ($("#raFwd")) $("#raFwd").onclick = async () => {
    try { toast((await call("POST", "/remote/forward")).message); } catch (e) { toast(e.message, true); }
  };
  if ($("#raAdd")) $("#raAdd").onclick = async () => {
    try {
      const d = await call("POST", "/remote/vpn/devices", {name: $("#raDevName").value.trim()});
      raConf = d; await loadRemote(false, true);
      $("#raConfBox").hidden = false; $("#raConf").textContent = d.config;
      $("#raDl").onclick = () => { const a = document.createElement("a"); a.href = URL.createObjectURL(new Blob([d.config], {type: "text/plain"}));
        a.download = "p8s-" + d.device.name.replace(/[^A-Za-z0-9]+/g, "-").toLowerCase() + ".conf"; a.click(); };
      $("#raCopy").onclick = () => navigator.clipboard?.writeText(d.config).then(() => toast("Copied"));
    } catch (e) { toast(e.message, true); }
  };
  if ($("#raDevs")) $("#raDevs").onclick = async e => {
    const b = e.target.closest("button[data-ra]"); if (!b) return;
    if (!armed(b, "Remove — sure?")) return;
    try { await call("DELETE", "/remote/vpn/devices/" + encodeURIComponent(b.closest("tr").dataset.dev)); toast("Device removed"); loadRemote(false, true); }
    catch (err) { toast(err.message, true); }
  };
}
if (typeof openNode === "function") {
  const _openNodeRA = openNode;
  openNode = async function(){ loadRemote(); return _openNodeRA(); };
}
