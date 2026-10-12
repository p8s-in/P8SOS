// P8S OS subscription: the Subscription dialog (server ID, tier, key) and the
// "no subscription" notice for admins. Uses the dashboard helpers
// ($, esc, call, toast, me, isAdmin).
"use strict";

let lic = null, licLoaded = false, upd = null, updTimer = null;

document.body.insertAdjacentHTML("beforeend", `
<dialog id="dlgLicense" class="wide"><div class="dlg-body">
  <h2>Subscription and updates</h2>
  <div class="cards" style="margin:12px 0 6px">
    <div class="card"><h3>This server</h3><div class="big mono" id="lsId" style="font-size:17px"></div>
      <div class="inline" style="margin-top:6px"><button class="btn" id="lsCopy">Copy server ID</button></div>
      <div class="hint">You need this ID to buy a subscription for this server.</div></div>
    <div class="card"><h3>Subscription</h3><div class="big" id="lsTier"></div><div class="sub" id="lsState"></div></div>
  </div>
  <div id="lsMsg" class="toast" style="max-width:none;box-shadow:none;margin:8px 0"></div>
  <div id="lsDetail" class="sub"></div>
  <h3>Updates</h3>
  <div id="upBox">
    <div class="inline" style="flex-wrap:wrap"><span id="upNow"></span><span class="spacer"></span>
      <label for="upCh" class="sub" style="margin:0">Channel</label>
      <select id="upCh" style="width:auto"></select>
      <button class="btn" id="upCheck">Check now</button></div>
    <div id="upAvail" class="sub" style="margin-top:6px"></div>
    <div class="inline" id="upGoRow" hidden><button class="btn primary" id="upGo">Install update</button>
      <span class="sub">VMs keep running; the dashboard restarts for a minute.</span></div>
    <div id="upRun" hidden><div class="sub" id="upRunState" style="margin-top:8px"></div>
      <pre id="upLog" class="mono" style="max-height:220px;overflow:auto;background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:8px;font-size:12px;white-space:pre-wrap"></pre></div>
  </div>
  <h3>Enter key</h3>
  <textarea id="lsKey" class="mono" placeholder="P8S1-…" spellcheck="false" style="min-height:70px"></textarea>
  <div class="inline"><button class="btn primary" id="lsSave">Save key</button><button class="btn" id="lsCheck" hidden>Check now</button>
    <span class="spacer"></span><button class="btn danger" id="lsRemove" hidden>Remove key</button></div>
  <h3>Plans</h3>
  <div class="plans" id="lsTiers" style="display:grid;grid-template-columns:repeat(auto-fit,minmax(190px,1fr));gap:10px"></div>
  <div class="hint" id="lsShopHint"></div>
  <div class="dlg-actions"><button class="btn" id="lsClose">Close</button></div>
</div></dialog>`);

const licMoney = v => { try { return new Intl.NumberFormat("en-IN", {style: "currency", currency: "INR", maximumFractionDigits: 0}).format(v / 100); } catch { return "₹" + v / 100; } };
const licStates = {development: "Development build, everything unlocked", none: "No subscription", active: "Active", expiring: "Ending soon", grace: "Expired, grace period", expired: "Expired", revoked: "Cancelled"};
const shopLink = (q) => lic?.shop ? lic.shop + "?" + new URLSearchParams(q).toString() : "";

async function licenseShell(){
  lic = null;
  $("#licNotice")?.remove();
  if (!isAdmin()) return;
  try { lic = await call("GET", "/license"); } catch { return; }
  if (!$("#bLicense")) return;
  $("#bLicense").hidden = false;
  $("#bLicense").onclick = openLicense;
  sideGroups();
  licenseNotice();
  try { upd = await call("GET", "/updates"); } catch { upd = null; }
  if (upd?.update && !$("#bLicense .badge")) $("#bLicense").insertAdjacentHTML("beforeend", `<span class="badge" title="Update available">1</span>`);
}

function licenseNotice(){
  $("#licNotice")?.remove();
  if (!lic) return;
  const warn = ["expiring", "grace", "expired", "revoked"].includes(lic.state);
  if (!lic.notice && !warn) return;
  const k = "p8s.licNotice." + lic.state + "." + (lic.license?.expires || "");
  try { if (sessionStorage.getItem(k)) return; } catch {}
  const d = document.createElement("div");
  d.id = "licNotice"; d.className = "toast" + (warn ? " err" : "");
  d.style.cssText = "max-width:none;box-shadow:none;margin:0 0 14px;display:flex;gap:10px;align-items:center;flex-wrap:wrap";
  if (!warn) d.style.borderLeftColor = "var(--warn)";
  const text = lic.state === "none"
    ? "You do not have a subscription for this server: it runs P8S OS Home (free, up to " + lic.user_limit + " users, testing updates). Companies and hosting providers need a subscription."
    : lic.message;
  d.innerHTML = `<span style="flex:1;min-width:220px">${esc(text)}</span><button class="btn" id="licMore">Subscription</button><button class="btn" id="licHide" title="Hide until next sign-in">✕</button>`;
  $("main").prepend(d);
  $("#licMore").onclick = openLicense;
  $("#licHide").onclick = () => { try { sessionStorage.setItem(k, "1"); } catch {} d.remove(); };
}

function renderLicense(){
  const l = lic, x = l.license;
  $("#lsId").textContent = l.server_id;
  $("#lsTier").textContent = "P8S OS " + l.tier_name;
  $("#lsState").textContent = (licStates[l.state] || l.state) + (x ? " · until " + x.expires : "") + " · " + l.channel + " updates";
  $("#lsMsg").textContent = l.message;
  $("#lsMsg").className = "toast" + (["grace", "expired", "revoked", "expiring"].includes(l.state) ? " err" : "");
  $("#lsDetail").innerHTML = x
    ? `Licence <b class="mono">${esc(x.id)}</b> · ${esc(x.customer || "")}${x.email ? " &lt;" + esc(x.email) + "&gt;" : ""} · covers ${x.servers.length} server${x.servers.length > 1 ? "s" : ""}` +
      (l.checked ? ` · last checked ${esc(new Date(l.checked).toLocaleString())}` : "") +
      (l.shop ? ` · <a href="${esc(shopLink({renew: x.id, tier: x.tier}))}" target="_blank" rel="noopener">Renew</a>` : "")
    : (l.user_limit ? `Home allows up to ${l.user_limit} user accounts.` : "");
  $("#lsRemove").hidden = !x;
  $("#lsCheck").hidden = !(x && l.online);
  const rank = {home: 0, business: 1, hosting: 2, partner: 3, internal: 4};
  $("#lsTiers").innerHTML = l.tiers.map(t => {
    const cur = t.id === l.tier, up = rank[t.id] > rank[l.tier];
    const buy = t.price && up && l.shop ? `<a class="btn primary" style="margin-top:8px;display:inline-block;text-decoration:none" href="${esc(shopLink({tier: t.id, server: l.server_id}))}" target="_blank" rel="noopener">Buy ${esc(t.name)}</a>` : "";
    return `<div class="plan${cur ? " on" : ""}" style="cursor:default${cur ? ";border-color:var(--accent)" : ""}">
      <b>${esc(t.name)}${cur ? " · this server" : ""}</b><div class="sub">${esc(t.for)}</div>
      <div class="price">${t.price ? esc(licMoney(t.price)) : "Free"}</div><div class="sub">${t.price ? "per " + esc(t.per) + " / year + GST" : "forever"}</div>
      <ul class="sub" style="padding-left:16px;margin:8px 0 0">${t.summary.map(s => `<li>${esc(s)}</li>`).join("")}</ul>${buy}</div>`;
  }).join("");
  $("#lsShopHint").textContent = l.shop ? "Buying opens the P8S shop with this server's ID filled in. The key arrives by email; paste it above." :
    "Prices are suggested. To buy, send this server's ID to P8S; you'll get a key to paste above.";
}

async function openLicense(){
  try { lic = await call("GET", "/license"); } catch (e) { return toast(e.message, true); }
  $("#lsKey").value = "";
  renderLicense();
  $("#dlgLicense").showModal();
  await loadUpdates();
}

/* ---------- updates ---------- */
async function loadUpdates(){
  try { upd = await call("GET", "/updates"); } catch { upd = null; }
  renderUpdates();
}

function renderUpdates(){
  const u = upd;
  $("#upBox").hidden = !u;
  if (!u) return;
  $("#upNow").innerHTML = `Running <b class="mono">${esc(u.current || "?")}</b>` + (u.checked ? ` <span class="sub">· checked ${esc(new Date(u.checked).toLocaleString())}</span>` : "");
  $("#upCh").innerHTML = ["stable", "testing"].map(c => `<option value="${c}" ${c === u.channel ? "selected" : ""} ${u.channels.includes(c) ? "" : "disabled"}>${c}${c === "stable" && !u.channels.includes(c) ? " (subscription)" : ""}</option>`).join("");
  $("#upCh").disabled = $("#upCheck").disabled = !u.enabled;
  const run = u.install?.state === "running";
  if (!u.enabled) {
    $("#upAvail").innerHTML = `This server has no update server set. Add <code>"license_server": "https://…"</code> to /etc/p8s/p8sd.json, or keep updating with <code>git pull &amp;&amp; sudo bash update.sh</code>.`;
  } else if (u.error) {
    $("#upAvail").textContent = u.error;
  } else if (u.update) {
    $("#upAvail").innerHTML = `<b>${esc(u.available.version)}</b> is available on ${esc(u.channel)} (${esc(u.available.date)})${u.available.notes ? ": " + esc(u.available.notes) : ""}`;
  } else {
    $("#upAvail").textContent = u.checked ? "Up to date." : "Not checked yet.";
  }
  $("#upGoRow").hidden = !(u.enabled && u.update && !run);
  $("#upRun").hidden = !u.install;
  if (u.install) {
    const i = u.install;
    $("#upRunState").innerHTML = {running: `Installing <b>${esc(i.version)}</b>… (started ${esc(new Date(i.started).toLocaleTimeString())})`,
      done: `Updated from ${esc(i.from)} to <b>${esc(i.version)}</b>.`, failed: `<span style="color:var(--bad)">Updating to ${esc(i.version)} did not finish</span>; the log is below. The old version is still running.`}[i.state] || "";
    const log = $("#upLog"), atEnd = log.scrollTop + log.clientHeight >= log.scrollHeight - 20;
    log.textContent = i.log || "(no output yet)";
    if (atEnd) log.scrollTop = log.scrollHeight;
  }
  clearTimeout(updTimer);
  if (run && $("#dlgLicense").open) updTimer = setTimeout(pollUpdates, 4000);
}

async function pollUpdates(){ // p8sd restarts during an update: keep trying quietly
  try { upd = await fetch(API + "/updates", {headers: {"Authorization": "Bearer " + token}}).then(r => r.ok ? r.json() : Promise.reject()); } catch {}
  renderUpdates();
  if (upd?.install?.state === "running") return;
  if (upd?.install?.state === "done") { version = ""; toast("Updated to " + upd.install.version); }
}

$("#upCheck").onclick = async () => {
  try { upd = await call("POST", "/updates/check"); renderUpdates(); } catch (e) { toast(e.message, true); await loadUpdates(); }
};
$("#upCh").onchange = async () => {
  try { upd = await call("PUT", "/updates/channel", {channel: $("#upCh").value}); renderUpdates(); $("#upCheck").click(); } catch (e) { toast(e.message, true); }
};
$("#upGo").onclick = async () => {
  if (!confirm("Install " + upd.available.version + " now? VMs keep running; the dashboard restarts and may be unreachable for a minute.")) return;
  $("#upGo").disabled = true;
  try { const r = await call("POST", "/updates/install"); upd = r.status; renderUpdates(); toast("Update started"); }
  catch (e) { toast(e.message, true); }
  finally { $("#upGo").disabled = false; }
};

$("#lsClose").onclick = () => { clearTimeout(updTimer); $("#dlgLicense").close(); };
$("#lsCopy").onclick = async () => { try { await navigator.clipboard.writeText(lic.server_id); toast("Server ID copied"); } catch { toast(lic.server_id); } };
$("#lsSave").onclick = async () => {
  const key = $("#lsKey").value.trim();
  if (!key) return toast("Paste the key from your email first", true);
  try { lic = await call("PUT", "/license", {key}); $("#lsKey").value = ""; renderLicense(); licenseNotice(); toast("Subscription saved: " + lic.tier_name); }
  catch (e) { toast(e.message, true); }
};
$("#lsRemove").onclick = async () => {
  if (!confirm("Remove the key? This server goes back to Home (free). Running VMs are not affected.")) return;
  try { lic = await call("DELETE", "/license"); renderLicense(); licenseNotice(); toast("Key removed"); } catch (e) { toast(e.message, true); }
};
$("#lsCheck").onclick = async () => {
  try { lic = await call("POST", "/license/check"); renderLicense(); licenseNotice(); toast("Checked with P8S"); } catch (e) { toast(e.message, true); }
};
