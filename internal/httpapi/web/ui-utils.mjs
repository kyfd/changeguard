const focusableSelector = [
  "a[href]", "button:not([disabled])", "input:not([disabled]):not([type='hidden'])",
  "select:not([disabled])", "textarea:not([disabled])", "[tabindex]:not([tabindex='-1'])"
].join(",");

const modalState = new WeakMap();

function focusableElements(modal) {
  return [...modal.querySelectorAll(focusableSelector)].filter(element =>
    !element.hidden && element.getAttribute("aria-hidden") !== "true" && element.getClientRects().length > 0
  );
}

function setModalOpen(modal, open) {
  if (!modal) return;
  if (open) {
    if (modal.classList.contains("open")) return;
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const onKeydown = event => {
      if (event.key !== "Tab") return;
      const elements = focusableElements(modal);
      if (!elements.length) {
        event.preventDefault();
        modal.querySelector("[role='dialog']")?.focus();
        return;
      }
      const first = elements[0];
      const last = elements[elements.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };
    modalState.set(modal, { opener, onKeydown });
    modal.classList.add("open");
    modal.setAttribute("aria-hidden", "false");
    document.addEventListener("keydown", onKeydown);
    requestAnimationFrame(() => {
      const target = focusableElements(modal)[0] || modal.querySelector("[role='dialog']");
      if (target) {
        if (!target.hasAttribute("tabindex") && target.matches("[role='dialog']")) target.tabIndex = -1;
        target.focus({ preventScroll: true });
      }
    });
  } else {
    if (!modal.classList.contains("open")) return;
    const current = modalState.get(modal);
    if (current) document.removeEventListener("keydown", current.onKeydown);
    modalState.delete(modal);
    modal.classList.remove("open");
    modal.setAttribute("aria-hidden", "true");
    if (current?.opener?.isConnected) current.opener.focus({ preventScroll: true });
  }
  document.body.classList.toggle("modal-open", Boolean(document.querySelector(".modal-layer.open")));
}

function activateOnKeyboard(event) {
  if (event.key !== "Enter" && event.key !== " ") return;
  event.preventDefault();
  event.currentTarget.click();
}

function enableKeyboardActivation(root = document) {
  root.addEventListener("keydown", event => {
    const target = event.target.closest?.("[data-open-change], [data-route]");
    if (!target || target.matches("button, a, input, select, textarea")) return;
    activateOnKeyboard({
      key: event.key,
      currentTarget: target,
      preventDefault: () => event.preventDefault()
    });
  });
}

export { activateOnKeyboard, enableKeyboardActivation, setModalOpen };

if (typeof window !== "undefined") {
  window.ChangeGuardUI = { setModalOpen, enableKeyboardActivation };
  await import("./app.js?v=20260829-accessibility");
}
