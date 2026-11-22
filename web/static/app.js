// rensaigo: comportamiento de la UI (filtros, acciones de tarjeta, popovers, modal y avisos). Sin dependencias
// aparte de htmx.
(function () {
  'use strict';
  // textos: window.I18N (lo pone la plantilla) traduce el español; L('… %s …', x) sustituye cada %s en orden
  const L = (s, ...a) => { let r = (window.I18N && window.I18N[s]) || s; a.forEach(v => { r = r.replace('%s', v); }); return r; };
  const $ = (s, r = document) => r.querySelector(s);
  const $$ = (s, r = document) => Array.from(r.querySelectorAll(s));
  const store = {
    get(k) { try { return JSON.parse(localStorage.getItem(k)); } catch (e) { return null; } },
    set(k, v) { try { localStorage.setItem(k, JSON.stringify(v)); } catch (e) { /* modo privado */ } },
  };

  // ---------------------------------------------------------------- avisos
  function toast(msg, bad) {
    const box = $('#toasts');
    if (!box || !msg) return;
    // un <dialog> abierto vive en la capa superior y taparía los avisos: mientras lo esté, van dentro de él
    const dlg = $('dialog[open]');
    if ((dlg || document.body) !== box.parentNode) (dlg || document.body).appendChild(box);
    const t = document.createElement('div');
    t.className = 'toast' + (bad ? ' bad' : '');
    t.textContent = msg;
    box.appendChild(t);
    setTimeout(() => t.classList.add('out'), bad ? 7000 : 3500);
    setTimeout(() => t.remove(), bad ? 7400 : 3900);
  }
  document.addEventListener('toast', e => toast(e.detail && e.detail.value));
  // al cerrar el diálogo, los avisos que queden vuelven a la página
  document.addEventListener('close', e => {
    const box = $('#toasts');
    if (box && e.target.tagName === 'DIALOG' && box.parentNode === e.target) document.body.appendChild(box);
  }, true);
  const text = html => { const d = document.createElement('div'); d.innerHTML = html || ''; return d.textContent.trim(); };
  document.addEventListener('htmx:responseError', e => toast(text(e.detail.xhr.responseText) || 'Error ' + e.detail.xhr.status, true));
  document.addEventListener('htmx:sendError', () => toast(L('Sin conexión con el servidor'), true));

  // ---------------------------------------------------------------- modal
  const modal = () => $('#modal');
  function openModal(title, html) {
    const m = modal();
    $('#modal-title').textContent = title || '';
    if (html !== undefined) $('#modal-body').innerHTML = html;
    if (!m.open) m.showModal();
  }
  document.addEventListener('htmx:beforeRequest', e => {
    if (e.detail.target && e.detail.target.id === 'modal-body') {
      // solo al abrirlo desde fuera; lo que se envía desde dentro (guardar, aplicar) no borra el título ni el contenido
      if (e.detail.elt.closest('#modal')) return;
      const src = e.detail.elt.closest('[data-modal]');
      openModal(src ? src.dataset.modal : '', '<p class="loading">' + L('Cargando…') + '</p>');
    }
  });
  document.addEventListener('click', e => {
    if (e.target.closest('[data-close]')) modal().close();
    const add = e.target.closest('[data-open-add]');
    if (add) {
      openModal(L('Añadir serie'), $('#add-tpl').innerHTML);
      htmx.process($('#modal-body'));
      ddEnhance($('#modal-body'));
      const q = $('#modal-body input[name=q]'); if (q) q.focus();
    }
  });
  // clic fuera del cuadro = cerrar
  document.addEventListener('click', e => { const m = modal(); if (m && e.target === m) m.close(); });

  // ---------------------------------------------------------------- popovers de la cabecera
  function closePops(except) { $$('.pop').forEach(p => { if (p !== except) p.hidden = true; }); }
  document.addEventListener('click', e => {
    const b = e.target.closest('[data-pop]');
    if (b) {
      const p = document.getElementById(b.dataset.pop);
      const open = p.hidden;
      closePops(p);
      p.hidden = !open;
      if (open) {
        const r = b.getBoundingClientRect();
        p.style.top = (r.bottom + 8) + 'px';
        p.style.right = Math.max(8, window.innerWidth - r.right) + 'px';
        htmx.trigger(p.querySelector('.pop-b'), 'pop-open');
      }
      return;
    }
    if (!e.target.closest('.pop')) closePops();
  });
  document.addEventListener('keydown', e => { if (e.key === 'Escape') closePops(); });

  // ---------------------------------------------------------------- tarjetas de la biblioteca
  document.addEventListener('click', e => {
    const b = e.target.closest('.card [data-act]');
    if (!b) return;
    e.preventDefault();
    const card = b.closest('.card'), act = b.dataset.act;
    if (act === 'delete' && !confirm(L('¿Eliminar «%s»?\n\nSe BORRA también su carpeta con todos los CBZ. No se puede deshacer.', b.dataset.title))) return;
    b.disabled = true;
    card.classList.add('working');
    htmx.ajax('POST', '/series/' + card.dataset.id + '/' + act, {
      target: card, swap: act === 'delete' ? 'delete' : 'outerHTML', headers: { 'X-Card': '1' },
    }).finally(() => { b.disabled = false; card.classList.remove('working'); applyFilters(); });
  });

  // ---------------------------------------------------------------- desplegables
  // Cada <select> se sustituye por un botón y una lista con el estilo de la app (la lista nativa del navegador sale
  // blanca). El <select> sigue en el formulario, oculto: se le cambia el valor y se dispara "change", así htmx y los
  // filtros funcionan igual. data-sq en una opción = cuadradito de color de estado; data-font = se pinta con esa letra.
  let ddOpen = null;
  function optHTML(o) {
    const sq = o.dataset.sq ? '<i class="sq t-' + o.dataset.sq + '"></i>' : '';
    const span = document.createElement('span');
    span.textContent = o.textContent;
    if (o.dataset.font) span.style.fontFamily = '"' + o.value + '"';
    return sq + span.outerHTML;
  }
  function ddSync(sel) {
    if (!sel._dd) return;
    const o = sel.options[sel.selectedIndex];
    sel._dd.querySelector('.v').innerHTML = o ? optHTML(o) : '';
  }
  function ddClose() {
    if (!ddOpen) return;
    ddOpen.list.remove();
    ddOpen.wrap.classList.remove('open');
    ddOpen.btn.setAttribute('aria-expanded', 'false');
    ddOpen = null;
  }
  function ddPick(sel, i) {
    ddClose();
    if (sel.selectedIndex === i) return;
    sel.selectedIndex = i;
    ddSync(sel);
    sel.dispatchEvent(new Event('input', { bubbles: true }));
    sel.dispatchEvent(new Event('change', { bubbles: true }));
  }
  function ddShow(sel) {
    const wrap = sel._dd, btn = wrap.querySelector('.dd-btn');
    ddClose();
    const list = document.createElement('ul');
    list.className = 'dd-list';
    list.setAttribute('role', 'listbox');
    Array.from(sel.options).forEach((o, i) => {
      const li = document.createElement('li');
      li.setAttribute('role', 'option');
      li.setAttribute('aria-selected', i === sel.selectedIndex);
      li.innerHTML = optHTML(o);
      li.addEventListener('click', ev => { ev.stopPropagation(); ddPick(sel, i); btn.focus(); });
      li.addEventListener('mousemove', () => mark(i));
      list.appendChild(li);
    });
    (btn.closest('dialog') || document.body).appendChild(list);
    const r = btn.getBoundingClientRect();
    list.style.minWidth = r.width + 'px';
    list.style.left = Math.min(r.left, window.innerWidth - list.offsetWidth - 8) + 'px';
    const below = window.innerHeight - r.bottom;
    if (below < list.offsetHeight + 12 && r.top > below) list.style.top = (r.top - list.offsetHeight - 6) + 'px';
    else list.style.top = (r.bottom + 6) + 'px';
    wrap.classList.add('open');
    btn.setAttribute('aria-expanded', 'true');
    ddOpen = { sel, wrap, btn, list, i: sel.selectedIndex };
    function mark(i) {
      ddOpen.i = i;
      Array.from(list.children).forEach((li, j) => li.classList.toggle('act', j === i));
    }
    ddOpen.mark = mark;
    mark(sel.selectedIndex);
    const cur = list.children[sel.selectedIndex];
    if (cur) cur.scrollIntoView({ block: 'nearest' });
  }
  function ddEnhance(root) {
    $$('select:not([multiple]):not([data-native])', root).forEach(sel => {
      if (sel._dd) return;
      const wrap = document.createElement('span');
      wrap.className = 'dd';
      wrap.innerHTML = '<button type="button" class="dd-btn" aria-haspopup="listbox" aria-expanded="false"><span class="v"></span></button>';
      const btn = wrap.firstChild;
      btn.disabled = sel.disabled;
      if (sel.getAttribute('aria-label')) btn.setAttribute('aria-label', sel.getAttribute('aria-label'));
      if (sel.title) btn.title = sel.title;
      sel.parentNode.insertBefore(wrap, sel);
      wrap.appendChild(sel);
      sel.classList.add('dd-native');
      sel.tabIndex = -1;
      sel._dd = wrap;
      ddSync(sel);
      btn.addEventListener('click', e => { e.preventDefault(); e.stopPropagation(); if (sel.disabled) return; if (ddOpen && ddOpen.sel === sel) ddClose(); else ddShow(sel); });
      btn.addEventListener('keydown', e => {
        const open = ddOpen && ddOpen.sel === sel;
        if (!open && ['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(e.key)) { e.preventDefault(); ddShow(sel); return; }
        if (!open) return;
        const n = sel.options.length;
        if (e.key === 'ArrowDown') { e.preventDefault(); ddOpen.mark(Math.min(n - 1, ddOpen.i + 1)); ddOpen.list.children[ddOpen.i].scrollIntoView({ block: 'nearest' }); }
        else if (e.key === 'ArrowUp') { e.preventDefault(); ddOpen.mark(Math.max(0, ddOpen.i - 1)); ddOpen.list.children[ddOpen.i].scrollIntoView({ block: 'nearest' }); }
        else if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); ddPick(sel, ddOpen.i); }
        else if (e.key === 'Escape' || e.key === 'Tab') ddClose();
      });
    });
  }
  document.addEventListener('click', e => { if (ddOpen && !e.target.closest('.dd-list')) ddClose(); });
  window.addEventListener('resize', ddClose);
  document.addEventListener('scroll', e => { if (ddOpen && !(e.target.closest && e.target.closest('.dd-list'))) ddClose(); }, true);
  document.addEventListener('change', e => { if (e.target.matches && e.target.matches('select')) ddSync(e.target); });
  document.addEventListener('htmx:load', e => ddEnhance(e.detail.elt));

  // ---------------------------------------------------------------- filtros
  const FKEY = 'rensaigo.filters';
  function applyFilters() {
    const bar = $('#filters');
    if (!bar) return;
    const f = {};
    $$('select[data-f]', bar).forEach(s => { f[s.dataset.f] = s.value; s.classList.toggle('set', !!s.value); });
    store.set(FKEY, f);
    let shown = 0, total = 0;
    $$('#grid .card').forEach(c => {
      total++;
      const d = c.dataset;
      const ok = (!f.reading || (f.reading === 'fav' ? d.fav === '1' : d.reading === f.reading))
        && (!f.track || d.track === f.track)
        && (!f.genre || d.genres.includes('|' + f.genre + '|'))
        && (!f.src || d.srcs.includes('|' + f.src + '|'));
      c.hidden = !ok;
      if (ok) shown++;
    });
    $('#nomatch').hidden = !(total && !shown);
  }
  document.addEventListener('change', e => { if (e.target.matches('#filters select')) applyFilters(); });
  document.addEventListener('DOMContentLoaded', () => {
    const saved = store.get(FKEY) || {};
    $$('#filters select[data-f]').forEach(s => {
      const v = saved[s.dataset.f];
      if (v && Array.from(s.options).some(o => o.value === v)) { s.value = v; ddSync(s); }
    });
    applyFilters();
    ddEnhance(document);
  });

  // Tarjetas en vivo: se recargan al añadir una serie (evento "libchanged", aunque el diálogo siga abierto) y cada
  // pocos segundos mientras alguna tenga capítulos en cola. Solo se cambian las tarjetas que han cambiado.
  let gridBusy = false;
  function refreshGrid() {
    const grid = $('#grid');
    if (!grid || gridBusy) return;
    gridBusy = true;
    fetch('/library/grid').then(r => r.ok ? r.text() : Promise.reject()).then(html => {
      const tpl = document.createElement('template');
      tpl.innerHTML = html;
      const old = new Map($$('.card', grid).map(c => [c.dataset.id, c]));
      const nodes = Array.from(tpl.content.querySelectorAll('.card')).map(n => {
        const o = old.get(n.dataset.id);
        if (!o) return n;
        if (o.classList.contains('working')) return o; // con una acción en marcha manda la tarjeta de la página
        const c = o.cloneNode(true);
        c.hidden = false;
        return c.outerHTML === n.outerHTML ? o : n;
      });
      if (nodes.length === old.size && nodes.every((n, k) => n === grid.children[k])) return;
      grid.replaceChildren(...nodes);
      htmx.process(grid);
      if (nodes.length) $$('main > .blank').forEach(b => { if (b.id !== 'nomatch') b.hidden = true; });
      applyFilters();
    }).catch(() => {}).finally(() => { gridBusy = false; });
  }
  document.addEventListener('libchanged', refreshGrid);
  setInterval(() => {
    // también si la cabecera dice que se está descargando algo: así aparece el contador en tarjetas que no lo tenían
    if (!document.hidden && ($('#grid .card[data-busy]') || $('#hdr .pill.busy'))) refreshGrid();
  }, 4000);

  // ---------------------------------------------------------------- pestañas (diálogo de añadir)
  document.addEventListener('click', e => {
    const t = e.target.closest('[data-tab]');
    if (!t) return;
    const bar = t.parentNode;
    $$('[data-tab]', bar).forEach(b => {
      b.classList.toggle('on', b === t);
      const pane = document.getElementById(b.dataset.tab);
      if (pane) pane.hidden = b !== t;
    });
  });

  // ---------------------------------------------------------------- lista de MyAnimeList: elegir y añadir
  // Marcar una fila lanza su búsqueda en los sources (de dos en dos, para no acaparar las conexiones del navegador);
  // la fila vuelve con los candidatos para elegir. El botón fijo de abajo añade las marcadas que tengan uno elegido.
  const malQ = [];
  let malActive = 0, adding = null;
  function malPump() {
    while (malActive < 2 && malQ.length) {
      const job = malQ.shift(), tb = job.tb;
      if (!tb.isConnected) continue;
      malActive++;
      tb.classList.add('htmx-request', job.cls);
      htmx.ajax('POST', job.url, { source: tb, target: tb, swap: 'outerHTML', values: job.values })
        .catch(() => tb.classList.remove('htmx-request', job.cls))
        .finally(() => { malActive--; if (adding && job.cls === 'adding') adding.done++; malPump(); malCount(); });
    }
  }
  function malSearch(tb) {
    if (tb.dataset.searched !== undefined || tb.classList.contains('htmx-request')) return;
    const f = tb.querySelector('form.malgo');
    if (!f) return;
    tb.classList.add('htmx-request', 'searching');
    malQ.push({ tb, url: f.getAttribute('hx-post'), cls: 'searching', values: { t: f.elements.t.value } });
    malPump();
  }
  function malReady() {
    return $$('#mallist [data-malpick]:checked').map(c => c.closest('tbody')).filter(tb => $('input[type=radio]:checked', tb));
  }
  function malCount() {
    const box = $('#mallist');
    if (!box) return;
    const go = $('[data-malgo]', box), lbl = $('[data-mallbl]', box), hint = $('[data-malhint]', box);
    if (!go) return;
    const ready = malReady().length, searching = $$('tbody.searching', box).length;
    if (adding && adding.done >= adding.total) { toast(adding.total === 1 ? L('Serie añadida') : L('%s series añadidas', adding.total)); adding = null; }
    go.classList.toggle('busy', !!adding);
    if (adding) {
      go.disabled = true; lbl.textContent = L('Añadiendo…') + ' ' + adding.done + '/' + adding.total;
    } else {
      go.disabled = !ready; lbl.textContent = ready ? L('Añadir') + ' ' + ready : L('Añadir');
    }
    const pend = $$('[data-malpick]:checked', box).length - ready - searching;
    hint.textContent = searching ? L('Buscando %s…', searching) : pend > 0 ? L('%s sin source elegido', pend) : '';
  }
  document.addEventListener('change', e => {
    if (e.target.matches('[data-allmal]')) {
      $$('#mallist [data-malpick]').forEach(c => { c.checked = e.target.checked; if (c.checked) malSearch(c.closest('tbody')); });
    } else if (e.target.matches('[data-malpick]')) {
      if (e.target.checked) malSearch(e.target.closest('tbody'));
    } else if (e.target.matches('#mallist input[type=radio]')) {
      const c = $('[data-malpick]', e.target.closest('tbody')); if (c) c.checked = true; // elegir source = marcar la fila
    } else return;
    malCount();
  });
  document.addEventListener('htmx:afterSettle', () => { if ($('#mallist')) malCount(); });
  document.addEventListener('click', e => {
    if (!e.target.closest('[data-malgo]') || adding) return;
    const rows = malReady();
    if (!rows.length) return;
    adding = { total: rows.length, done: 0 };
    rows.forEach(tb => {
      const r = $('input[type=radio]:checked', tb).dataset, f = tb.querySelector('form.malgo');
      malQ.push({ tb, url: tb.dataset.import, cls: 'adding',
        values: { source: r.source, url: r.url, title: r.title, memo: r.memo, thumb: r.thumb, t: f ? f.elements.t.value : '' } });
    });
    const all = $('#mallist [data-allmal]'); if (all) all.checked = false;
    malPump(); malCount();
  });

  // al volver de iniciar sesión en MyAnimeList (/system#conexiones) se reabre el diálogo de Conexiones
  window.addEventListener('load', () => {
    if (location.hash !== '#conexiones') return;
    history.replaceState(null, '', location.pathname);
    const b = $('#conn-btn'); if (b) b.click();
  });

  // ---------------------------------------------------------------- varios
  document.addEventListener('click', e => { const c = e.target.closest('[data-clamp]'); if (c) c.classList.toggle('clamp'); });
  // ---------------------------------------------------------------- ordenar arrastrando (sources de la serie)
  // Con eventos de puntero: funciona igual con ratón y con el dedo (el drag & drop de HTML5 no va en móvil).
  document.addEventListener('pointerdown', e => {
    const grip = e.target.closest('[data-sort] .grip');
    if (!grip || e.button > 0) return;
    e.preventDefault();
    const li = grip.closest('li'), list = li.parentNode;
    const before = $$('li[data-ss]', list).map(x => x.dataset.ss).join();
    let startY = e.clientY;
    li.classList.add('drag');
    grip.setPointerCapture(e.pointerId);
    const move = ev => {
      const prev = li.previousElementSibling, next = li.nextElementSibling;
      if (prev && ev.clientY < prev.getBoundingClientRect().top + prev.offsetHeight / 2) {
        startY -= prev.offsetHeight; list.insertBefore(li, prev);
      } else if (next && ev.clientY > next.getBoundingClientRect().top + next.offsetHeight / 2) {
        startY += next.offsetHeight; list.insertBefore(next, li);
      }
      li.style.transform = 'translateY(' + (ev.clientY - startY) + 'px)';
    };
    const up = () => {
      grip.removeEventListener('pointermove', move);
      li.classList.remove('drag');
      li.style.transform = '';
      const ids = $$('li[data-ss]', list).map(x => x.dataset.ss);
      if (ids.join() !== before) htmx.ajax('POST', list.dataset.sort, { values: { ss: ids }, swap: 'none' });
    };
    grip.addEventListener('pointermove', move);
    grip.addEventListener('pointerup', up, { once: true });
    grip.addEventListener('pointercancel', up, { once: true });
  });
})();
