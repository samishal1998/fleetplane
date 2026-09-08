// Apply the persisted theme before first paint to avoid a flash. Loaded from
// <head> as a same-origin script so the dashboard's CSP allows it.
(function () {
  var t = localStorage.getItem('fp.theme');
  if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
})();
