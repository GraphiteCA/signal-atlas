// Browser polling fallback and preparation controls on an owned 64-network fixture.
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const [port,origin,out]=process.argv.slice(2);if(!/^http:\/\/127\.0\.0\.1:\d+$/.test(origin))throw new Error('Local test app required');
const pause=ms=>new Promise(r=>setTimeout(r,ms));
(async()=>{
  let r=await fetch(origin+'/api/app/preparation/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:'{"paused":true}'});assert.equal(r.status,200);
  const tabs=await(await fetch(`http://127.0.0.1:${port}/json/list`)).json(),tab=tabs.find(t=>t.type==='page');
  const ws=new WebSocket(tab.webSocketDebuggerUrl),pending=new Map();let id=0;
  await new Promise(resolve=>ws.addEventListener('open',resolve,{once:true}));
  ws.addEventListener('message',e=>{const r=JSON.parse(e.data);if(r.id){const p=pending.get(r.id);pending.delete(r.id);r.error?p.reject(new Error(JSON.stringify(r.error))):p.resolve(r.result);}});
  const cmd=(method,params={})=>new Promise((resolve,reject)=>{pending.set(++id,{resolve,reject});ws.send(JSON.stringify({id,method,params}));});
  async function evaluate(expression){const r=await cmd('Runtime.evaluate',{expression,awaitPromise:true,returnByValue:true});if(r.exceptionDetails)throw new Error(r.exceptionDetails.exception?.description||r.exceptionDetails.text);return r.result.value;}
  async function wait(expression){for(let i=0;i<600;i++){if(await evaluate(expression))return;await pause(25);}throw new Error('Timed out: '+expression);}
  async function screenshot(name){const r=await cmd('Page.captureScreenshot',{format:'png',captureBeyondViewport:false});fs.writeFileSync(path.join(out,name+'.png'),Buffer.from(r.data,'base64'));}
  const report={schema:1,checks:[],success:false};
  try{
    await cmd('Page.enable');await cmd('Runtime.enable');await cmd('Page.navigate',{url:origin+'/map?lng=-123.3996&lat=48.4003&zoom=15'});
    await wait(`typeof initialized!=='undefined'&&initialized&&fits&&fits.loaded===fits.total&&document.getElementById('pausePreparation').textContent==='Resume background'`);
    assert.equal(await evaluate('isFallback'),false);
    await evaluate(`window.prepProbe={deliveries:[],first:0};SignalAtlasApp.listenJobs(e=>{if(e.kind==='fits-committed'){prepProbe.first ||= e.committed_at_ms;prepProbe.deliveries.push(Date.now()-e.committed_at_ms);}});document.getElementById('pausePreparation').click()`);
    await wait(`fits.loaded>0&&Number(document.getElementById('located').textContent.replaceAll(',',''))>0`);
    report.first_visible_ms=await evaluate('Date.now()-prepProbe.first');
    await wait(`fits.total===64&&fits.loaded===64&&document.getElementById('preparation').textContent.includes('Locations ready')`);
    report.delivery_ms=await evaluate('prepProbe.deliveries');assert.ok(report.delivery_ms.length>0);assert.ok(Math.max(...report.delivery_ms)<500);assert.ok(report.first_visible_ms<500);
    report.checks.push('Polling delivers committed changes and visible APs within 500 ms','All 64 AP locations load on the GPU without panning');
    await screenshot('preparation-map');
    await evaluate(`document.getElementById('pausePreparation').click()`);await wait(`document.getElementById('pausePreparation').textContent==='Resume background'`);
    await cmd('Page.navigate',{url:origin+'/admin?tab=survey#background-preparation'});await wait(`!!document.getElementById('adminPrepMode')?.value&&document.getElementById('adminPrepStatus').textContent!=='Connecting…'`);
    await evaluate(`document.getElementById('adminPrepMode').value='all';document.getElementById('adminPrepGrouping').value='both';document.getElementById('adminPrepWorkers').value='1';document.getElementById('adminPrepSave').click()`);
    await wait(`fetch('/api/app/preparation/status').then(r=>r.json()).then(s=>s.settings.paused&&s.settings.fit_mode==='all'&&s.settings.grouping==='both'&&s.settings.workers===1)`);
    await cmd('Page.reload');await wait(`document.getElementById('adminPrepMode')?.value==='all'&&document.getElementById('adminPrepGrouping').value==='both'`);
    await evaluate(`document.getElementById('background-preparation').scrollIntoView({block:'start'})`);await screenshot('preparation-settings');assert.equal(await evaluate(`document.getElementById('adminError').textContent`),'');
    report.checks.push('Pause and survey-specific full/both settings persist across reload');report.success=true;
    console.log(JSON.stringify(report,null,2));
  }finally{ws.close();}
})().catch(e=>{console.error(e);process.exitCode=1;});
