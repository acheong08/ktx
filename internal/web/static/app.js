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

// Ctrl/Cmd + Enter in a textarea with data-ctrl-enter-submits submits its form.
document.addEventListener("keydown", (e) => {
  if (e.key !== "Enter" || !(e.ctrlKey || e.metaKey) || !e.target.matches("[data-ctrl-enter-submits]")) return;
  e.preventDefault();
  e.target.form?.requestSubmit();
});

// Drag to reorder: a list with data-sortable="<url>" whose items have
// data-key and a [data-drag-handle]. Pointer events, so touch works too;
// arrow keys on a focused handle move an item by one. After a move, the
// keys are posted to the URL in their new order (task=<key>, repeated).
(() => {
  let drag = null;
  const keyed = (list) => [...list.querySelectorAll(":scope > li[data-key]")];
  const order = (list) => keyed(list).map((li) => li.dataset.key).join("\n");
  const save = async (list) => {
    const body = new URLSearchParams();
    keyed(list).forEach((li) => body.append("task", li.dataset.key));
    try {
      const res = await fetch(list.dataset.sortable, { method: "POST", body, credentials: "same-origin" });
      if (!res.ok) throw new Error("HTTP " + res.status);
    } catch (err) {
      alert("The new order couldn't be saved (" + err.message + "). The page will reload.");
      location.reload();
    }
  };
  // place puts li before the first other item whose middle is below y,
  // staying among the keyed (open, own) items.
  const place = (list, li, y) => {
    const others = keyed(list).filter((o) => o !== li);
    const before = others.find((o) => {
      const r = o.getBoundingClientRect();
      return y < r.top + r.height / 2;
    });
    if (before) {
      if (li.nextElementSibling !== before) before.before(li);
    } else if (others.length && others[others.length - 1].nextElementSibling !== li) {
      others[others.length - 1].after(li);
    }
  };
  document.addEventListener("pointerdown", (e) => {
    const handle = e.target.closest("[data-drag-handle]");
    const li = handle?.closest("li[data-key]");
    const list = li?.closest("[data-sortable]");
    if (!list || e.button > 0) return;
    e.preventDefault();
    try {
      handle.setPointerCapture(e.pointerId); // keep getting moves outside the handle
    } catch {}
    drag = { li, list, start: order(list) };
    li.classList.add("dragging");
  });
  document.addEventListener("pointermove", (e) => {
    if (drag) place(drag.list, drag.li, e.clientY);
  });
  const end = () => {
    if (!drag) return;
    const { li, list, start } = drag;
    drag = null;
    li.classList.remove("dragging");
    if (order(list) !== start) save(list);
  };
  document.addEventListener("pointerup", end);
  document.addEventListener("pointercancel", end);
  document.addEventListener("keydown", (e) => {
    const handle = e.target.closest?.("[data-drag-handle]");
    const li = handle?.closest("li[data-key]");
    const list = li?.closest("[data-sortable]");
    if (!list || (e.key !== "ArrowUp" && e.key !== "ArrowDown")) return;
    e.preventDefault();
    const items = keyed(list);
    const i = items.indexOf(li);
    if (e.key === "ArrowUp" && i > 0) items[i - 1].before(li);
    else if (e.key === "ArrowDown" && i < items.length - 1) items[i + 1].after(li);
    else return;
    handle.focus();
    save(list);
  });
})();

// Goal/decision block of the note forms (data-graph-fields).
// The kind select shows the status select for that kind (the other is
// disabled, so it isn't submitted) and the strength.
function syncKind(block) {
  const kind = block.querySelector("[data-kind-select]")?.value || "";
  block.dataset.kind = kind;
  block.querySelectorAll("[data-status-for]").forEach((el) => {
    const on = el.dataset.statusFor === kind;
    el.hidden = !on;
    el.querySelector("select").disabled = !on;
  });
  block.querySelectorAll(".node-only:not([data-status-for])").forEach((el) => (el.hidden = kind === ""));
}
document.addEventListener("change", (e) => {
  if (e.target.matches("[data-kind-select]")) syncKind(e.target.closest("[data-graph-fields]"));
});
// "Based on" picker: a search result adds a row; × removes one; Enter in
// the search box doesn't submit the note.
document.addEventListener("click", (e) => {
  const add = e.target.closest("[data-add-ref]");
  if (add) {
    const block = add.closest("[data-graph-fields]");
    const rows = block.querySelector("[data-ref-rows]");
    const exists = [...rows.querySelectorAll('input[name="ref"]')].some((i) => i.value === add.dataset.addRef);
    if (!exists) {
      const row = block.querySelector("[data-ref-row-template]").content.firstElementChild.cloneNode(true);
      row.querySelector('input[name="ref"]').value = add.dataset.addRef;
      row.querySelector(".ref-label").textContent = add.dataset.refLabel;
      rows.append(row);
      row.querySelector('input[name="quote"]').focus();
    }
    add.parentElement.replaceChildren();
    const search = block.querySelector("[data-ref-search]");
    if (search) search.value = "";
    return;
  }
  const remove = e.target.closest("[data-remove-row]");
  if (remove) remove.closest(".ref-row").remove();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && e.target.matches("[data-ref-search]")) e.preventDefault();
});
