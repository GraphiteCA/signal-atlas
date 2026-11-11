// Functional GPU preference/namespace/rollback checks on a fresh 64-AP fixture.
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const [port,origin,out]=process.argv.slice(2);if(!/^http:\/\/127\.0\.0\.1:\d+$/.test(origin))throw new Error('Local owned app required');
const pause=ms=>new Promise(r=>setTimeout(r,ms));
(async()=>{
  const tabs=await(await fetch(`http://127.0.0.1:${port}/json/list`)).json(),tab=tabs.find(t=>t.type==='page');
  const ws=new WebSocket(tab.webSocketDebuggerUrl),pending=new Map();let id=0;
  await new Promise(r=>ws.addEventListener('open',r,{once:true}));
  ws.addEventListener('message',e=>{const r=JSON.parse(e.data);if(r.id){const p=pending.get(r.id);pending.delete(r.id);r.error?p.reject(new Error(JSON.stringify(r.error))):p.resolve(r.result);}});
  const cmd=(method,params={})=>new Promise((resolve,reject)=>{pending.set(++id,{resolve,reject});ws.send(JSON.stringify({id,method,params}));});
  async function evaluate(expression){const r=await cmd('Runtime.evaluate',{expression,awaitPromise:true,returnByValue:true});if(r.exceptionDetails)throw new Error(r.exceptionDetails.exception?.description||r.exceptionDetails.text);return r.result.value;}
  async function wait(expression){for(let i=0;i<1200;i++){if(await evaluate(expression))return;await pause(25);}throw new Error('Timed out: '+expression);}
  async function screenshot(name){const r=await cmd('Page.captureScreenshot',{format:'png',captureBeyondViewport:false});fs.writeFileSync(path.join(out,name+'.png'),Buffer.from(r.data,'base64'));}
  async function api(url,body){const r=await fetch(origin+url,body===undefined?{}:{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}),j=await r.json();assert.equal(r.status,200,JSON.stringify(j));return j;}
  async function complete(solver){for(let i=0;i<1200;i++){const s=await api('/api/app/preparation/status');if(s.namespace.solver===solver&&s.status==='complete')return s;assert.notEqual(s.status,'failed',JSON.stringify(s));await pause(25);}throw new Error('Preparation did not complete: '+solver);}
  const report={schema:1,checks:[],success:false};
  try{
    await api('/api/app/preparation/settings',{paused:true});
    await cmd('Page.enable');await cmd('Runtime.enable');await cmd('Page.navigate',{url:origin+'/admin?tab=survey'});
    await wait(`!!document.getElementById('adminGPUFits')&&document.getElementById('adminGPUStatus').textContent.includes('off')`);
    await evaluate(`document.getElementById('adminGPUFits').click()`);
    await wait(`document.getElementById('adminGPUStatus').textContent.includes('GPU ready')`);
    let config=await api('/api/app/settings');assert.equal(config.settings.gpu_fits,true);assert.equal(config.settings.fast_fits,false);assert.equal(config.acceleration.available,true);
    await cmd('Page.reload');await wait(`document.getElementById('adminGPUFits')?.checked&&document.getElementById('adminGPUStatus').textContent.includes('GPU ready')`);
    await evaluate(`document.getElementById('adminGPUFits').closest('article').scrollIntoView({block:'center'})`);await screenshot('phase5-gpu-settings');report.checks.push('Admin GPU opt-in persists and names the physical float64 GPU');
    await cmd('Page.navigate',{url:origin+'/map?lng=-123.3996&lat=48.4003&zoom=15'});
    await wait(`typeof initialized!=='undefined'&&initialized&&fits&&fits.loaded===fits.total`);assert.equal(await evaluate('isFallback'),false);
    await api('/api/app/preparation/settings',{paused:false});await complete('opencl64-reference-v1');
    await wait(`fits?.total===64&&fits.loaded===64`);await screenshot('phase5-gpu-map');report.checks.push('All 64 GPU reference APs display without panning');
    await api('/api/app/settings',{fast_fits:true});await complete('opencl64-fast-v1');await wait(`summarySolver==='opencl64-fast-v1'&&fits?.total===64&&fits.loaded===64`);
    await api('/api/app/settings',{gpu_fits:false});await complete('fast');await wait(`summarySolver==='fast'&&fits?.total===64&&fits.loaded===64`);
    await api('/api/app/settings',{fast_fits:false});await complete('reference');await wait(`summarySolver==='reference'&&fits?.total===64&&fits.loaded===64`);
    report.checks.push('GPU/CPU and reference/fast switches reconcile to four independent completed namespaces');
    await api('/api/app/preparation/settings',{paused:true});
    config=await api('/api/app/settings');report.acceleration=config.acceleration;assert.ok(config.acceleration.calls>0);assert.equal(config.acceleration.fallbacks,0);
    await cmd('Page.navigate',{url:origin+'/admin?tab=survey'});await wait(`!!document.getElementById('adminGPUStatus')?.textContent&&document.getElementById('adminFastFits')`);
    assert.equal(await evaluate(`document.getElementById('adminGPUFits').checked`),false);assert.equal(await evaluate(`document.getElementById('adminFastFits').checked`),false);
    report.checks.push('Turning GPU/fast preferences off restores reference CPU selection');report.success=true;console.log(JSON.stringify(report,null,2));
  }finally{ws.close();}
})().catch(e=>{console.error(e);process.exitCode=1;});
