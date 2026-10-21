// The theme is chosen before the page paints: the one the viewer picked,
// or the system's. It lives in its own file because the content security
// policy allows no inline script.
"use strict";
(function () {
  let t = null;
  try { t = localStorage.getItem("ci-accelerator-theme"); } catch (e) { /* storage blocked */ }
  if (t !== "dark" && t !== "light") {
    t = window.matchMedia && matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
  }
  document.documentElement.dataset.theme = t;
  try {
    if (localStorage.getItem("ci-accelerator-collapsed") === "true") document.documentElement.dataset.collapsed = "true";
  } catch (e) { /* storage blocked */ }
})();
