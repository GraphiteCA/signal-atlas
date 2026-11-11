// Browser integration/visual check of our own app in an isolated Chrome profile.
// Usage: node benchmarks/map-smoke.cjs <local CDP port> <app origin> <new output dir>
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
  async function evaluate(expression){const r=await cmd('Runtime.evaluate',{expression,awaitPromise:true,returnByValue:true});if(r.exceptionDetails)throw new Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text);return r.result.value;}
  async function wait(expression){for(let i=0;i<300;i++){if(await evaluate(expression))return;await pause(100);}throw new Error('Timed out: '+expression);}
  const settled=`typeof initialized!=='undefined'&&initialized&&fits&&fits.loaded===fits.total&&!document.getElementById('status').textContent.includes('loading')`;
  async function screenshot(name){const r=await cmd('Page.captureScreenshot',{format:'png',captureBeyondViewport:false});fs.writeFileSync(path.join(out,name+'.png'),Buffer.from(r.data,'base64'));}
  const result={schema:1,checks:[],screenshots:[]};
  let original;
  try{
    await cmd('Page.enable');await cmd('Runtime.enable');await cmd('Network.enable');
    await cmd('Page.navigate',{url:origin+'/map?lng=-123.399&lat=48.401&zoom=15'});await wait(settled);
    original=await evaluate(`json('/api/app/map/display')`);
    assert.equal(await evaluate('isFallback'),false);assert.equal(await evaluate('fits.total'),50008);
    result.checks.push('GPU initialized with all 50,008 AP identities');
    await screenshot('gpu-clusters');result.screenshots.push('gpu-clusters.png');
    await evaluate(`showList(fits.features.find(r=>r.count>1))`);assert.match(await evaluate(`$('listProgress').textContent`),/^20 of /);
    await evaluate(`$('apList').firstElementChild.click()`);await wait(`!!document.getElementById('pinAP')`);
    const selected=await evaluate(`$('detailBody').querySelector('h2').textContent`);
    await evaluate(`$('pinAP').click()`);await wait(`pins.size===1&&!!document.getElementById('centerAP')`);await wait(settled);
    await evaluate(`$('radius').value='50';$('radius').dispatchEvent(new Event('change'));$('labelZoom').value='2';$('labelZoom').dispatchEvent(new Event('change'));$('centerAP').click()`);
    await wait(`map.getZoom()>=18&&!map.isMoving()`);await wait(settled);await pause(300);
    await screenshot('gpu-rings-and-pin');result.screenshots.push('gpu-rings-and-pin.png');
    result.checks.push('Cluster member details, pin layer, radius and label controls');
    const analysis=await evaluate(`$('analyzeAP').href`);
    // Keep reference auto-fit jobs out of this display/selection smoke test.
    await cmd('Network.setBlockedURLs',{urls:['*/api/estimate?*','*/api/area-estimate?*','https://unpkg.com/*']});
    await cmd('Page.navigate',{url:analysis});await wait(`!!window.signalExplorer&&document.documentElement.dataset.saSelectionApplied==='true'`);
    assert.equal(await evaluate(`window.signalExplorer.getState().ssids.size`),1);
    assert.equal(await evaluate(`Array.from(window.signalExplorer.getState().ssids)[0]`),selected==='(hidden SSID)'?'':selected);
    result.checks.push('GPU to reference analysis preserves the selected SSID');
    await cmd('Network.setBlockedURLs',{urls:['*/tiles/osm/*','https://unpkg.com/*']});
    await cmd('Page.navigate',{url:origin+'/map?renderer=canvas&lng=-123.399&lat=48.401&zoom=15'});await wait(settled);
    assert.equal(await evaluate('isFallback'),true);assert.equal(await evaluate('fits.total'),50008);
    result.checks.push('Canvas fallback loads exact APs with raster tiles and CDN blocked');
    await screenshot('canvas-offline');result.screenshots.push('canvas-offline.png');
    // The GPU path also has to initialize without waiting for raster tile load.
    await cmd('Page.navigate',{url:origin+'/map?lng=-123.399&lat=48.401&zoom=15'});await wait(settled);
    assert.equal(await evaluate('isFallback'),false);assert.equal(await evaluate('fits.total'),50008);
    result.checks.push('GPU initializes with raster tiles and CDN blocked');
    console.log(JSON.stringify(result,null,2));
  }finally{
    if(original)try{await pause(350);await evaluate(`fetch('/api/app/map/display',{method:'POST',headers:{'Content-Type':'application/json'},body:${JSON.stringify(JSON.stringify(original))}}).then(r=>r.json())`);}catch(e){}
    ws.close();
  }
})().catch(e=>{console.error(e);process.exitCode=1;});
