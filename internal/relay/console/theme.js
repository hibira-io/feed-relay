'use strict';
// Runs in <head> so the saved theme applies before the page paints.
try {
  const t = localStorage.getItem('feed-relay-theme');
  if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
} catch { /* storage blocked: follow the system */ }
