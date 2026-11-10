// Loaded only by cmd/nativebench after the unchanged production page scripts.
(async () => {
  const report = {schema: 1, page: location.pathname, user_agent: navigator.userAgent, viewport: [innerWidth, innerHeight], device_pixel_ratio: devicePixelRatio, synthetic_saved_locations: true, errors: [], scenarios: []};
  let finished = false;
  const send = async () => { if (finished) return; finished = true; await fetch('/__bench/report', {method: 'POST', body: JSON.stringify(report)}); };
  const timeout = setTimeout(() => { report.errors.push('native benchmark timed out after 120 s'); send(); }, 120000);
  const sleep = ms => new Promise(r => setTimeout(r, ms));
  const frame = () => new Promise(r => requestAnimationFrame(r));
  const stats = values => { const x = [...values].sort((a,b) => a-b); return {count:x.length,p50_ms:x[Math.ceil(x.length*.5)-1] ?? null,p95_ms:x[Math.ceil(x.length*.95)-1] ?? null,max_ms:x.at(-1) ?? null,runs_ms:[...values]}; };
  const frames = async (move, n=120) => {const v=[];let last=await frame();for(let i=0;i<n;i++){move(i);const t=await frame();v.push(t-last);last=t;}return stats(v);};
  window.addEventListener('error', e => report.errors.push(String(e.message)));
  window.addEventListener('unhandledrejection', e => report.errors.push(String(e.reason)));
  try {
    // Deferred explorer scripts run after this script is parsed. A DOM element
    // named "map" is not the Leaflet/MapLibre instance.
    for (let i=0;i<300 && (typeof map === 'undefined' || !map || typeof map.getCenter!=='function');i++) await sleep(100);
    if (typeof map === 'undefined' || !map || typeof map.getCenter!=='function') throw new Error('map not created; check CDN libraries/WebGL');
    const isGPU = location.pathname === '/map';
    if (isGPU) {
      report.renderer='upstream MapLibre/deck.gl';
      for(let i=0;i<200&&!map.loaded();i++)await sleep(100);
      if(!map.loaded())throw new Error('GPU map did not finish loading');
      for(let i=0;i<300&&document.getElementById('status').textContent.includes('loading');i++)await sleep(100);
      if(document.getElementById('status').textContent.includes('loading'))throw new Error('initial GPU data load did not settle');
      // Preserve every map/renderer event listener. Replace only the app's
      // auto-fetch entrypoint; each controlled scenario calls the saved loader
      // explicitly, so a debounced fetch cannot supersede it.
      const explicitLoad=load;load=async()=>{};clearTimeout(timer);seq++;
      report.pan_policy='controlled movement on loaded buffers; app auto-fetch entrypoint disabled; map and renderer events retained';
      const canvas=map.getCanvas(), gl=canvas.getContext('webgl2') || canvas.getContext('webgl');
      report.webgl=gl?{version:gl.getParameter(gl.VERSION),renderer:gl.getParameter(gl.RENDERER)}:null;
      const rendererInfo=gl?.getExtension('WEBGL_debug_renderer_info');
      if(rendererInfo)report.webgl.unmasked_renderer=gl.getParameter(rendererInfo.UNMASKED_RENDERER_WEBGL);
      map.jumpTo({center:[-123.399,48.401],zoom:15});clearTimeout(timer);
      const originalDraw=draw;
      let drawTimes=[];
      draw=function(){const t=performance.now();const r=originalDraw();drawTimes.push(performance.now()-t);return r;};
      for (const config of [{id:'ap-only',limit:2000000,observations:false},{id:'500k',limit:500000,observations:true},{id:'2m',limit:2000000,observations:true},{id:'8m',limit:8000000,observations:true}]) {
        const limit=String(config.limit);
        document.getElementById('showObs').checked=config.observations;
        document.getElementById('max').value=limit;clearTimeout(timer);drawTimes=[];
        const start=performance.now();await explicitLoad();await frame();await frame();
        if(config.observations&&(!points||points.n>config.limit))throw new Error('scenario returned inconsistent point count');
        const scenario={id:config.id,point_limit:Number(limit),observations_enabled:config.observations,first_usable_ms:performance.now()-start,shown_observations:points?.n,total_observations:points?.total,saved_aps:fits?.count,draw_submit:stats(drawTimes)};
        scenario.pan_frames=await frames(i=>map.jumpTo({center:[-123.399+(i%30)*.000002,48.401],zoom:15}));clearTimeout(timer);
        report.scenarios.push(scenario);
      }
    } else if(window.__donor){
      report.renderer='donor Leaflet single canvas';
      const p=new URLSearchParams({bbox:'-123.401,48.399,-123.390,48.410'});
      const fetchStart=performance.now();
      const payload=await(await fetch('/api/app/map/fits?'+p)).json();
      const rows=payload.lat.map((lat,i)=>({latitude:lat,longitude:payload.lon[i],ssid:payload.ssid[i],mac:payload.mac[i],radio_key:String(i)}));
      report.data_fetch_decode_convert_ms=performance.now()-fetchStart;
      const layer=new SignalAtlasNearbyCanvas({color:()=> '#49e4c4',popup:r=>r.ssid}).addTo(map);
      for(const clustering of [true,false]){
        layer.setDisplayOptions({clustering,label_min_zoom:18});const start=performance.now();layer.setEstimates(rows,10);await frame();await frame();
        const firstUsable=performance.now()-start;
        const vals=[];const draw=layer._draw;layer._draw=function(){const t=performance.now();const r=draw.call(this);vals.push(performance.now()-t);return r;};
        const pacing=await frames(i=>map.setView([48.401,-123.399+(i%30)*.000002],15,{animate:false}));
        layer._draw=draw;report.scenarios.push({clustering,saved_aps:rows.length,data_ready_to_display_ms:firstUsable,pan_frames:pacing,draw_cpu:stats(vals)});
      }
    } else {
      report.renderer='upstream Leaflet explorer';
      await sleep(500);clearTimeout(areaTimer);areaController?.abort();areaSequence++;
      const explicitAreaLoad=loadAreaEstimates;loadAreaEstimates=async()=>{};
      report.pan_policy='controlled movement on loaded AP layers; app AP auto-fetch entrypoint disabled; renderer events retained';
      map.setView([48.401,-123.399],15,{animate:false});clearTimeout(areaTimer);
      const start=performance.now();await explicitAreaLoad();await frame();await frame();
      const initial=performance.now()-start;
      const pacing=await frames(i=>map.setView([48.401,-123.399+(i%30)*.000002],15,{animate:false}));clearTimeout(areaTimer);
      report.scenarios.push({first_usable_ms:initial,shown_saved_aps:areaRows.length,dom_elements:document.querySelectorAll('*').length,pan_frames:pacing});
    }
    report.resources=performance.getEntriesByType('resource').map(r=>({name:r.name,duration_ms:r.duration,transfer_bytes:r.transferSize}));
    report.note='RAF measures frame pacing, not GPU timer-query execution. Basemaps and library delivery follow the production page. Donor canvas uses bundled Leaflet and the same AP payload; component-only comparison.';
  } catch (e) { report.errors.push(String(e.stack || e)); }
  clearTimeout(timeout);await send();
})();
