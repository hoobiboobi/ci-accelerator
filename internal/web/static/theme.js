// The theme is chosen before the page paints: the one the viewer picked,
// else dark, as on the project's site. It lives in its own file because
// the content security policy allows no inline script.
"use strict";
(function () {
  let t = null;
  try { t = localStorage.getItem("ci-accelerator-theme"); } catch (e) { /* storage blocked */ }
  if (t !== "dark" && t !== "light") t = "dark";
  document.documentElement.dataset.theme = t;
  try {
    if (localStorage.getItem("ci-accelerator-collapsed") === "true") document.documentElement.dataset.collapsed = "true";
  } catch (e) { /* storage blocked */ }

  // Times are shown in the reader's zone: the server renders them in the
  // zone of the "tz" cookie and names it in data-tz. When the browser's
  // zone differs (the first visit, or the reader travelled), the cookie is
  // set and the page loads once more before it paints anything, so no time
  // ever shows in the wrong zone. Once per zone per session: a zone the
  // server does not know, or cookies or storage blocked, leave UTC (which
  // the pages then say) instead of reloading again.
  const shown = document.documentElement.dataset.tz;
  let tz = "";
  try { tz = Intl.DateTimeFormat().resolvedOptions().timeZone || ""; } catch (e) { /* no Intl */ }
  if (shown === undefined || !tz || tz === shown) return;
  document.cookie = "tz=" + encodeURIComponent(tz) + "; path=/; max-age=31536000; samesite=lax";
  if (document.cookie.split("; ").indexOf("tz=" + encodeURIComponent(tz)) < 0) return;
  try {
    if (sessionStorage.getItem("ci-accelerator-tz-reload") === tz) return;
    sessionStorage.setItem("ci-accelerator-tz-reload", tz);
  } catch (e) { return; }
  location.reload();
})();
