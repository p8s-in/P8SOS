// P8S Cloud: the customer side of a hosting provider (plans, wallet and
// payments, invoices, support, profile and API tokens) and the admin settings
// for it. Loaded after the main dashboard script and uses its helpers
// ($, esc, call, toast, me, isAdmin, refresh, armed, fmtTime, gb).
"use strict";

let cloudOn = false, cloudCur = "INR", plans = [], selPlan = null, bill = null, billTimer = null;
const money = (v, cur = cloudCur) => {
  try { return new Intl.NumberFormat("en-IN", {style: "currency", currency: cur}).format((v || 0) / 100); }
  catch { return (cur + " " + ((v || 0) / 100).toFixed(2)); }
};
const perMonth = hourly => hourly * 730;

document.body.insertAdjacentHTML("beforeend", `
<dialog id="dlgBilling" class="wide"><div class="dlg-body">
  <h2>Billing</h2>
  <div id="bCust">
    <div class="cards" style="margin:12px 0 6px">
      <div class="card"><h3>Balance</h3><div class="big" id="bBal"></div><div class="sub" id="bBalSub"></div></div>
      <div class="card"><h3>Servers</h3><div class="big" id="bBurn"></div><div class="sub" id="bBurnSub"></div></div>
    </div>
    <div id="bWarn" class="toast err" style="max-width:none;box-shadow:none" hidden></div>
    <h3>Add credit</h3>
    <div id="bPayBox">
      <div class="inline" id="bPresets"></div>
      <div class="inline"><input id="bAmt" type="number" min="1" step="1" placeholder="amount" style="flex:0 0 140px" aria-label="Amount">
        <span class="sub" id="bTaxNote"></span><span class="spacer"></span><button class="btn primary" id="bPay">Pay</button></div>
      <div class="hint">Credit is used by the hour; a server never costs more than its monthly price. Card, UPI and netbanking via Razorpay.</div>
    </div>
    <div id="bNoPay" class="sub" hidden>Online payment isn't available yet. Contact support to add credit (bank transfer / UPI).</div>
    <h3>Servers being billed</h3>
    <table class="snaps"><tbody id="bSubs"></tbody></table>
    <h3>Invoices</h3>
    <table class="snaps"><tbody id="bInv"></tbody></table>
    <h3>Transactions</h3>
    <table class="snaps"><tbody id="bTx"></tbody></table>
  </div>
  <div id="bAdmin" hidden>
    <div class="sub">Customers' balances. Record bank transfers / UPI payments with an invoice, or give goodwill credit.</div>
    <table class="snaps"><tbody id="bCustList"></tbody></table>
    <h3>All invoices</h3>
    <table class="snaps"><tbody id="bAllInv"></tbody></table>
  </div>
  <div class="dlg-actions"><button class="btn" id="bClose">Close</button></div>
</div></dialog>

<dialog id="dlgInvoice" class="wide"><div class="dlg-body">
  <div id="invBody"></div>
  <div class="dlg-actions"><button class="btn" id="invPrint">Print / Save as PDF</button><button class="btn" id="invClose">Close</button></div>
</div></dialog>

<dialog id="dlgSupport" class="wide"><div class="dlg-body">
  <h2>Support</h2>
  <div id="spList">
    <table class="snaps"><tbody id="spTickets"></tbody></table>
    <details class="uedit" id="spNewBox"><summary><b>New request</b></summary>
      <label for="spSubj">Subject</label><input id="spSubj" maxlength="120">
      <label for="spVM">Server</label><select id="spVM"></select>
      <label for="spText">What happened?</label><textarea id="spText" maxlength="10000" style="min-height:110px"></textarea>
      <div class="dlg-actions"><button class="btn primary" id="spSend">Send</button></div>
    </details>
  </div>
  <div id="spThread" hidden>
    <div class="inline"><button class="btn" id="spBack">← All requests</button><span class="spacer"></span><button class="btn" id="spCloseT">Mark solved</button></div>
    <h3 id="spTitle"></h3><div class="sub" id="spMeta"></div>
    <div class="thread" id="spMsgs"></div>
    <textarea id="spReply" placeholder="Write a reply" maxlength="10000"></textarea>
    <div class="dlg-actions"><button class="btn primary" id="spReplyGo">Reply</button></div>
  </div>
  <div class="dlg-actions"><button class="btn" id="spClose">Close</button></div>
</div></dialog>`);

/* ---------- shell ---------- */
async function cloudShell(){
  try {
    const a = await fetch(API + "/auth").then(r => r.json());
    brandName = a.brand || brandName; document.title = brandName;
    $("#hBrand").innerHTML = brandHTML();
  } catch {}
  try { bill = await call("GET", "/billing"); cloudOn = true; cloudCur = bill.currency; } catch { cloudOn = false; }
  $("#bBilling").hidden = !cloudOn; $("#bSupport").hidden = !cloudOn;
  if (typeof sideGroups === "function") sideGroups();
  $("#bBilling").onclick = openBilling; $("#bSupport").onclick = openSupport;
  const acc = $("#bMe").onclick;
  $("#bMe").onclick = async () => { await acc(); cloudAccount(); };
  const st = $("#bSettings").onclick;
  $("#bSettings").onclick = async () => { await st(); cloudSettings(); };
  badge();
  clearInterval(billTimer);
  billTimer = setInterval(async () => { if (!token || document.hidden || !cloudOn) return; try { bill = await call("GET", "/billing"); badge(); } catch {} }, 60000);
  const h = location.hash.slice(1);
  if (h === "billing") openBilling(); else if (h === "support") openSupport();
}

function badge(){
  const label = ic("card") + "<span>Billing</span>";
  if (!cloudOn || !bill || isAdmin()) { $("#bBilling").innerHTML = label; return; }
  $("#bBilling").innerHTML = label + `<span class="badge ${bill.balance < 0 || bill.suspended ? "bad" : ""}">${esc(money(bill.balance))}</span>`;
}

/* ---------- create: plans ---------- */
async function cloudCreate(){
  selPlan = null;
  try { const r = await call("GET", "/plans"); plans = r.plans; cloudCur = r.currency || cloudCur; } catch { plans = []; }
  const box = $("#cPlanBox");
  box.hidden = !plans.length;
  $("#cSizeRow").hidden = plans.length > 0 && !isAdmin();
  if (!plans.length) return;
  const cards = plans.map(p => `<div class="plan" data-plan="${esc(p.id)}" tabindex="0">
      <b>${esc(p.name)}${p.hidden ? ' <span class="pill">hidden</span>' : ""}</b>
      <div class="price">${esc(money(p.price_monthly))}<span class="sub">/mo</span></div>
      <div class="sub">${p.vcpus} CPU · ${gb(p.memory_mib)} · ${p.disk_gb} GB${p.public_ip ? " · public IP" : ""}</div>
      <div class="sub">${esc(money(p.hourly))}/hour</div></div>`).join("");
  $("#cPlans").innerHTML = cards + (isAdmin() ? `<div class="plan" data-plan="" tabindex="0"><b>Custom</b><div class="sub">any size, not billed</div></div>` : "");
  pickPlan(plans[0].id);
}
function pickPlan(id){
  selPlan = plans.find(p => p.id === id) || null;
  for (const el of document.querySelectorAll("#cPlans .plan")) el.classList.toggle("sel", el.dataset.plan === (selPlan?.id || ""));
  $("#cSizeRow").hidden = !!selPlan || (!isAdmin() && plans.length > 0);
  const pub = !!selPlan?.public_ip;
  $("#cNet").disabled = pub;
  if (pub) $("#cNetHint").textContent = "This plan includes its own public IP address.";
  else netHint();
  const min = +($("#cTpl").selectedOptions[0]?.dataset.min || 1);
  if (selPlan && selPlan.disk_gb < min) toast(`This OS needs at least ${min} GB of disk; pick a bigger plan`, true);
}
document.addEventListener("click", e => { const p = e.target.closest("#cPlans .plan"); if (p) pickPlan(p.dataset.plan); });
document.addEventListener("keydown", e => { const p = e.target.closest?.("#cPlans .plan"); if (p && (e.key === "Enter" || e.key === " ")) { e.preventDefault(); pickPlan(p.dataset.plan); } });
function cloudPlan(){ return $("#cPlanBox").hidden ? null : selPlan; }

/* ---------- billing ---------- */
async function openBilling(){
  $("#bCust").hidden = isAdmin(); $("#bAdmin").hidden = !isAdmin();
  $("#dlgBilling").showModal();
  if (isAdmin()) return loadAdminBilling();
  await loadBilling();
}
async function loadBilling(){
  try {
    bill = await call("GET", "/billing"); cloudCur = bill.currency; badge();
    const [tx, inv] = await Promise.all([call("GET", "/billing/entries?limit=100"), call("GET", "/billing/invoices")]);
    $("#bBal").textContent = money(bill.balance);
    $("#bBal").style.color = bill.balance < 0 ? "var(--bad)" : "";
    const hours = bill.hourly ? Math.floor(bill.balance / bill.hourly) : 0;
    $("#bBalSub").textContent = bill.hourly ? (bill.balance > 0 ? `lasts about ${hours >= 48 ? Math.floor(hours / 24) + " days" : hours + " hours"}` : "add credit to keep servers running") : "";
    $("#bBurn").textContent = money(bill.servers.reduce((a, x) => a + x.monthly, 0)) + "/mo";
    $("#bBurnSub").textContent = `${bill.servers.length} server(s) · ${money(bill.hourly)}/hour`;
    $("#bWarn").hidden = !(bill.suspended || bill.balance < 0);
    $("#bWarn").textContent = bill.suspended ? "Your servers were stopped because the balance stayed negative. Add credit, then start them again."
      : `Your balance is negative. Add credit within ${bill.grace_hours} hours of it going negative, or your servers will be stopped.`;
    $("#bPayBox").hidden = !bill.payments; $("#bNoPay").hidden = bill.payments;
    const min = bill.min_topup / 100;
    $("#bPresets").innerHTML = [min, 500, 1000, 2000, 5000].filter((v, i, a) => v >= min && a.indexOf(v) === i).slice(0, 4)
      .map(v => `<button class="btn" data-amt="${v}">${esc(money(v * 100))}</button>`).join("");
    taxNote();
    $("#bSubs").innerHTML = bill.servers.length ? bill.servers.map(x => `<tr><td><b>${esc(x.vm)}</b><div class="sub">${esc(x.plan_name)} · since ${esc(fmtTime(x.since))}</div></td>
      <td class="mono">${esc(money(x.monthly))}/mo</td></tr>`).join("") : `<tr><td class="muted">No servers on a plan.</td></tr>`;
    $("#bInv").innerHTML = inv.invoices.length ? inv.invoices.map(invRow).join("") : `<tr><td class="muted">No invoices yet.</td></tr>`;
    $("#bTx").innerHTML = tx.entries.length ? tx.entries.map(e => `<tr><td>${esc(e.note || e.kind)}<div class="sub">${esc(fmtTime(e.time))}${e.invoice ? " · " + esc(e.invoice) : ""}</div></td>
      <td class="mono" style="color:${e.amount < 0 ? "" : "var(--ok)"}">${e.amount > 0 ? "+" : ""}${esc(money(e.amount))}</td></tr>`).join("")
      : `<tr><td class="muted">Nothing yet.</td></tr>`;
  } catch (err) { toast(err.message, true); }
}
function invRow(i){
  return `<tr><td><b>${esc(i.number)}</b><div class="sub">${esc(i.date)}${isAdmin() ? " · " + esc(i.user) : ""} · ${esc(i.method)}</div></td>
    <td class="mono">${esc(money(i.total, i.currency))}</td><td><button class="btn" data-inv="${esc(i.number)}">View</button></td></tr>`;
}
function taxNote(){
  const a = Math.round((+$("#bAmt").value || 0) * 100);
  $("#bTaxNote").textContent = a ? `+ ${bill.tax_percent}% ${bill.tax_name} = ${money(a + Math.round(a * bill.tax_percent / 100))}` : `${bill.tax_name} ${bill.tax_percent}% is added`;
}
$("#bAmt").oninput = taxNote;
$("#bPresets").addEventListener("click", e => { const b = e.target.closest("[data-amt]"); if (b) { $("#bAmt").value = b.dataset.amt; taxNote(); } });
function loadScript(src){
  return new Promise((ok, bad) => {
    if (document.querySelector(`script[src="${src}"]`)) return ok();
    const s = document.createElement("script"); s.src = src; s.onload = ok; s.onerror = () => bad(new Error("couldn't load the payment window; check your connection"));
    document.head.append(s);
  });
}
$("#bPay").onclick = async () => {
  const amount = Math.round((+$("#bAmt").value || 0) * 100);
  if (amount < bill.min_topup) return toast("The smallest top-up is " + money(bill.min_topup), true);
  const go = $("#bPay"); go.disabled = true;
  try {
    const o = await call("POST", "/billing/topup", {amount});
    await loadScript("https://checkout.razorpay.com/v1/checkout.js");
    const rz = new window.Razorpay({
      key: o.key_id, amount: o.amount, currency: o.currency, name: o.name, order_id: o.order_id,
      description: "Wallet top-up " + money(o.credit, o.currency), prefill: o.prefill,
      theme: {color: getComputedStyle(document.documentElement).getPropertyValue("--accent").trim()},
      handler: async resp => {
        try {
          const r = await call("POST", "/billing/topup/verify", resp);
          toast(`Thank you! ${money(r.invoice.subtotal, r.invoice.currency)} added. Invoice ${r.invoice.number}.`);
          $("#bAmt").value = ""; loadBilling();
        } catch (err) { toast("Payment received but not confirmed yet: " + err.message + " — it will show up shortly.", true); }
      },
    });
    rz.on("payment.failed", e => toast("Payment failed: " + (e.error?.description || "try again"), true));
    rz.open();
  } catch (err) { toast(err.message, true); }
  go.disabled = false;
};
document.addEventListener("click", async e => {
  const b = e.target.closest("[data-inv]"); if (!b) return;
  try { showInvoice(await call("GET", "/billing/invoices/" + encodeURIComponent(b.dataset.inv))); } catch (err) { toast(err.message, true); }
});
function showInvoice(i){
  const party = p => `<b>${esc(p.name)}</b>${p.address ? "<br>" + esc(p.address).replace(/\n/g, "<br>") : ""}${p.tax_id ? "<br>GSTIN: " + esc(p.tax_id) : ""}${p.email ? "<br>" + esc(p.email) : ""}`;
  $("#invBody").innerHTML = `<div class="inv">
    <div style="display:flex;justify-content:space-between;gap:20px;flex-wrap:wrap">
      <div>${party(i.seller)}</div>
      <div style="text-align:right"><h2 style="margin:0">Tax invoice</h2><div>No. <b>${esc(i.number)}</b></div><div>Date ${esc(i.date)}</div></div>
    </div>
    <div style="margin:18px 0"><div style="color:#666;font-size:12px">Billed to</div>${party(i.buyer)}</div>
    <table style="width:100%"><thead><tr><th style="text-align:left">Description</th><th style="text-align:right">Amount</th></tr></thead><tbody>
      <tr><td>${esc(i.item)}</td><td style="text-align:right">${esc(money(i.subtotal, i.currency))}</td></tr>
      <tr><td>${esc(i.tax_name)} ${i.tax_percent}%</td><td style="text-align:right">${esc(money(i.tax, i.currency))}</td></tr>
      <tr><td><b>Total</b></td><td style="text-align:right"><b>${esc(money(i.total, i.currency))}</b></td></tr>
    </tbody></table>
    <div style="margin-top:14px;color:#555;font-size:12px">Paid ${i.method === "razorpay" ? "online (Razorpay)" : "by transfer"} · reference ${esc(i.payment_ref)}</div>
  </div>`;
  $("#dlgInvoice").showModal();
}
$("#invPrint").onclick = () => window.print();
$("#invClose").onclick = () => $("#dlgInvoice").close();
$("#bClose").onclick = () => $("#dlgBilling").close();

async function loadAdminBilling(){
  try {
    const [c, inv] = await Promise.all([call("GET", "/billing/customers"), call("GET", "/billing/invoices?all=1")]);
    cloudCur = c.currency;
    $("#bCustList").innerHTML = c.customers.length ? c.customers.map(x => `<tr data-u="${esc(x.name)}"><td><b>${esc(x.name)}</b>${x.suspended ? '<span class="pill">suspended</span>' : ""}
        <div class="sub">${esc(x.email || "no email")} · ${x.servers} server(s) · ${esc(money(perMonth(x.hourly)))}/mo</div>
        <div class="inline" data-credit hidden><input data-k="amt" type="number" step="1" placeholder="amount (− to debit)" style="flex:0 0 150px">
          <input data-k="note" placeholder="note / payment reference" maxlength="100">
          <label class="check" style="margin:0;white-space:nowrap"><input data-k="inv" type="checkbox"> payment (invoice)</label>
          <button class="btn primary" data-b="save">Save</button></div></td>
        <td class="mono" style="color:${x.balance < 0 ? "var(--bad)" : ""}">${esc(money(x.balance))}</td>
        <td><button class="btn" data-b="credit">Credit</button></td></tr>`).join("")
      : `<tr><td class="muted">No customers yet.</td></tr>`;
    $("#bAllInv").innerHTML = inv.invoices.length ? inv.invoices.slice(0, 100).map(invRow).join("") : `<tr><td class="muted">No invoices yet.</td></tr>`;
  } catch (err) { toast(err.message, true); }
}
$("#bCustList").addEventListener("click", async e => {
  const b = e.target.closest("[data-b]"); if (!b) return;
  const row = b.closest("tr"), box = row.querySelector("[data-credit]");
  if (b.dataset.b === "credit") { box.hidden = !box.hidden; return; }
  const amount = Math.round((+box.querySelector('[data-k="amt"]').value || 0) * 100);
  const note = box.querySelector('[data-k="note"]').value.trim(), invoice = box.querySelector('[data-k="inv"]').checked;
  if (!amount) return toast("Enter an amount", true);
  if (invoice && amount < 0) return toast("A payment with invoice must be positive", true);
  try {
    const r = await call("POST", "/billing/adjust", {user: row.dataset.u, amount, note, invoice});
    toast(`${row.dataset.u}: balance ${money(r.balance)}${r.invoice ? " · invoice " + r.invoice.number : ""}`); loadAdminBilling();
  } catch (err) { toast(err.message, true); }
});

/* ---------- support ---------- */
let ticket = null;
async function openSupport(){
  $("#spList").hidden = false; $("#spThread").hidden = true;
  $("#spVM").innerHTML = `<option value="">—</option>` + vms.map(v => `<option>${esc(v.name)}</option>`).join("");
  $("#spNewBox").hidden = isAdmin();
  $("#dlgSupport").showModal();
  await loadTickets();
}
async function loadTickets(){
  try {
    const l = (await call("GET", "/tickets")).tickets;
    const label = {open: isAdmin() ? "waiting for you" : "waiting for support", answered: isAdmin() ? "answered" : "new reply", closed: "solved"};
    $("#spTickets").innerHTML = l.length ? l.map(t => `<tr data-t="${esc(t.id)}" style="cursor:pointer"><td><b>${esc(t.subject)}</b>
        <span class="pill ${(t.status === "open") === isAdmin() && t.status !== "closed" ? "on" : ""}">${esc(label[t.status] || t.status)}</span>
        <div class="sub">${esc(t.id)}${isAdmin() ? " · " + esc(t.user) : ""}${t.vm ? " · " + esc(t.vm) : ""} · updated ${esc(fmtTime(t.updated))}</div></td></tr>`).join("")
      : `<tr><td class="muted">${isAdmin() ? "No support requests." : "No requests yet. Open one below — we usually answer within a day."}</td></tr>`;
  } catch (err) { toast(err.message, true); }
}
$("#spTickets").addEventListener("click", e => { const r = e.target.closest("[data-t]"); if (r) openTicket(r.dataset.t); });
async function openTicket(id){
  try { ticket = await call("GET", "/tickets/" + encodeURIComponent(id)); } catch (err) { return toast(err.message, true); }
  $("#spList").hidden = true; $("#spThread").hidden = false;
  $("#spTitle").textContent = ticket.subject;
  $("#spMeta").textContent = `${ticket.id} · ${ticket.user}${ticket.vm ? " · " + ticket.vm : ""} · ${ticket.status}`;
  $("#spMsgs").innerHTML = ticket.messages.map(m => `<div class="m ${m.staff ? "staff" : ""}"><div class="sub">${esc(m.staff ? brandName + " support (" + m.from + ")" : m.from)} · ${esc(fmtTime(m.time))}</div>${esc(m.text)}</div>`).join("");
  $("#spCloseT").hidden = ticket.status === "closed";
  $("#spReply").value = "";
}
$("#spBack").onclick = () => { $("#spList").hidden = false; $("#spThread").hidden = true; loadTickets(); };
$("#spSend").onclick = async () => {
  try {
    const t = await call("POST", "/tickets", {subject: $("#spSubj").value.trim(), vm: $("#spVM").value, text: $("#spText").value});
    $("#spSubj").value = $("#spText").value = ""; $("#spNewBox").open = false; toast(`Request ${t.id} sent`); openTicket(t.id);
  } catch (err) { toast(err.message, true); }
};
$("#spReplyGo").onclick = async () => {
  try { await call("POST", `/tickets/${encodeURIComponent(ticket.id)}/reply`, {text: $("#spReply").value}); openTicket(ticket.id); }
  catch (err) { toast(err.message, true); }
};
$("#spCloseT").onclick = async () => {
  try { await call("POST", `/tickets/${encodeURIComponent(ticket.id)}/close`); toast("Marked as solved"); openTicket(ticket.id); }
  catch (err) { toast(err.message, true); }
};
$("#spClose").onclick = () => $("#dlgSupport").close();

/* ---------- account: profile + API tokens ---------- */
function cloudAccount(){
  if ($("#acProfile")) $("#acProfile").remove();
  if (!me?.account) return;
  $("#acUser").insertAdjacentHTML("afterbegin", `<div id="acProfile">
    <h3>Profile <span class="sub" style="font-weight:400">(shown on your invoices)</span></h3>
    <div class="row" style="grid-template-columns:1fr 1fr">
      <div><label for="pfEmail">Email${me.email && !me.verified ? ' <span class="pill">not confirmed</span>' : ""}</label><input id="pfEmail" type="email" value="${esc(me.email || "")}"></div>
      <div><label for="pfName">Full name</label><input id="pfName" value="${esc(me.full_name || "")}" maxlength="100"></div>
      <div><label for="pfPhone">Phone</label><input id="pfPhone" value="${esc(me.phone || "")}" maxlength="20" placeholder="+91…"></div>
      <div><label for="pfCo">Company <span class="muted">(optional)</span></label><input id="pfCo" value="${esc(me.company || "")}" maxlength="100"></div>
      <div><label for="pfGst">GSTIN <span class="muted">(optional)</span></label><input id="pfGst" value="${esc(me.gstin || "")}" maxlength="15"></div>
    </div>
    <label for="pfAddr">Billing address</label><textarea id="pfAddr" maxlength="400">${esc(me.address || "")}</textarea>
    <div class="inline"><span class="spacer"></span><button class="btn" id="pfGo">Save profile</button></div>
    <h3>API tokens</h3>
    <div class="sub">For scripts and tools: <span class="mono">Authorization: Bearer p8sk_…</span> on <span class="mono">${esc(location.origin)}/api/v1/…</span> — acts as you. See docs/api.md.</div>
    <table class="snaps"><tbody id="tkList"></tbody></table>
    <div class="inline"><input id="tkName" placeholder="name, e.g. laptop script" maxlength="40"><button class="btn" id="tkGo">Create token</button></div>
    <div class="row" style="grid-template-columns:auto 1fr 140px;align-items:end;margin-top:4px">
      <label class="check" style="margin:0 0 10px"><input id="tkRO" type="checkbox"> Read-only</label>
      <div><label for="tkVMs">Only these VMs (optional)</label><input id="tkVMs" placeholder="web1, db1 — empty = all yours" autocomplete="off"></div>
      <div><label for="tkDays">Valid for (days)</label><input id="tkDays" type="number" min="1" max="3650" placeholder="forever"></div>
    </div>
    <div class="hint">Give scripts the least they need: a monitoring tool a read-only token, a deploy script one for its VMs only.</div>
    <div class="hint" id="tkNew" hidden></div>
  </div>`);
  $("#pfGo").onclick = async () => {
    try {
      const u = await call("PUT", "/me/profile", {email: $("#pfEmail").value.trim(), full_name: $("#pfName").value.trim(), phone: $("#pfPhone").value.trim(),
        company: $("#pfCo").value.trim(), gstin: $("#pfGst").value.trim(), address: $("#pfAddr").value.trim()});
      toast(u.email && !u.verified ? "Saved. We sent a confirmation link to your new email." : "Profile saved");
      me = await call("GET", "/me");
    } catch (err) { toast(err.message, true); }
  };
  $("#tkGo").onclick = async () => {
    try {
      const vmsList = $("#tkVMs").value.split(/[\s,]+/).map(x => x.trim()).filter(Boolean);
      const r = await call("POST", "/me/tokens", {name: $("#tkName").value.trim(), read_only: $("#tkRO").checked, vms: vmsList, days: +$("#tkDays").value || 0});
      $("#tkName").value = ""; $("#tkVMs").value = ""; $("#tkDays").value = ""; $("#tkRO").checked = false;
      $("#tkNew").hidden = false;
      $("#tkNew").innerHTML = `Copy it now — it won't be shown again: <span class="mono copy" data-copy="${esc(r.secret)}">${esc(r.secret)}</span>`;
      loadTokens();
    } catch (err) { toast(err.message, true); }
  };
  loadTokens();
}
async function loadTokens(){
  try {
    const l = (await call("GET", "/me/tokens")).tokens;
    const scope = t => [t.read_only ? "read-only" : "", (t.vms || []).length ? "VMs " + t.vms.join(", ") : "", t.expires_at ? "until " + fmtTime(t.expires_at) : ""].filter(Boolean).join(" · ") || "full access (acts as you)";
    $("#tkList").innerHTML = l.length ? l.map(t => `<tr data-tk="${esc(t.id)}"><td><b>${esc(t.name)}</b> <span class="sub">${esc(scope(t))}</span><div class="sub">created ${esc(fmtTime(t.created_at))} · ${t.last_used ? "used " + esc(fmtTime(t.last_used)) : "never used"}</div></td>
      <td><button class="btn danger" data-tkdel>Revoke</button></td></tr>`).join("") : `<tr><td class="muted">No tokens.</td></tr>`;
  } catch {}
}
document.addEventListener("click", async e => {
  const b = e.target.closest("[data-tkdel]"); if (!b) return;
  if (!armed(b, "Revoke — sure?")) return;
  try { await call("DELETE", "/me/tokens/" + b.closest("tr").dataset.tk); toast("Token revoked"); loadTokens(); } catch (err) { toast(err.message, true); }
});

/* ---------- settings: cloud, plans, payments, email ---------- */
let cset = null;
async function cloudSettings(){
  if (!$("#csBox")) {
    $("#tClose").closest(".dlg-actions").insertAdjacentHTML("beforebegin", `<div id="csBox">
    <h3>Selling: P8S Cloud</h3>
    <div class="sub">What your customers see: brand, sign-up, prices and invoices.</div>
    <div class="row">
      <div><label for="csBrand">Brand</label><input id="csBrand" maxlength="40"></div>
      <div><label for="csUrl">Panel address</label><input id="csUrl" placeholder="https://panel.example.com"></div>
      <div><label for="csSupport">Support email</label><input id="csSupport" type="email"></div>
    </div>
    <label class="check"><input id="csSignup" type="checkbox"> Anyone can create an account (self sign-up)</label>
    <div class="row">
      <div><label for="csCur">Currency</label><input id="csCur" maxlength="3"></div>
      <div><label for="csTaxN">Tax</label><input id="csTaxN" maxlength="20"></div>
      <div><label for="csTaxP">Tax %</label><input id="csTaxP" type="number" min="0" max="50"></div>
    </div>
    <div class="row">
      <div><label for="csSeller">Seller (on invoices)</label><input id="csSeller" maxlength="100"></div>
      <div><label for="csGst">Seller GSTIN</label><input id="csGst" maxlength="30"></div>
      <div><label for="csPrefix">Invoice prefix</label><input id="csPrefix" maxlength="10"></div>
    </div>
    <label for="csAddr">Seller address</label><textarea id="csAddr" maxlength="400"></textarea>
    <div class="row">
      <div><label for="csMin">Smallest top-up</label><input id="csMin" type="number" min="1"></div>
      <div><label for="csGrace">Stop servers after (hours negative)</label><input id="csGrace" type="number" min="1"></div>
      <div><label for="csPool">IP pool for plans with public IP</label><select id="csPool"></select></div>
    </div>
    <h3>Plans (sizes and prices)</h3>
    <div class="sub">Monthly price before tax; customers pay by the hour (price ÷ 730), never more than the monthly price.</div>
    <table class="snaps"><tbody id="plList"></tbody></table>
    <div class="row qrow" style="grid-template-columns:repeat(7,1fr)">
      <div><label>Id</label><input id="plId" placeholder="s1"></div>
      <div><label>Name</label><input id="plName" placeholder="Starter"></div>
      <div><label>CPUs</label><input id="plCpu" type="number" min="1" value="1"></div>
      <div><label>RAM MB</label><input id="plMem" type="number" min="256" step="256" value="1024"></div>
      <div><label>Disk GB</label><input id="plDisk" type="number" min="1" value="25"></div>
      <div><label>Price/mo</label><input id="plPrice" type="number" min="0" step="1" value="399"></div>
      <div><label>&nbsp;</label><button class="btn primary" id="plGo">Save plan</button></div>
    </div>
    <label class="check"><input id="plPub" type="checkbox"> includes a public IP</label>
    <label class="check"><input id="plHide" type="checkbox"> hidden (not offered for new servers)</label>
    <h3>Payments (Razorpay)</h3>
    <div class="row">
      <div><label for="csKey">Key id</label><input id="csKey" placeholder="rzp_live_… or rzp_test_…" autocomplete="off"></div>
      <div><label for="csSec">Key secret</label><input id="csSec" type="password" autocomplete="off"></div>
      <div><label for="csWh">Webhook secret</label><input id="csWh" type="password" autocomplete="off"></div>
    </div>
    <div class="hint">Razorpay dashboard → Account &amp; Settings → API keys. Webhook (recommended): URL <span class="mono" id="csWhUrl"></span>, events <span class="mono">payment.captured</span>, same secret as above. Turn on automatic capture.</div>
    <h3>Email (SMTP)</h3>
    <div class="row">
      <div><label for="csHost">Server</label><input id="csHost" placeholder="smtp.example.com"></div>
      <div><label for="csPort">Port</label><input id="csPort" type="number" placeholder="587"></div>
      <div><label for="csFrom">From</label><input id="csFrom" placeholder="P8S Cloud &lt;noreply@example.com&gt;"></div>
      <div><label for="csUser">User</label><input id="csUser" autocomplete="off"></div>
      <div><label for="csPass">Password</label><input id="csPass" type="password" autocomplete="off"></div>
    </div>
    <div class="inline"><button class="btn primary" id="csGo">Save settings</button><span class="spacer"></span>
      <input id="csTestTo" type="email" placeholder="test address" style="flex:0 0 200px"><button class="btn" id="csTest">Send test email</button></div>
    <details class="uedit"><summary><b>Sent emails</b></summary><table class="snaps"><tbody id="csOut"></tbody></table></details>
    </div>`);
    $("#csGo").onclick = saveCloud; $("#plGo").onclick = savePlan; $("#csTest").onclick = testMail;
    $("#plList").addEventListener("click", planAction);
  }
  try {
    const [st, pl, pools, out] = await Promise.all([call("GET", "/cloud/settings"), call("GET", "/plans"), call("GET", "/ippools").catch(() => ({pools: []})), call("GET", "/cloud/outbox").catch(() => ({emails: []}))]);
    cset = st; cloudCur = st.currency;
    const v = (id, x) => { $(id).value = x ?? ""; };
    v("#csBrand", st.brand); v("#csUrl", st.public_url); v("#csSupport", st.support_email); $("#csSignup").checked = st.signup_open;
    v("#csCur", st.currency); v("#csTaxN", st.tax_name); v("#csTaxP", st.tax_percent); v("#csSeller", st.seller_name); v("#csGst", st.seller_tax_id);
    v("#csPrefix", st.invoice_prefix); v("#csAddr", st.seller_address); v("#csMin", st.min_topup / 100); v("#csGrace", st.grace_hours);
    $("#csPool").innerHTML = `<option value="">— none —</option>` + pools.pools.map(p => `<option>${esc(p.name)}</option>`).join(""); $("#csPool").value = st.public_pool || "";
    v("#csKey", st.razorpay_key_id); v("#csSec", st.razorpay_key_secret); v("#csWh", st.razorpay_webhook_secret);
    v("#csHost", st.smtp_host); v("#csPort", st.smtp_port); v("#csFrom", st.mail_from); v("#csUser", st.smtp_user); v("#csPass", st.smtp_pass);
    $("#csWhUrl").textContent = (st.public_url || location.origin) + "/api/v1/billing/razorpay/webhook";
    $("#plList").innerHTML = pl.plans.length ? pl.plans.map(p => `<tr data-pl="${esc(p.id)}"><td><b>${esc(p.name)}</b> <span class="mono sub">${esc(p.id)}</span>${p.hidden ? ' <span class="pill">hidden</span>' : ""}
        <div class="sub">${p.vcpus} CPU · ${gb(p.memory_mib)} · ${p.disk_gb} GB${p.public_ip ? " · public IP" : ""} · ${esc(money(p.price_monthly))}/mo (${esc(money(p.hourly))}/h)</div></td>
        <td><button class="btn" data-pl-edit>Edit</button><button class="btn danger" data-pl-del>Delete</button></td></tr>`).join("")
      : `<tr><td class="muted">No plans yet. Without plans, customers create servers within their quota and nothing is billed.</td></tr>`;
    plans = pl.plans;
    $("#csOut").innerHTML = out.emails.length ? out.emails.map(m => `<tr><td>${esc(m.subject)}<div class="sub">${esc(fmtTime(m.time))} · ${esc(m.to)}${m.error ? " · " + esc(m.error) : ""}</div></td>
      <td class="mono" style="color:${m.status === "sent" ? "var(--ok)" : "var(--bad)"}">${esc(m.status)}</td></tr>`).join("") : `<tr><td class="muted">No emails yet.</td></tr>`;
  } catch (err) { toast(err.message, true); }
}
async function saveCloud(){
  const n = id => +$(id).value || 0;
  const st = {...cset, brand: $("#csBrand").value.trim(), public_url: $("#csUrl").value.trim(), support_email: $("#csSupport").value.trim(),
    signup_open: $("#csSignup").checked, currency: $("#csCur").value.trim().toUpperCase(), tax_name: $("#csTaxN").value.trim(), tax_percent: n("#csTaxP"),
    seller_name: $("#csSeller").value.trim(), seller_tax_id: $("#csGst").value.trim(), invoice_prefix: $("#csPrefix").value.trim().toUpperCase(),
    seller_address: $("#csAddr").value.trim(), min_topup: Math.round(n("#csMin") * 100), grace_hours: n("#csGrace"), public_pool: $("#csPool").value,
    razorpay_key_id: $("#csKey").value.trim(), razorpay_key_secret: $("#csSec").value, razorpay_webhook_secret: $("#csWh").value,
    smtp_host: $("#csHost").value.trim(), smtp_port: n("#csPort") || 587, mail_from: $("#csFrom").value.trim(), smtp_user: $("#csUser").value.trim(), smtp_pass: $("#csPass").value};
  try {
    cset = await call("PUT", "/cloud/settings", st); toast("Settings saved");
    brandName = cset.brand; $("#hBrand").innerHTML = brandHTML(); document.title = brandName;
    bill = await call("GET", "/billing").catch(() => bill);
  } catch (err) { toast(err.message, true); }
}
async function savePlan(){
  const id = $("#plId").value.trim().toLowerCase();
  if (!id) return toast("Give the plan an id, e.g. s1", true);
  try {
    await call("PUT", "/plans/" + encodeURIComponent(id), {name: $("#plName").value.trim(), vcpus: +$("#plCpu").value, memory_mib: +$("#plMem").value,
      disk_gb: +$("#plDisk").value, price_monthly: Math.round((+$("#plPrice").value || 0) * 100), public_ip: $("#plPub").checked, hidden: $("#plHide").checked});
    toast(`Plan ${id} saved`); cloudSettings();
  } catch (err) { toast(err.message, true); }
}
async function planAction(e){
  const row = e.target.closest("tr[data-pl]"); if (!row) return;
  const p = plans.find(x => x.id === row.dataset.pl);
  if (e.target.closest("[data-pl-edit]")) {
    $("#plId").value = p.id; $("#plName").value = p.name; $("#plCpu").value = p.vcpus; $("#plMem").value = p.memory_mib;
    $("#plDisk").value = p.disk_gb; $("#plPrice").value = p.price_monthly / 100; $("#plPub").checked = p.public_ip; $("#plHide").checked = p.hidden;
    $("#plId").scrollIntoView({block: "center"});
  } else if (e.target.closest("[data-pl-del]")) {
    const b = e.target.closest("[data-pl-del]");
    if (!armed(b, "Delete — sure?")) return;
    try { await call("DELETE", "/plans/" + encodeURIComponent(p.id)); toast("Plan deleted (servers on it keep their price)"); cloudSettings(); }
    catch (err) { toast(err.message, true); }
  }
}
async function testMail(){
  const to = $("#csTestTo").value.trim(); if (!to) return toast("Enter an address to send the test to", true);
  try { await call("POST", "/cloud/test-email", {to}); toast("Test email sent to " + to); } catch (err) { toast(err.message, true); }
  cloudSettings();
}
