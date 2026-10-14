"use strict";
/* The Server page as a full page in the content area (like a VM's page), with
   tabs, and its sections as cards in a responsive grid. Loaded after every
   script that adds a section to #dlgNode (node.js, cluster.js, ha.js,
   remote.js, security.js, storages.js, ceph.js). Uses $, esc, markSide. */

const NODE_TABS = [["summary", "Summary"], ["storage", "Storage"], ["templates", "Templates"],
  ["network", "Network"], ["cluster", "Cluster"], ["tasks", "Tasks"]];
let nodeTab = "summary";
try { nodeTab = localStorage.getItem("p8s.nodeTab") || "summary"; } catch {}
if (!NODE_TABS.some(([k]) => k === nodeTab)) nodeTab = "summary";

function nodeTabOf(title){
  const t = title.toLowerCase();
  if (/health|memory|debian|update|power/.test(t)) return "summary";
  if (/storage|ceph|disk|encrypt/.test(t)) return "storage";
  if (/template/.test(t)) return "templates";
  if (/network|remote|public|internet|certificate|https/.test(t)) return "network";
  if (/cluster|high availability/.test(t)) return "cluster";
  if (/task/.test(t)) return "tasks";
  return "summary";
}

(function buildNodePage(){
  const dlg = $("#dlgNode"), body = dlg.querySelector(".dlg-body");
  const head = body.firstElementChild;                 // "Server · name" + Rename / Shell
  const running = $("#ndRunning");
  const actions = body.querySelector(":scope > .dlg-actions");
  head.insertAdjacentHTML("beforebegin", `<div class="page-crumb"><a data-nhome>Datacenter</a> › <span>server</span></div>`);
  running.insertAdjacentHTML("afterend", `<nav class="tabs" role="tablist" id="ndTabs">${NODE_TABS.map(([k, l]) =>
    `<button type="button" role="tab" data-ntab="${k}">${l}</button>`).join("")}</nav><div id="ndPanes"></div>`);
  const panes = $("#ndPanes");
  for (const [k] of NODE_TABS) panes.insertAdjacentHTML("beforeend", `<div class="nd-grid" data-npane="${k}"></div>`);

  // every section (an <h3> with what follows it, or a box that starts with one) becomes a card
  let card = null;
  const kids = [...body.children].filter(el => el !== head && el !== running && el !== actions &&
    el.id !== "ndTabs" && el !== panes && !el.classList.contains("page-crumb"));
  for (const el of kids) {
    const h = el.tagName === "H3" ? el : (el.firstElementChild?.tagName === "H3" ? el.firstElementChild : null);
    if (h || !card) {
      card = document.createElement("section");
      card.className = "nd-card";
      const title = h ? h.textContent : "";
      card.dataset.title = title;
      panes.querySelector(`[data-npane="${nodeTabOf(title)}"]`).appendChild(card);
    }
    card.appendChild(el);
  }
  // wide sections take the whole row
  for (const c of panes.querySelectorAll(".nd-card")) if (/task|remote|cluster|network$|certificate/i.test(c.dataset.title)) c.classList.add("nd-wide");

  // a card whose content is all hidden (e.g. Cluster for a viewer, HA when off) is hidden too
  const sync = () => {
    for (const c of panes.querySelectorAll(".nd-card")) {
      const kids = [...c.children], rest = kids[0]?.tagName === "H3" && kids.length > 1 ? kids.slice(1) : kids;
      const h = rest.every(el => el.hidden);
      if (c.hidden !== h) c.hidden = h; // only on change: setting it fires the observer again
    }
  };
  new MutationObserver(sync).observe(panes, {subtree: true, attributes: true, attributeFilter: ["hidden"]});
  sync();

  $("#ndTabs").addEventListener("click", e => { const b = e.target.closest("[data-ntab]"); if (b) selectNodeTab(b.dataset.ntab); });
  dlg.querySelector("[data-nhome]").onclick = () => $("#ndClose").click();
  selectNodeTab(nodeTab);

  // node.js opens it with showModal(): show it as a page instead
  dlg.showModal = function(){
    for (const d of document.querySelectorAll("dialog.as-page[open]")) if (d !== dlg) d.close();
    dlg.classList.add("as-page");
    if (!dlg.open) dlg.show();
    dlg.scrollTop = 0;
    document.activeElement?.blur?.(); // show() focuses Rename: no ring until the user tabs
    dlg.querySelector(".page-crumb span").textContent = "server " + ($("#hName2")?.textContent || "");
    markServerSide(true);
  };
  dlg.addEventListener("close", () => { dlg.classList.remove("as-page"); markServerSide(false); if (typeof markSide === "function") markSide(); });
})();

function selectNodeTab(k){
  nodeTab = k;
  try { localStorage.setItem("p8s.nodeTab", k); } catch {}
  for (const b of document.querySelectorAll("#ndTabs [data-ntab]")) b.classList.toggle("on", b.dataset.ntab === k);
  for (const p of document.querySelectorAll("#ndPanes [data-npane]")) p.hidden = p.dataset.npane !== k;
}

// open the Server page on one tab (e.g. from a cluster server in the sidebar)
function openNodeTab(k){ selectNodeTab(k); return openNode(); }

function markServerSide(on){
  $("#bNode")?.classList.toggle("on", on);
  if (on) $("#bHome")?.classList.remove("on");
}
