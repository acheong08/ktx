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

// Log in as soon as a complete signature is pasted.
document.addEventListener("paste", (e) => {
  if (e.target.id !== "signature") return;
  const text = e.clipboardData.getData("text");
  if (!text.includes("-----END SSH SIGNATURE-----")) return;
  e.preventDefault();
  e.target.value = text;
  e.target.form.requestSubmit();
});

// Enter saves a type field. A datalist popup can swallow Enter without
// firing "change", so fire it ourselves. (htmx trigger filters such as
// keyup[key=='Enter'] need eval, which the CSP forbids.)
document.addEventListener("keydown", (e) => {
  if (e.key !== "Enter" || !e.target.matches("[data-enter-commits]")) return;
  e.preventDefault();
  e.target.dispatchEvent(new Event("change", { bubbles: true }));
});

// Login command: fill in the clipboard tool and key path, and remember both.
(() => {
  const variant = document.getElementById("cmd-variant");
  const key = document.getElementById("key-path");
  const cmd = document.getElementById("login-cmd");
  if (!variant || !key || !cmd) return;
  const saved = { variant: localStorage.getItem("login.variant"), key: localStorage.getItem("login.key") };
  if (saved.variant && [...variant.options].some((o) => o.value === saved.variant)) variant.value = saved.variant;
  if (saved.key) key.value = saved.key;
  const render = () => {
    const tmpl = variant.selectedOptions[0].dataset.template;
    cmd.textContent = tmpl.replace("{key}", key.value.trim() || "~/.ssh/id_ed25519");
  };
  variant.addEventListener("change", () => { localStorage.setItem("login.variant", variant.value); render(); });
  key.addEventListener("input", () => { localStorage.setItem("login.key", key.value.trim()); render(); });
  render();
})();
