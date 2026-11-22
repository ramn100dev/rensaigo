// Lector: modo vertical (tira continua) o paginado, con dirección manga (derecha a izquierda) opcional.
// El progreso se guarda en el servidor; las preferencias de lectura, en localStorage.
(function () {
  const cfg = window.READER, root = document.getElementById('pages'), bar = document.getElementById('pos');
  let mode = localStorage.getItem('rl-mode') || 'vertical';
  let rtl = localStorage.getItem('rl-rtl') === '1';
  let cur = Math.min(cfg.start || 0, cfg.pages - 1), sent = -1, timer = null;

  function save(done) {
    if (cur === sent && !done) return;
    sent = cur;
    const body = new URLSearchParams({ chapter: cfg.id, page: cur, done: done ? 1 : 0 });
    fetch('/api/progress', { method: 'POST', body, keepalive: true });
  }
  function setPos() {
    bar.textContent = (cur + 1) + ' / ' + cfg.pages;
    clearTimeout(timer);
    timer = setTimeout(() => save(cur >= cfg.pages - 1), 800);
  }
  function img(i, lazy) {
    const el = document.createElement('img');
    el.src = '/page/' + cfg.id + '/' + i; el.dataset.i = i; el.alt = 'Página ' + (i + 1);
    if (lazy) el.loading = 'lazy';
    return el;
  }
  function render() {
    root.innerHTML = ''; root.className = mode + (rtl ? ' rtl' : '');
    document.getElementById('mode').textContent = mode === 'vertical' ? 'Vertical' : 'Paginado';
    document.getElementById('rtl').textContent = rtl ? 'Manga (←)' : 'Occidental (→)';
    document.getElementById('rtl').style.display = mode === 'paged' ? '' : 'none';
    if (mode === 'vertical') {
      const io = new IntersectionObserver(es => {
        es.forEach(e => { if (e.isIntersecting) { cur = +e.target.dataset.i; setPos(); } });
      }, { rootMargin: '-45% 0px -45% 0px' });
      for (let i = 0; i < cfg.pages; i++) {
        const el = img(i, Math.abs(i - cur) > 2);
        // reserva de altura para que el salto a la página guardada no baile mientras cargan las anteriores
        el.style.minHeight = '60vh'; el.onload = () => { el.style.minHeight = ''; };
        root.appendChild(el); io.observe(el);
      }
      if (cur > 0) root.children[cur].scrollIntoView();
      // la última página, si es corta, nunca llega al centro de la pantalla: tocar fondo también cuenta
      window.onscroll = () => {
        if (cur < cfg.pages - 1 && window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 80) {
          cur = cfg.pages - 1; setPos();
        }
      };
    } else {
      window.onscroll = null;
      root.appendChild(img(cur, false));
      if (cur + 1 < cfg.pages) { const pre = new Image(); pre.src = '/page/' + cfg.id + '/' + (cur + 1); }
      window.scrollTo(0, 0);
    }
    setPos();
  }
  function go(d) {
    if (mode !== 'paged') return;
    const n = cur + d;
    if (n < 0) { if (cfg.prev) location.href = '/read/' + cfg.prev; return; }
    if (n >= cfg.pages) { save(true); if (cfg.next) location.href = '/read/' + cfg.next; else location.href = '/library/' + cfg.series; return; }
    cur = n; render();
  }
  document.getElementById('mode').onclick = () => { mode = mode === 'vertical' ? 'paged' : 'vertical'; localStorage.setItem('rl-mode', mode); render(); };
  document.getElementById('rtl').onclick = () => { rtl = !rtl; localStorage.setItem('rl-rtl', rtl ? '1' : '0'); render(); };
  root.addEventListener('click', e => {
    if (mode !== 'paged') return;
    const right = e.clientX > window.innerWidth / 2;
    go((right ? 1 : -1) * (rtl ? -1 : 1));
  });
  document.addEventListener('keydown', e => {
    if (e.key === 'ArrowRight') go(rtl ? -1 : 1);
    else if (e.key === 'ArrowLeft') go(rtl ? 1 : -1);
    else if (e.key === ' ' && mode === 'paged') { e.preventDefault(); go(1); }
  });
  // Ir al capítulo siguiente es darlo por leído, aunque se pase rápido.
  document.addEventListener('click', e => {
    const a = e.target.closest('a[href]');
    if (a && cfg.next && a.getAttribute('href') === '/read/' + cfg.next) { cur = cfg.pages - 1; save(true); }
  });
  window.addEventListener('pagehide', () => save(cur >= cfg.pages - 1));
  render();
})();
