// ============================================================================
//  Signal Atlas's page bridge, loaded first in the compare page and the explorer.
//
//  window.SignalAtlasApp gives the pages what only the app can do:
//    pathOf(file)          where a dropped or picked file lives (a page is never told)
//    compare.post(msg)     the compare page's worker messages, to its Go engine session
//    compare.listen(fn)    and its answers back (long-polled)
//    saveFile(file, name)  move an export the engine wrote to where the user chooses
//    pickFiles(opts)       the Windows file dialog
//  It also adapts Graphite's explorer (CSV imports read in place, the database saved to a file),
//  sends links to other sites to the user's browser, and points OpenStreetMap tiles at the engine.
// ============================================================================
(function () {
  'use strict';
  if (window.SignalAtlasApp) return;
  const json = async (r) => { let j = null; try { j = await r.json(); } catch (e) { /* not JSON */ } if (!r.ok) throw new Error((j && j.error) || ('the engine answered ' + r.status)); return j; };
  const post = (p, body) => fetch(p, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) }).then(json);
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const enc = encodeURIComponent;
  const info = { version: '', data: '', window: false };
  const ready = fetch('/api/app/info', { cache: 'no-store' }).then(json).then((j) => Object.assign(info, j)).catch(() => info);
  const picked = new Map();   // name -> path, for files chosen in a native dialog

  async function pathOf(file) {
    if (!file) return '';
    if (file.path) return file.path;
    if (picked.has(file.name) && file.size === 0) return picked.get(file.name);
    try { return (await json(await fetch('/api/app/path?name=' + enc(file.name) + '&size=' + file.size, { cache: 'no-store' }))).path || ''; }
    catch (e) { return ''; }
  }

  // ---- Wardrive Compare's engine session ----
  let session = null, after = 0, chain = Promise.resolve();
  const listeners = [];
  const emit = (m) => listeners.forEach((fn) => { try { fn(m); } catch (e) { console.error(e); } });
  async function ensureSession() {
    if (!session) { session = (await post('/api/app/compare/new')).id; poll(); }
    return session;
  }
  async function poll() {
    for (;;) {
      try {
        const r = await fetch('/api/app/compare/' + session + '/next?after=' + after, { cache: 'no-store' });
        if (r.status === 404) { emit({ type: 'error', msg: 'the compare engine session ended; reload the page' }); return; }
        const list = await json(r);
        for (const q of list) { after = q.seq; emit(q.msg); }
      } catch (e) { await sleep(1000); }
    }
  }
  async function describe(f) {
    const p = await pathOf(f);
    if (p) return { name: f.name, size: f.size, path: p };
    // a log with no file behind it (the example drives): its bytes. They go as an ArrayBuffer: a File
    // or Blob request body crashes the Linux webview and arrives empty on macOS (wailsapp/wails#6068).
    const j = await json(await fetch('/api/app/compare/' + session + '/upload?name=' + enc(f.name), { method: 'POST', body: await f.arrayBuffer() }));
    return { name: f.name, size: f.size, path: j.path };
  }
  const compare = {
    post(msg) {
      chain = chain.then(async () => {
        await ensureSession();
        let m = msg;
        if (m && m.type === 'add' && m.files) m = Object.assign({}, m, { files: await Promise.all(Array.from(m.files).map(describe)) });
        await post('/api/app/compare/' + session + '/post', m);
      }).catch((e) => emit({ type: 'error', msg: e.message }));
    },
    listen(fn) { listeners.push(fn); },
  };
  addEventListener('pagehide', () => { if (session) navigator.sendBeacon('/api/app/compare/' + session + '/close'); });

  // ---- map tiles: OpenStreetMap refuses an embedded window, so its tiles come through the engine,
  // which identifies the app and caches them (internal/engine/tiles.go) ----
  const osm = /^https?:\/\/(?:[abc]\.)?tile\.openstreetmap\.org\/(\d+)\/(\d+)\/(\d+)\.png/;
  const hookLeaflet = (L) => {
    if (!L || !L.TileLayer || L.TileLayer.prototype.__sa) return;
    const get = L.TileLayer.prototype.getTileUrl;
    L.TileLayer.prototype.getTileUrl = function (c) {
      const u = get.call(this, c), m = osm.exec(u);
      return m ? '/tiles/osm/' + m[1] + '/' + m[2] + '/' + m[3] + '.png' : u;
    };
    L.TileLayer.prototype.__sa = true;
  };
  let leaflet = window.L;
  if (leaflet) hookLeaflet(leaflet);
  else {
    try {
      Object.defineProperty(window, 'L', { configurable: true, enumerable: true, get: () => leaflet, set: (v) => { leaflet = v; try { hookLeaflet(v); } catch (e) { /* not Leaflet */ } } });
    } catch (e) { /* already defined */ }
  }

  window.SignalAtlasApp = {
    get version() { return info.version; },
    get data() { return info.data; },
    pathOf,
    compare,
    saveFile: (file, name) => post('/api/app/save-export', { file, name }),
    pickFiles: (opts) => post('/api/app/pick-files', opts || {}),
    showData: () => post('/api/app/show-data'),
    ready,
  };

  // Links to other sites open in the user's browser, never inside the app.
  document.addEventListener('click', (e) => {
    const a = e.target && e.target.closest && e.target.closest('a[href]');
    if (!a || e.defaultPrevented || !info.window) return;
    const u = new URL(a.getAttribute('href'), location.href);
    if (u.origin !== location.origin) {
      if (/^(https?|mailto):$/.test(u.protocol)) { e.preventDefault(); post('/api/app/open-url', { url: u.href }); }
    } else if (a.target === '_blank' && !a.hasAttribute('download')) { e.preventDefault(); location.href = u.href; }
  });
  ready.then(() => {
    if (!info.window) return;
    const realOpen = window.open;
    window.open = function (url, name, feat) {
      const u = new URL(url || '', location.href);
      if (u.origin !== location.origin) { post('/api/app/open-url', { url: u.href }); return null; }
      return realOpen.call(window, url, name, feat);
    };
  });

  // Files a page makes and "downloads" (a link to a blob, clicked from script) are saved through the
  // system's Save dialog instead: the webviews on macOS and Linux don't download on their own.
  const realClick = HTMLAnchorElement.prototype.click;
  HTMLAnchorElement.prototype.click = function () {
    if (!(info.window && this.hasAttribute('download') && /^blob:/.test(this.href))) return realClick.call(this);
    const name = this.getAttribute('download') || 'export';
    fetch(this.href).then((r) => r.arrayBuffer())
      .then((buf) => fetch('/api/app/save-bytes?name=' + enc(name), { method: 'POST', body: buf }).then(json))
      .catch((err) => alert("Couldn't save " + name + ': ' + err.message));
  };

  const page = location.pathname;
  // Compare: "Choose files" opens the Windows file dialog, so the engine reads the logs in place.
  if (/^\/compare(\.html)?$/.test(page)) {
    document.addEventListener('click', async (e) => {
      const input = e.target && e.target.closest && e.target.closest('input[type=file]#file, label[for=file]');
      if (!input || !info.window) return;
      e.preventDefault();
      const list = await window.SignalAtlasApp.pickFiles({ title: 'Add WiGLE CSV logs or logger databases', exts: ['csv', 'sqlite3', 'sqlite', 'db'], multiple: true }).catch(() => []);
      if (list.length && typeof window.addFiles === 'function') window.addFiles(list);
    }, true);
  }
  // Graphite's explorer: a chosen CSV is imported where it is (no upload, any size), and the database
  // is saved straight to a file the user picks.
  if (/^\/atlas\/?$/.test(page)) {
    // always dark: OpenStreetMap's street tiles get the same night treatment as Compare's map
    // (satellite imagery and the offline styles are left as they are)
    const st = document.createElement('style');
    st.textContent = 'img.leaflet-tile[src^="/tiles/osm/"]{filter:invert(1) hue-rotate(180deg) brightness(.85) contrast(.9)}' +
      '.sa-home{display:inline-block;margin-bottom:10px;font-size:13px;color:#52c2b0;text-decoration:none}.sa-home:hover{text-decoration:underline}' +
      '.database-bar .sa-db-actions{display:flex;gap:8px;align-items:center;flex-wrap:wrap}' +
      'body.sa-dropping::after{content:"Drop a CSV survey or an SQLite database";position:fixed;inset:12px;display:flex;align-items:center;justify-content:center;' +
      'border:2px dashed #52c2b0;border-radius:14px;background:rgba(10,20,18,.82);color:#e8eeea;font-size:20px;z-index:100000;pointer-events:none}';
    document.documentElement.appendChild(st);

    // the map opens on where the user is (or where they left it, or the whole world); never on a
    // fixed place. The view is remembered between visits.
    const VIEW = 'signal-atlas.explorer.view';
    const hookMap = (L) => {
      if (!L || !L.Map || L.Map.__saView) return;
      L.Map.__saView = true;
      L.Map.addInitHook(function () {
        const m = this;
        if (!m.getContainer || m.getContainer().id !== 'map') return;
        let touched = false;
        setTimeout(() => {   // after the page's own setView
          try {
            const v = JSON.parse(localStorage.getItem(VIEW) || 'null');
            if (v && isFinite(v.lat) && isFinite(v.lng)) m.setView([v.lat, v.lng], v.zoom || 15, { animate: false });
          } catch (e) { /* no saved view */ }
          m.on('dragstart zoomstart', () => { touched = true; });
          m.on('moveend', () => { const c = m.getCenter(); localStorage.setItem(VIEW, JSON.stringify({ lat: c.lat, lng: c.lng, zoom: m.getZoom() })); });
          if (navigator.geolocation) {
            navigator.geolocation.getCurrentPosition((p) => {
              if (!touched) m.setView([p.coords.latitude, p.coords.longitude], 16);
            }, () => { /* no location: stay on the saved view or the world */ }, { enableHighAccuracy: false, timeout: 15000, maximumAge: 600000 });
          }
        }, 0);
      });
    };
    if (window.L) hookMap(window.L); else { const prev = Object.getOwnPropertyDescriptor(window, 'L'); if (prev && prev.set) { const set = prev.set; Object.defineProperty(window, 'L', { configurable: true, enumerable: true, get: prev.get, set: (v) => { set(v); try { hookMap(v); } catch (e) { /* not Leaflet */ } } }); } }

    // back to the home page, and SQLite databases from this computer
    const isDb = (name) => /\.(sqlite3?|db)$/i.test(name || '');
    async function openDatabase(path) {
      try {
        const k = await post('/api/app/database-kind', { path });
        if (k.kind === 'atlas' && k.store_observations > 0 &&
            !confirm('Open ' + k.name + '?\n\nIt replaces the ' + k.store_observations.toLocaleString() + ' observations in the explorer now, with their saved estimates and comparisons. ' +
                     'Use "Save SQLite database" first if you want to keep them.')) return;
        const skip = !!(document.getElementById('skip-invalid') || {}).checked;
        await post('/api/app/open-database', { path, skip_invalid: skip });
        if (typeof window.pollJob === 'function') window.pollJob();
      } catch (err) { alert("Couldn't open the database: " + err.message); }
    }
    const addControls = () => {
      const head = document.querySelector('header > div');
      if (head && !document.querySelector('.sa-home')) {
        const a = document.createElement('a');
        a.className = 'sa-home'; a.href = '/'; a.textContent = '← Home';
        head.insertBefore(a, head.firstChild);
      }
      const save = document.getElementById('save-database');
      if (save && !document.querySelector('.sa-db-actions')) {
        const box = document.createElement('div');
        box.className = 'sa-db-actions';
        save.parentNode.insertBefore(box, save);
        const button = (id, text, title) => {
          const b = document.createElement('button');
          b.id = id; b.className = 'button'; b.type = 'button'; b.textContent = text; b.title = title;
          box.appendChild(b);
          return b;
        };
        button('sa-fit-all', 'Fit every AP', 'Fit the location of every network the filters keep, a batch at a time, instead of only those near the map view')
          .addEventListener('click', fitEveryAP);
        if (info.window) {
          button('sa-open-db', 'Open database…', 'Open a Signal Atlas database (replaces what is here), or add a logger database (WiGLE columns) or a WiGLE app backup')
            .addEventListener('click', async () => {
              const list = await window.SignalAtlasApp.pickFiles({ title: 'Open an SQLite database', exts: ['sqlite3', 'sqlite', 'db'], multiple: false }).catch(() => []);
              if (list.length) openDatabase(list[0].path);
            });
        }
        box.appendChild(save);
      }
    };

    // "Fit every AP": one background job over every network, in batches (internal/engine/jobs.go
    // FitAll). Its fits go to the cache the map's nearby fits use, so they appear on the map as the
    // job goes; the map is refreshed every few seconds while it runs.
    let fitAll = null;   // the latest snapshot of a fit-all job this page started
    const fmt = (n) => Number(n || 0).toLocaleString();
    const relabel = () => {
      if (!fitAll) return;
      const t = document.getElementById('job-title'), d = document.getElementById('job-detail');
      const title = 'Fitting every AP';
      const detail = fitAll.status === 'failed' ? (fitAll.error || 'failed') :
        fmt(fitAll.completed) + ' of ' + fmt(fitAll.total) + ' networks · ' + fmt(fitAll.fitted) + ' fitted · ' +
        fmt(fitAll.skipped) + ' too few observations · ' + fmt(fitAll.cached) + ' already fitted · ' + fitAll.status;
      if (t && t.textContent !== title) t.textContent = title;
      if (d && d.textContent !== detail) d.textContent = detail;
    };
    new MutationObserver(relabel).observe(document.documentElement, { subtree: true, childList: true, characterData: true });
    async function followFitAll() {
      let lastRefresh = 0;
      for (;;) {
        try { fitAll = await (await fetch('/api/job', { cache: 'no-store' })).json(); } catch (e) { /* try again */ }
        if (!fitAll || fitAll.kind !== 'fit-all') { fitAll = null; return; }
        relabel();
        const doneNow = fitAll.status !== 'running';
        if (doneNow || Date.now() - lastRefresh > 6000) {
          lastRefresh = Date.now();
          try { if (typeof window.scheduleAreaEstimates === 'function') window.scheduleAreaEstimates(); } catch (e) { /* the page will catch up */ }
          try { if (typeof window.loadDatabaseInfo === 'function') window.loadDatabaseInfo(); } catch (e) { /* ditto */ }
        }
        const b = document.getElementById('sa-fit-all');
        if (b) b.disabled = !doneNow;
        if (doneNow) { setTimeout(() => { fitAll = null; }, 15000); return; }
        await sleep(1000);
      }
    }
    async function fitEveryAP() {
      if (typeof window.areaQuery !== 'function') return;
      const nearby = document.getElementById('nearby-networks');
      if (nearby && !nearby.checked) { nearby.checked = true; nearby.dispatchEvent(new Event('change', { bubbles: true })); }
      try {
        const q = window.areaQuery();
        q.delete('bbox'); q.delete('zoom');
        fitAll = await post('/api/app/fit-all?' + q.toString());
        if (typeof window.pollJob === 'function') window.pollJob();
        followFitAll();
      } catch (err) { alert("Couldn't start fitting: " + err.message); }
    }
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', () => ready.then(addControls)); else ready.then(addControls);

    // drop a CSV survey or a database anywhere on the explorer
    let depth = 0;
    const hasFiles = (e) => e.dataTransfer && Array.from(e.dataTransfer.types || []).includes('Files');
    addEventListener('dragenter', (e) => { if (hasFiles(e)) { depth++; document.body.classList.add('sa-dropping'); } });
    addEventListener('dragleave', (e) => { if (hasFiles(e) && --depth <= 0) { depth = 0; document.body.classList.remove('sa-dropping'); } });
    addEventListener('dragover', (e) => { if (hasFiles(e)) e.preventDefault(); });
    addEventListener('drop', async (e) => {
      if (!hasFiles(e)) return;
      e.preventDefault(); depth = 0; document.body.classList.remove('sa-dropping');
      const f = e.dataTransfer.files[0];
      if (!f) return;
      await sleep(150);   // the window reports a drop's file paths just after the page sees it
      if (isDb(f.name)) {
        const p = await pathOf(f);
        if (p) openDatabase(p); else alert("Couldn't find where " + f.name + ' is on disk; use Open database… instead.');
        return;
      }
      if (/\.csv$/i.test(f.name)) {
        const input = document.getElementById('csv-file'), dt = new DataTransfer();
        dt.items.add(f); input.files = dt.files;
        input.dispatchEvent(new Event('change', { bubbles: true }));
      }
    });
    const open = XMLHttpRequest.prototype.open, send = XMLHttpRequest.prototype.send, setHeader = XMLHttpRequest.prototype.setRequestHeader;
    XMLHttpRequest.prototype.open = function (method, url) { this.__sa = { method: String(method).toUpperCase(), url: String(url) }; return open.apply(this, arguments); };
    XMLHttpRequest.prototype.send = function (body) {
      const w = this.__sa;
      if (!(w && w.method === 'POST' && new URL(w.url, location.href).pathname === '/api/import' && body instanceof File)) return send.apply(this, arguments);
      pathOf(body).then(async (p) => {
        if (p) { setHeader.call(this, 'X-App-Path', enc(p)); send.call(this, null); }
        else send.call(this, await body.arrayBuffer());   // not the File itself: see describe()
      });
    };
    document.addEventListener('click', async (e) => {
      if (!info.window || !e.target || !e.target.closest) return;
      if (e.target.closest('label[for=csv-file]') || e.target.closest('#csv-file')) {
        e.preventDefault();
        const list = await window.SignalAtlasApp.pickFiles({ title: 'Import a CSV survey', exts: ['csv'], multiple: false }).catch(() => []);
        if (!list.length) return;
        const f = new File([], list[0].name, { type: 'text/csv' });
        picked.set(f.name, list[0].path);
        const input = document.getElementById('csv-file'), dt = new DataTransfer();
        dt.items.add(f);
        input.files = dt.files;
        input.dispatchEvent(new Event('change', { bubbles: true }));
        return;
      }
      const save = e.target.closest('#save-database');
      if (save) {
        e.preventDefault();
        const label = save.textContent;
        save.textContent = 'Saving…';
        try { const r = await post('/api/app/save-atlas'); save.textContent = r.saved ? 'Saved ✓' : label; }
        catch (err) { alert("Couldn't save the database: " + err.message); save.textContent = label; }
        setTimeout(() => { save.textContent = label; }, 4000);
      }
    }, true);
  }
})();
