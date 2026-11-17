// Copy buttons: <button data-copy="#selector"> copies that element's text.
document.addEventListener("click", async (e) => {
  const btn = e.target.closest("[data-copy]");
  if (!btn) return;
  const src = document.querySelector(btn.dataset.copy);
  if (!src) return;
  try {
    await navigator.clipboard.writeText(src.textContent.trim());
    const label = btn.textContent;
    btn.textContent = "Copied";
    setTimeout(() => (btn.textContent = label), 1500);
  } catch {
    const range = document.createRange();
    range.selectNodeContents(src);
    getSelection().removeAllRanges();
    getSelection().addRange(range);
  }
});

// Enter saves a type field. A datalist popup can swallow Enter without
// firing "change", so fire it ourselves. (htmx trigger filters such as
// keyup[key=='Enter'] need eval, which the CSP forbids.)
document.addEventListener("keydown", (e) => {
  if (e.key !== "Enter" || !e.target.matches("[data-enter-commits]")) return;
  e.preventDefault();
  e.target.dispatchEvent(new Event("change", { bubbles: true }));
});

// New transcript form: default the time zone to the browser's.
document.querySelectorAll("[data-default-timezone]").forEach((el) => {
  if (!el.value) el.value = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
});

// Bulk selection on the list: a header checkbox (data-select-all="group")
// ticks every row checkbox (data-group="group") shown; the count is shown
// next to the bulk form.
function updateSelection(group) {
  const boxes = [...document.querySelectorAll(`[data-group="${group}"]`)];
  const ticked = boxes.filter((b) => b.checked).length;
  const all = document.querySelector(`[data-select-all="${group}"]`);
  if (all) {
    all.checked = boxes.length > 0 && ticked === boxes.length;
    all.indeterminate = ticked > 0 && ticked < boxes.length;
  }
  document.querySelectorAll("[data-selected-count]").forEach((el) => {
    el.textContent = ticked ? `${ticked} of ${boxes.length} selected` : "";
  });
}
document.addEventListener("change", (e) => {
  const all = e.target.closest("[data-select-all]");
  if (all) {
    document.querySelectorAll(`[data-group="${all.dataset.selectAll}"]`).forEach((b) => (b.checked = all.checked));
    updateSelection(all.dataset.selectAll);
    return;
  }
  const one = e.target.closest("[data-group]");
  if (one) updateSelection(one.dataset.group);
});
