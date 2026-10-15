// Public addresses of a VM or container (Manage page): the addresses it has
// from its IP pool, and for admins: add an extra IPv4, add IPv6, remove an
// extra one, change the main address (same or another pool).
// Uses the dashboard helpers ($, esc, call, toast, isAdmin, armed).
"use strict";

let pubPools = null;

// renderPublicIPs fills el for kind "vms" | "containers".
async function renderPublicIPs(el, kind, name, p, opts = {}){
  const addrs = [`<b class="mono">${esc(p.ip)}/${p.prefix}</b> <span class="sub">main</span>`]
    .concat((p.extra || []).map(a => `<span class="mono">${esc(a)}/${p.prefix}</span>` +
      (isAdmin() ? ` <button class="btn" data-pub="remove" data-ip="${esc(a)}" title="Give this address back to the pool">✕</button>` : "")))
    .concat(p.ip6 ? [`<span class="mono">${esc(p.ip6)}/${p.prefix6}</span> <span class="sub">IPv6</span>`] : []);
  let html = `<div>${addrs.join(" · ")}</div>
    <div class="sub">gateway <span class="mono">${esc(p.gateway)}</span>${p.gateway6 ? ` · IPv6 gateway <span class="mono">${esc(p.gateway6)}</span>` : ""}
      · DNS <span class="mono">${esc((p.dns || []).join(", "))}</span> · pool ${esc(p.pool)}</div>`;
  if (opts.hint) html += `<div class="hint">${opts.hint}</div>`;
  if (isAdmin()) {
    if (!pubPools) pubPools = (await call("GET", "/ippools").catch(() => ({pools: []}))).pools;
    const pool = pubPools.find(x => x.name === p.pool);
    html += `<div class="inline" style="margin-top:6px;flex-wrap:wrap">
      <button class="btn" data-pub="add">+ Extra IPv4</button>
      ${pool?.cidr6 && !p.ip6 ? `<button class="btn" data-pub="ipv6">+ IPv6</button>` : ""}
      <span class="spacer"></span>
      <input data-pub-ip placeholder="new main address (empty = next free)" style="flex:1;min-width:180px" autocomplete="off">
      <select data-pub-pool>${pubPools.map(x => `<option value="${esc(x.name)}"${x.name === p.pool ? " selected" : ""}>${esc(x.name)} (${x.free} free)</option>`).join("")}</select>
      <button class="btn" data-pub="change">Change address</button></div>
      <div class="hint">Changes take effect at once; inside a running ${kind === "vms" ? "VM (with the guest agent)" : "container"} the address is set for you.
        Another pool on another bridge needs it shut down.</div>`;
  }
  el.innerHTML = html;
  for (const b of el.querySelectorAll("[data-pub]")) b.onclick = async () => {
    const body = {action: b.dataset.pub};
    if (body.action === "remove") { body.ip = b.dataset.ip; if (!armed(b, "Remove?")) return; }
    if (body.action === "change") {
      body.ip = el.querySelector("[data-pub-ip]").value.trim(); body.pool = el.querySelector("[data-pub-pool]").value;
      if (body.pool === p.pool && !body.ip && !armed(b, "Next free address — sure?")) return;
    }
    b.disabled = true;
    try {
      const r = await call("POST", `/${kind}/${encodeURIComponent(name)}/public-ips`, body);
      toast(`${name}: ${r.public.ip}${(r.public.extra || []).length ? " + " + r.public.extra.length : ""}${r.public.ip6 ? " · " + r.public.ip6 : ""} — ${r.note}`, !r.applied && /set it inside/.test(r.note));
      pubPools = null;
      if (opts.onChanged) opts.onChanged(r);
    } catch (err) { toast(err.message, true); }
    b.disabled = false;
  };
}
