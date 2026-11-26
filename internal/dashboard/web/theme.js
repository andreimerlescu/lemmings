(function () {
  var root = document.documentElement, theme, fx;
  try { theme = localStorage.getItem('lemmings-theme'); fx = localStorage.getItem('lemmings-fx'); } catch (e) {}
  if (theme !== 'light' && theme !== 'dark') {
    theme = window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
  }
  if (fx !== 'on' && fx !== 'off') {
    fx = window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'off' : 'on';
  }
  root.setAttribute('data-theme', theme);
  root.setAttribute('data-fx', fx);
})();
