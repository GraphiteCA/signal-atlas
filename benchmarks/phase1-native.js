// Phase 1: actual GPU/canvas map, exact counts and display interactions.
(async()=>{
  const report={schema:1,phase:1,errors:[],scenarios:[],synthetic_saved_locations:true};
  const sleep=ms=>new Promise(r=>setTimeout(r,ms)),frame=()=>new Promise(r=>requestAnimationFrame(r));
  const assert=(v,m)=>{if(!v)throw new Error(m);};
  const stats=xs=>{const s=[...xs].sort((a,b)=>a-b);return {count:s.length,p50_ms:s[Math.ceil(s.length*.5)-1],p95_ms:s[Math.ceil(s.length*.95)-1],max_ms:s.at(-1)};};
  const errors=e=>report.errors.push(String(e.message || e.reason || e));
  addEventListener('error',errors);addEventListener('unhandledrejection',errors);
  const timeout=setTimeout(()=>{report.errors.push('Timed out after 120 seconds');send();},120000);
  let sent=false;
  async function send(){if(sent)return;sent=true;await fetch('/__bench/report',{method:'POST',body:JSON.stringify(report)});}
  try{
    for(let i=0;i<450&&(typeof initialized==='undefined'||!initialized||$('status').textContent.includes('loading'));i++)await sleep(100);
    assert(initialized,'Map did not initialize');
    report.renderer=isFallback?'Leaflet single canvas fallback':'MapLibre/deck.gl GPU';
    report.fallback_reason=window.mapFallbackReason || null;
    if(new URLSearchParams(location.search).get('renderer')!=='canvas')assert(!isFallback,'Expected GPU renderer: '+report.fallback_reason);
    if(!isFallback){const gl=map.getCanvas().getContext('webgl2'),ext=gl.getExtension('WEBGL_debug_renderer_info');report.webgl={version:gl.getParameter(gl.VERSION),renderer:ext?gl.getParameter(ext.UNMASKED_RENDERER_WEBGL):gl.getParameter(gl.RENDERER)};}
    report.initial_counts={located:$('located').textContent,observed:$('observed').textContent,stored:$('stored').textContent};
    report.viewport=[innerWidth,innerHeight];
    // Let startup settle outside the measured scenarios.
    await sleep(1000);
    const explicitLoad=load;load=async()=>{};clearTimeout(timer);controller?.abort();seq++;
    report.pan_policy='Loaded data; only app auto-fetch entrypoint disabled. Map and renderer events remain active.';
    const jump=(zoom=15,x=-123.399)=>isFallback?map.setView([48.401,x],zoom,{animate:false}):map.jumpTo({center:[x,48.401],zoom});
    async function scenario(name,cluster,labels=18,obs=false,limit=2000000){
      display.clustering=cluster;display.label_min_zoom=labels;display.rings=true;applyDisplay();$('showObs').checked=obs;$('max').value=limit;jump();clearTimeout(timer);
      const start=performance.now();await explicitLoad();await frame();await frame();
      assert(fits.loaded===fits.total,'Incomplete AP pages');
      if(obs)assert(points&&points.n<=limit,'Observation limit/count mismatch');
      const row={name,clustering:cluster,label_min_zoom:labels,rings:true,first_usable_ms:performance.now()-start,total_aps:fits.total,shown_symbols:fits.features.length,labels:labelRows(allRows()).length,shown_observations:points?.n || 0};
      let last=await frame();const gaps=[];
      for(let i=0;i<120;i++){jump(15,-123.399+(i%30)*.000002);const t=await frame();gaps.push(t-last);last=t;}
      row.pan_frames=stats(gaps);report.scenarios.push(row);clearTimeout(timer);
    }
    await scenario('ap-only-clustered',true);
    const cluster=fits.features.find(r=>r.count>1);assert(cluster,'No clusters on dense fixture');
    await showList(cluster);assert($('apList').children.length===Math.min(cluster.count,20),'Cluster members not accessible');
    report.cluster_members_progress=$('listProgress').textContent;hideDetails();
    await scenario('ap-only-individuals',false);
    const sample=fits.features[0];await showAP(sample.id);assert($('pinAP'),'No AP detail');$('pinAP').click();
    // Pin refresh is asynchronous; its selected layer must survive subsequent pans.
    for(let i=0;i<30&&!pins.size;i++)await sleep(50);assert(pins.size===1,'Pin missing');report.pin_separate=allRows().some(r=>r.pinned);pins.clear();hideDetails();
    await scenario('individuals-any-label-zoom',false,2);
    if(!isFallback){await scenario('500k-sightings',true,18,true,500000);await scenario('2m-sightings',true,18,true,2000000);await scenario('8m-sightings',true,18,true,8000000);}
    // A display preference write must leave saved fits untouched, and survive a
    // second read. Restore the fixture's original preferences after this check.
    const old=await json('/api/app/map/display'),before=await json('/api/app/map/summary');
    await post('/api/app/map/display',{clustering:false,label_min_zoom:2,radius_m:50,rings:true});
    const saved=await json('/api/app/map/display'),after=await json('/api/app/map/summary');
    assert(saved.radius_m===50&&saved.label_min_zoom===2&&!saved.clustering,'Preference round trip');assert(before.revision===after.revision,'Display changed fit revision');await post('/api/app/map/display',old);
    report.preferences_round_trip=true;
    report.resources=performance.getEntriesByType('resource').filter(r=>/\.(js|css)(\?|$)/.test(r.name)).map(r=>r.name);
    assert(report.resources.every(u=>!u.includes('unpkg.com')),'CDN script dependency');
    report.note='RAF gaps measure frame pacing, not GPU timer execution. Pans reuse loaded buffers. Exact backend API latency is measured separately.';
  }catch(e){report.errors.push(String(e.stack || e));}
  clearTimeout(timeout);await send();
})();
