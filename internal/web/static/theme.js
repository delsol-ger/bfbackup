// Applies the appearance chosen in Settings (light or dark) before the page
// is drawn, so it does not flash in the wrong colors. app.js stores the
// choice here and keeps it in sync with the controller's setting.
try {
  const t = localStorage.getItem('bfb-theme');
  if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
} catch (_) { /* storage blocked: follow the system */ }
