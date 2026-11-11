(async()=>{
  const sleep=ms=>new Promise(r=>setTimeout(r,ms));
  const wait=async(fn,timeout=30000)=>{const end=Date.now()+timeout;while(Date.now()<end){if(await fn())return;await sleep(25);}throw new Error('Native preparation timeout');};
  const post=(path,body)=>fetch(path,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}).then(async r=>{const v=await r.json();if(!r.ok)throw new Error(JSON.stringify(v));return v;});
  const report={schema:1,native_events:0,bridge_events:0,delivery_ms:[],success:false};
  try{
    await SignalAtlasApp.ready;
    await window.SignalAtlasNativeReady;
    await post('/api/app/preparation/settings',{paused:true});
    if(!window.runtime?.EventsOn)throw new Error('Wails native EventsOn unavailable');
    // Block the polling adapter: this probe must use real Wails delivery.
    const realFetch=window.fetch;window.fetch=(url,options)=>String(url).includes('/preparation/events')?Promise.resolve(new Response('{}',{status:503})):realFetch(url,options);
    let firstCommit=0;
    window.runtime.EventsOn('signal-atlas:job',e=>{if(e.kind==='fits-committed'){report.native_events++;report.delivery_ms.push(Date.now()-e.committed_at_ms);firstCommit ||= e.committed_at_ms;}});
    SignalAtlasApp.listenJobs(()=>report.bridge_events++);
    await wait(()=>typeof initialized!=='undefined'&&initialized&&fits&&fits.loaded===fits.total);
    if(isFallback)throw new Error('GPU renderer unavailable');
    await post('/api/app/preparation/settings',{paused:false});
    await wait(()=>fits?.loaded>0&&Number(document.getElementById('located').textContent.replaceAll(',',''))>0);
    report.first_visible_ms=Date.now()-firstCommit;
    await wait(async()=>{const s=await(await fetch('/api/app/preparation/status')).json();return s.status==='complete';});
    await wait(()=>fits?.total===64&&fits.loaded===64);
    report.located_ssids=Number(document.getElementById('located').textContent.replaceAll(',',''));
    if(!report.native_events||!report.bridge_events||report.located_ssids!==64)throw new Error('Native delivery or complete GPU data missing');
    if(Math.max(...report.delivery_ms)>500||report.first_visible_ms>500)throw new Error('Native commit delivery/visibility exceeded 500 ms');
    await post('/api/app/preparation/settings',{paused:true});report.success=true;
  }catch(e){report.error=String(e.stack||e);}
  await fetch('/__bench/report',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(report)});
})();
