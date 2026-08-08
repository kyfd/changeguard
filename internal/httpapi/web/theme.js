(() => {
  const KEY = "changeguard-theme";
  const preferDark = () => window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
  const normalize = (v) => (v === "light" || v === "dark" ? v : null);
  const root = document.documentElement;
  const initial = normalize(localStorage.getItem(KEY)) || "light";
  function paint(theme) {
    const next = theme === "dark" ? "dark" : "light";
    root.setAttribute("data-theme", next);
    root.style.colorScheme = next;
    localStorage.setItem(KEY, next);
    const meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.setAttribute("content", next === "dark" ? "#0b1220" : "#f1f5f9");
    document.querySelectorAll("[data-theme-toggle]").forEach((btn) => {
      const dark = next === "dark";
      btn.setAttribute("aria-label", dark ? "切换为亮色模式" : "切换为暗色模式");
      btn.innerHTML = dark
        ? '<i data-lucide="sun" aria-hidden="true"></i>'
        : '<i data-lucide="moon" aria-hidden="true"></i>';
    });
    if (window.lucide) window.lucide.createIcons({ attrs: { "stroke-width": 1.75, "aria-hidden": "true" } });
    return next;
  }
  window.ChangeGuardTheme = {
    get() { return root.getAttribute("data-theme") === "dark" ? "dark" : "light"; },
    set: paint,
    toggle() { return paint(this.get() === "dark" ? "light" : "dark"); }
  };
  paint(initial);
})();
