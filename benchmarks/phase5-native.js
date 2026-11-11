// Real GPU map + actual OpenCL preparation. No synthetic saved locations.
(async()=>{
  const sleep=ms=>new Promise(r=>setTimeout(r,ms)),frame=()=>new Promise(r=>requestAnimationFrame(r));
  const wait=async(fn,ms=45000)=>{const end=Date.now()+ms;while(Date.now()<end){if(await fn())return;await sleep(25);}throw new Error('Phase 5 native timeout');};
  const json=async(path,body)=>{const r=await fetch(path,body===undefined?{}:{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}),v=await r.json();if(!r.ok)throw new Error(JSON.stringify(v));return v;};
  const report={schema:1,success:false,synthetic_saved_locations:false,auto_fetch_retained:true,native_events:0,errors:[]};
  window.addEventListener('error',e=>report.errors.push(e.message));
  try{
    await SignalAtlasApp.ready;await window.SignalAtlasNativeReady;
    await json('/api/app/preparation/settings',{paused:true,workers:8});
    const start=performance.now(),settings=await json('/api/app/settings',{fast_fits:true,gpu_fits:true});
    report.gpu_enable_ms=performance.now()-start;
    if(!settings.acceleration.available)throw new Error('Physical OpenCL GPU unavailable: '+JSON.stringify(settings.acceleration));
    window.runtime.EventsOn('signal-atlas:job',e=>{if(e.kind==='fits-committed')report.native_events++;});
    await wait(()=>typeof initialized!=='undefined'&&initialized&&fits&&fits.loaded===fits.total);
    if(isFallback)throw new Error('GPU map renderer unavailable');
    $('showObs').checked=true;$('max').value='500000';await load();
    await wait(()=>points?.n>0&&fits.loaded===fits.total);
    report.observations=points.n;
    const originalPoints=points;
    const before=(await json('/api/app/preparation/status')).progress.completed;
    await json('/api/app/preparation/settings',{paused:false});
    await wait(()=>fits?.loaded>0);
    await sleep(500);report.observation_object_retained_before_pan=points===originalPoints;
    const gaps=[];let last=await frame();
    let emptyFrames=0;
    for(let i=0;i<900;i++){
      map.jumpTo({center:[-123.3996+(i%30)*.000002,48.4003],zoom:15});
      const t=await frame();gaps.push(t-last);last=t;
      if(!fits?.loaded)emptyFrames++;
    }
    const state=await json('/api/app/preparation/status');
    report.completed_before=before;report.completed_after=state.progress.completed;
    report.visible_ap_total=fits.total;report.located_ssids=Number($('located').textContent.replaceAll(',',''));
    report.empty_ap_frames=emptyFrames;
    const sorted=[...gaps].sort((a,b)=>a-b);report.frame_gaps={count:gaps.length,p50_ms:sorted[Math.ceil(sorted.length*.5)-1],p95_ms:sorted[Math.ceil(sorted.length*.95)-1],max_ms:sorted.at(-1)};
    const pauseStart=performance.now();await json('/api/app/preparation/settings',{paused:true});report.pause_join_ms=performance.now()-pauseStart;
    await load();await wait(()=>fits?.total>0&&fits.loaded===fits.total);report.settled_ap_total=fits.total;
    report.acceleration=(await json('/api/app/settings')).acceleration;
    if(state.status==='failed'||state.progress.completed<=before||!report.native_events||!report.acceleration.calls||report.acceleration.fallbacks)throw new Error('Background GPU work did not progress cleanly');
    if(!report.observation_object_retained_before_pan)throw new Error('Fit updates rebuilt observation data before camera movement');
    if(emptyFrames>0)throw new Error('Pan refresh blanked existing APs');
    if(report.frame_gaps.p95_ms>40||report.pause_join_ms>1000)throw new Error('Map/pause latency budget exceeded');
    if(report.errors.length)throw new Error(report.errors.join('; '));
    report.success=true;
  }catch(e){report.error=String(e.stack||e);}
  await fetch('/__bench/report',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(report)});
})();
