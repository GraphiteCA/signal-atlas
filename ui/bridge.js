// ============================================================================
//  Signal Atlas's page bridge, loaded first in the app pages.
//
//  window.SignalAtlasApp gives the pages what only the app can do:
//    pathOf(file)          where a dropped or picked file lives (a page is never told)
//    compare.post(msg)     the compare page's worker messages, to its Go engine session
//    compare.listen(fn)    and its answers back (long-polled)
//    compare.survey()      the open survey's days and imports; compare.addSurvey(choice) adds them as drives
//    saveFile(file, name)  move an export the engine wrote to where the user chooses
//    pickFiles(opts)       the Windows file dialog
//  It sends links to other sites to the user's browser, and points OpenStreetMap tiles at the engine.
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
  const localFetch=window.fetch.bind(window);
  window.fetch=function(input,options={}){
    const url=new URL(typeof input==='string'?input:input.url,location.href);
    if(url.origin===location.origin&&url.pathname.startsWith('/api/')&&info.survey_id&&!['/api/app/info','/api/app/admin/status','/api/app/admin/operation'].includes(url.pathname)){
      const headers=new Headers(input instanceof Request?input.headers:undefined);new Headers(options.headers||{}).forEach((v,k)=>headers.set(k,v));headers.set('X-Survey-ID',info.survey_id);options={...options,headers};
    }
    return localFetch(input,options);
  };

  // Native push notifies; sequence polling reconciles dropped events/reloads.
  const jobListeners=new Set();let jobAfter=0,lastNative=0,jobTimer=null,jobPolling=false,jobStopped=false;
  function jobMessage(event,native=false){
    if(!event)return;
    if(event.kind==='survey-changed'&&event.survey_id!==info.survey_id){if(/^\/(map|admin|sql)(\/|\.html)?$/.test(location.pathname))location.reload();return;}
    if(event.survey_id!==info.survey_id)return;
    if(native)lastNative=Date.now();
    if(event.sequence<=jobAfter&&!event.reconcile)return;
    jobAfter=Math.max(jobAfter,event.sequence||0);
    for(const fn of jobListeners){try{fn(event);}catch(e){console.error(e);}}
  }
  async function pollJobEvents(){
    if(jobStopped||jobPolling||!jobListeners.size)return;jobPolling=true;
    try{await ready;const r=await fetch('/api/app/preparation/events?after='+jobAfter,{cache:'no-store'});if(!r.ok)return;const j=await r.json();
      if(j.reconcile)jobMessage({survey_id:j.survey_id,sequence:j.sequence,reconcile:true,kind:'reconcile'});
      else for(const e of j.events||[])jobMessage(e);
      jobAfter=Math.max(jobAfter,j.sequence||0);
    }catch(e){}finally{jobPolling=false;}
  }
  function listenJobs(fn){jobListeners.add(fn);if(!jobTimer){jobTimer=setInterval(pollJobEvents,400);pollJobEvents();}
    return()=>{jobListeners.delete(fn);if(!jobListeners.size&&jobTimer){clearInterval(jobTimer);jobTimer=null;}};
  }
  ready.then(async()=>{try{await window.SignalAtlasNativeReady;if(window.runtime?.EventsOn)window.runtime.EventsOn('signal-atlas:job',e=>jobMessage(e,true));}catch(e){}});
  addEventListener('pagehide',()=>{jobStopped=true;if(jobTimer)clearInterval(jobTimer);});

  async function pathOf(file) {
    if (!file) return '';
    if (file.path) return file.path;
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
    // the open survey's days and imports, and adding a choice of them as drives (in turn with post)
    survey() { return ready.then(() => fetch('/api/app/compare/survey', { cache: 'no-store' })).then(json); },
    addSurvey(choice) {
      const done = chain.then(async () => {
        await ensureSession();
        return post('/api/app/compare/' + session + '/survey', choice);
      });
      chain = done.catch(() => {});
      return done;
    },
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
	listenJobs,
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
})();
