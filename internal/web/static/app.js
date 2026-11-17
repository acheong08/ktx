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
