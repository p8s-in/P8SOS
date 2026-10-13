/* ---------- VM extra (data) disks: Manage → Resources ---------- */
// Uses the dashboard's globals: $, esc, call, toast, armed, refresh, mName.

function renderDisks(v){
  const disks = v.disks || [], stopped = v.state === "stopped";
  const key = v.name + "|" + v.state + "|" + JSON.stringify(disks);
  if ($("#mDisks").dataset.key === key) return; // keep half-confirmed buttons
  $("#mDisks").dataset.key = key;
  const total = disks.reduce((n, d) => n + d.size_gb, 0);
  $("#mDkSub").textContent = disks.length
    ? `${disks.length} extra disk${disks.length > 1 ? "s" : ""}, ${total} GB, next to the ${v.disk_gb} GB system disk.` +
      (stopped ? "" : " Stop the VM to grow or remove one.")
    : "Only the system disk. Add one for data (databases, files) that you can grow or back up with the VM.";
  $("#mDisks").innerHTML = disks.map(d => `<tr data-i="${d.index}">
      <td><b class="mono">${esc(d.device)}</b><div class="sub">disk ${d.index}</div></td>
      <td><input type="number" min="${d.size_gb}" value="${d.size_gb}" style="width:90px" ${stopped ? "" : "disabled"} aria-label="Size of disk ${d.index} (GB)"> GB</td>
      <td style="text-align:right;white-space:nowrap">
        <button class="btn" data-dk="grow" ${stopped ? "" : "disabled"}>Grow</button>
        <button class="btn danger" data-dk="rm" ${stopped ? "" : "disabled"}>Remove</button></td></tr>`).join("");
  $("#mDkAdd").disabled = disks.length >= 8;
}

$("#mDisks").addEventListener("click", async e => {
  const b = e.target.closest("button[data-dk]");
  if (!b) return;
  const tr = b.closest("tr"), i = tr.dataset.i, path = `/vms/${encodeURIComponent(mName)}/disks/${i}`;
  try {
    if (b.dataset.dk === "grow") {
      const gb = +tr.querySelector("input").value;
      b.disabled = true;
      await call("PATCH", path, {size_gb: gb});
      toast(`disk ${i} is now ${gb} GB — grow the partition inside the VM to use it`);
    } else {
      if (!armed(b, "Delete its data — sure?")) return;
      b.disabled = true;
      await call("DELETE", path);
      toast(`disk ${i} removed`);
    }
  } catch (err) { toast(err.message, true); b.disabled = false; }
  $("#mDisks").dataset.key = "";
  await refresh();
});

$("#mDkAdd").onclick = async () => {
  const gb = +$("#mDkSize").value, go = $("#mDkAdd");
  if (!(gb >= 1)) return toast("Size must be at least 1 GB", true);
  go.disabled = true; go.textContent = "Adding…";
  try {
    const r = await call("POST", `/vms/${encodeURIComponent(mName)}/disks`, {size_gb: gb});
    toast(r.note || `disk added`);
  } catch (err) { toast(err.message, true); }
  go.textContent = "Add disk"; go.disabled = false;
  $("#mDisks").dataset.key = "";
  await refresh();
};
