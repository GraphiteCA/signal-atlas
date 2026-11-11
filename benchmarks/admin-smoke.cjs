// Isolated browser integration test of our own app. Requires an owned legacy fixture.
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const [port,origin,out]=process.argv.slice(2);
if(!/^http:\/\/127\.0\.0\.1:\d+$/.test(origin))throw new Error('Local test app required');
const pause=ms=>new Promise(r=>setTimeout(r,ms));
(async()=>{
  const tabs=await(await fetch(`http://127.0.0.1:${port}/json/list`)).json(),tab=tabs.find(t=>t.type==='page');
  const ws=new WebSocket(tab.webSocketDebuggerUrl),pending=new Map();let id=0;
  await new Promise(resolve=>ws.addEventListener('open',resolve,{once:true}));
  ws.addEventListener('message',e=>{const r=JSON.parse(e.data);if(r.id){const p=pending.get(r.id);pending.delete(r.id);r.error?p.reject(new Error(JSON.stringify(r.error))):p.resolve(r.result);}});
  const cmd=(method,params={})=>new Promise((resolve,reject)=>{pending.set(++id,{resolve,reject});ws.send(JSON.stringify({id,method,params}));});
  async function evaluate(expression){const r=await cmd('Runtime.evaluate',{expression,awaitPromise:true,returnByValue:true});if(r.exceptionDetails)throw new Error(r.exceptionDetails.exception?.description||r.exceptionDetails.text);return r.result.value;}
  async function wait(expression){for(let i=0;i<400;i++){if(await evaluate(expression))return;await pause(100);}throw new Error('Timed out: '+expression);}
  async function api(action,body){return evaluate(`fetch('/api/app/admin/${action}',${body===undefined?"{}":JSON.stringify({method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)})}).then(async r=>{const v=await r.json();if(!r.ok)throw new Error(JSON.stringify(v));return v;})`);}
  async function navigate(query=''){await cmd('Page.navigate',{url:origin+'/admin'+query});await wait(`document.getElementById('adminName')?.textContent!=='Connecting…' && !!window.SignalAtlasApp`);}
  let lastOperation='';
  async function done(){await wait(`document.getElementById('adminOperation')?.dataset.status==='complete' && document.getElementById('adminOperation').dataset.id!==${JSON.stringify(lastOperation)}`);lastOperation=await evaluate(`document.getElementById('adminOperation').dataset.id`);}
  async function screenshot(name){const r=await cmd('Page.captureScreenshot',{format:'png',captureBeyondViewport:false});fs.writeFileSync(path.join(out,name+'.png'),Buffer.from(r.data,'base64'));}
  const checks=[];
  try{
    await cmd('Page.enable');await cmd('Runtime.enable');await navigate();
    const initial=await api('status'),before=initial.database.observations,original=initial.path;lastOperation=initial.operation.id||'';
    assert.ok(before>0);await screenshot('admin-surveys');
    await evaluate(`document.querySelector('[data-tab=maintenance]').click();document.getElementById('adminPreview').click()`);
    await wait(`!document.getElementById('adminPreviewResult').hidden`);
    const preview=await api('preview');assert.ok(preview.rows>0);
    await evaluate(`document.getElementById('adminCleanup').click()`);await wait(`document.getElementById('adminConfirm').open`);await screenshot('admin-cleanup-preview');
    assert.match(await evaluate(`document.getElementById('adminConfirmText').textContent`),/recovery revision/);
    await evaluate(`document.getElementById('adminConfirmApply').click()`);await done();
    let status=await api('status');assert.equal(status.database.observations,before-preview.rows);assert.equal(status.recovery_available,true);
    checks.push('Exact preview, explicit confirmation, staged cleanup and retained recovery');
    await navigate('?tab=imports');
    const csv='MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n02:00:00:00:00:AA,UI-zero,2026-11-01,2412,-60,0,-0,3,WIFI\n02:00:00:00:00:AB,UI-equator,2026-11-01,2412,-60,0,-123.4,3,WIFI\n';
    await evaluate(`(()=>{const file=new File([${JSON.stringify(csv)}],'admin-ui.csv',{type:'text/csv'});const files=new DataTransfer();files.items.add(file);document.getElementById('adminFiles').files=files.files;document.getElementById('adminFiles').dispatchEvent(new Event('change'));})()`);await done();
    status=await api('status');assert.equal(status.database.observations,before-preview.rows+1);
    await wait(`document.getElementById('adminHistory').textContent.includes('admin-ui.csv')`);await screenshot('admin-import-history');
    checks.push('Browser file import excludes signed zero pair, retains equator coordinate, records history');
    await navigate();const backup=path.join(out,'ui-snapshot.atlas');
    await evaluate(`document.getElementById('adminSnapshotPath').value=${JSON.stringify(backup)};document.getElementById('adminSave').click()`);await done();
    assert.ok(fs.existsSync(path.join(backup,'workspace.json')));checks.push('Complete workspace snapshot from UI');
    const fresh=path.join(out,'new-survey.atlas');
    await evaluate(`document.getElementById('adminWorkspacePath').value=${JSON.stringify(fresh)};document.getElementById('adminNewPath').click()`);await done();
    assert.equal((await api('status')).database.observations,0);
    await evaluate(`document.getElementById('adminRecent').value=${JSON.stringify(original)};document.getElementById('adminRecentOpen').click()`);await done();
    assert.equal((await api('status')).database.observations,before-preview.rows+1);checks.push('New survey isolation and recent-survey switching');
    await cmd('Emulation.setDeviceMetricsOverride',{width:390,height:844,deviceScaleFactor:1,mobile:false});await navigate('?tab=maintenance');await screenshot('admin-mobile');
    assert.ok(await evaluate('document.documentElement.scrollWidth<=innerWidth'));checks.push('390px responsive layout without horizontal page overflow');
    const error=await evaluate(`document.getElementById('adminError').textContent`);assert.equal(error,'');
    console.log(JSON.stringify({schema:1,checks,screenshots:['admin-surveys.png','admin-cleanup-preview.png','admin-import-history.png','admin-mobile.png']},null,2));
  }finally{ws.close();}
})().catch(e=>{console.error(e);process.exitCode=1;});
