// Datacenter → Network (admins): every server's internet, its guests'
// internet, and whether it can be reached from outside — in one place, with
// Repair per server. Uses the dashboard helpers ($, esc, call, toast, isAdmin, trackTask?).
"use strict";

let dcNetData = null, dcNetBusy = false;

function renderDcNet(){
  const box = $("#dcNet");
  if (!box) return;
  box.hidden = !isFullAdmin();
  if (box.hidden) return;
  const servers = dcNetData?.servers || [];
  const dot = (ok, txt) => `<span style="color:var(--${ok === null ? "muted" : ok ? "ok" : "bad"})">●</span> ${esc(txt)}`;
  const rows = servers.map(s => {
    if (!s.report) return `<tr><td><b>${esc(s.name)}</b></td><td colspan="3" class="sub" style="color:var(--bad)">${esc(s.error || "no answer")}</td><td></td></tr>`;
    const sec = t => s.report.sections.filter(x => t(x.title));
    const host = sec(t => /^Host internet/.test(t)).flatMap(x => x.items);
    const outside = sec(t => /^From outside/.test(t)).flatMap(x => x.items);
    const guest = sec(t => !/^(Host internet|From outside|Guests)/.test(t)).flatMap(x => x.items);
    const hostBad = host.filter(i => i.state === "bad"), guestBad = guest.filter(i => i.state === "bad");
    const bad = [...hostBad, ...guestBad];
    return `<tr data-dcn="${esc(s.id)}"><td><b>${esc(s.name)}</b>${s.self ? ' <span class="sub">this server</span>' : ""}</td>
      <td>${hostBad.length ? dot(false, "no internet") : dot(true, "internet ok")}</td>
      <td>${guestBad.length ? dot(false, guestBad.length + " problem(s)") : dot(true, "VMs and containers ok")}</td>
      <td class="sub" style="white-space:normal">${outside.map(i => esc(i.text)).join("<br>")}</td>
      <td style="text-align:right">${guestBad.length ? `<button class="btn" data-dcn-fix>Repair</button>` : ""}</td></tr>` +
      (bad.length ? `<tr><td></td><td colspan="4" class="sub" style="white-space:normal">${bad.map(i => `✘ ${esc(i.text)}${(i.hints || []).map(h => `<br>&nbsp;&nbsp;${esc(h)}`).join("")}`).join("<br>")}</td></tr>` : "");
  }).join("");
  box.innerHTML = `<div class="panel-head"><h2>Network</h2><span class="sub">${dcNetData ? "checked " + esc(fmtTime(dcNetData.at)) : "internet of every server and its VMs and containers, and how it is reached from outside"}</span>
      <span class="spacer"></span><button class="btn" id="dcNetGo" ${dcNetBusy ? "disabled" : ""}>${dcNetBusy ? "Checking… (up to a minute)" : dcNetData ? "Check again" : "Check all servers"}</button></div>
    ${servers.length ? `<table><thead><tr><th>Server</th><th>Server's internet</th><th>Guests' internet</th><th>From outside</th><th></th></tr></thead><tbody>${rows}</tbody></table>` : ""}
    <div class="hint" style="padding:8px 12px">No public IP is needed for servers, VMs or containers to reach the internet. Reaching them <b>from</b> outside needs a public IP with a port
      forwarded on the router — or, without one (CGNAT, common on home lines), Tailscale or a Cloudflare Tunnel (Server → Network → Remote access).</div>`;
  $("#dcNetGo").onclick = checkDcNet;
  for (const b of box.querySelectorAll("[data-dcn-fix]")) b.onclick = async () => {
    const id = b.closest("tr").dataset.dcn;
    b.disabled = true;
    try {
      await call("POST", `/cluster/netcheck/${encodeURIComponent(id || "self")}/repair`);
      toast("Repair started (VMs keep running); check again in a minute");
      setTimeout(checkDcNet, 30000);
    } catch (e) { toast(e.message, true); b.disabled = false; }
  };
}

async function checkDcNet(){
  if (dcNetBusy) return;
  dcNetBusy = true; renderDcNet();
  try { dcNetData = await call("GET", "/cluster/netcheck"); dcNetData.at = new Date().toISOString(); }
  catch (e) { toast(e.message, true); }
  dcNetBusy = false; renderDcNet();
}
