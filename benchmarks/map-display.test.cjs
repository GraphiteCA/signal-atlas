const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const D=require('../ui/map/display.js');

test('display defaults and bounded radius/label zoom',()=>{
  assert.deepEqual(D.normalize(),D.defaults);
  assert.deepEqual(D.normalize({clustering:false,label_min_zoom:2,radius_m:50,rings:false}),{clustering:false,label_min_zoom:2,radius_m:50,rings:false});
  for(const value of [0,1,23,NaN,Infinity,18.5])assert.equal(D.normalize({label_min_zoom:value}).label_min_zoom,18);
  for(const value of [0,4,51,Infinity,10.5])assert.equal(D.normalize({radius_m:value}).radius_m,10);
});
test('labels avoid other symbols, overlapping labels and viewport edges',()=>{
  const rows=[{id:'a',lon:20,lat:20,label:'Alpha',count:1},{id:'b',lon:24,lat:20,label:'Bravo',count:1},{id:'c',lon:80,lat:80,label:'Charlie',count:1},{id:'cluster',lon:90,lat:20,count:20}];
  const labels=D.labels(rows,([x,y])=>({x,y}),160,100,t=>t.length*6);
  assert.equal(labels.some(r=>r.id==='a'),false);
  assert.equal(labels.some(r=>r.id==='b'),false);
  assert.deepEqual(labels.map(r=>r.id),['c']);
});
test('pins take label priority and input order does not alter placement',()=>{
  const rows=[{id:'a',lon:10,lat:10,label:'wide label',count:1},{id:'b',lon:10,lat:32,label:'wide label',count:1,pinned:true}];
  const draw=rs=>D.labels(rs,([x,y])=>({x,y}),200,100,()=>80).map(r=>r.id);
  assert.deepEqual(draw(rows),draw([...rows].reverse()));assert.equal(draw(rows)[0],'b');
});
test('map and fallback have no CDN script dependency',()=>{
  const html=fs.readFileSync('ui/map.html','utf8');
  for(const match of html.matchAll(/(?:src|href)="([^"]+)"/g))assert.equal(match[1].startsWith('https://unpkg'),false);
  for(const file of ['maplibre/maplibre-gl.js','deck/deck.gl.js','leaflet/leaflet.js'])assert.ok(fs.statSync('ui/vendor/'+file).size>100000);
});

// Exercise the actual controller with a small DOM/renderer seam. Startup waits
// forever at its first preference read so each test controls fetch ordering.
function controller(){
  const nodes=new Map();const node=id=>{if(!nodes.has(id))nodes.set(id,{value:'',checked:false,hidden:false,textContent:'',dataset:{},style:{},clientWidth:800,clientHeight:600});return nodes.get(id);};
  const context={console,performance,Map,Set,URLSearchParams,Float32Array,Uint8Array,AbortController,MapDisplay:D,document:{getElementById:node,createElement:()=>({getContext:()=>({measureText:t=>({width:t.length*6})})})},setTimeout:()=>0,clearTimeout:()=>{},requestAnimationFrame:f=>f(),fetch:()=>new Promise(()=>{}),devicePixelRatio:1,location:{search:""}};
  vm.createContext(context);vm.runInContext(fs.readFileSync('ui/map/map.js','utf8'),context);
  vm.runInContext(`initialized=true;map={getBounds:()=>({getWest:()=>-124,getEast:()=>-123,getSouth:()=>48,getNorth:()=>49}),getZoom:()=>15};globalThis.renderDraw=draw;draw=()=>{};`,context);
  node('showFits').checked=true;node('model').value='shared';node('minRssi').value='-150';
  return {context,node,run:s=>vm.runInContext(s,context)};
}
test('new map loads cancel old pages and discard late responses',async()=>{
  const c=controller(),calls=[];
  c.context.fetch=(url,opts)=>new Promise(resolve=>calls.push({url,opts,resolve}));
  const first=c.run('load()');const second=c.run('load()');assert.equal(calls[0].opts.signal.aborted,true);
  calls[1].resolve({ok:true,json:async()=>({features:[{id:'new',count:1}],total:1,next:'',revision:'2'})});await second;
  calls[0].resolve({ok:true,json:async()=>({features:[{id:'old',count:1}],total:1,next:'',revision:'1'})});await first;
  assert.equal(c.run('fits.features[0].id'),'new');
});

test('SSID and hidden filters refresh sightings independently of AP visibility',async()=>{
  const c=controller(),urls=[];
  c.node('showObs').checked=true;c.node('showFits').checked=false;c.node('ssid').value='Cafe';c.node('hideHidden').checked=true;
  c.context.fetch=async url=>{urls.push(url);return {ok:true,headers:new Map([['X-Shown','1'],['X-Total','1'],['X-Origin-Lon','-123.5'],['X-Origin-Lat','48.5']]),arrayBuffer:async()=>new Float32Array([0,0,-65]).buffer};};
  await c.run('load()');
  const params=new URL( urls[0],'http://localhost').searchParams;
  assert.equal(params.get('ssid_query'),'Cafe');assert.equal(params.get('hide_hidden'),'1');
  assert.equal(c.run('points.n'),1);assert.equal(urls.length,1);
  c.node('ssid').value='Other';c.context.fetch=()=>new Promise(()=>{});c.run('load()');
  assert.equal(c.run('points'),null);
  c.node('hideHidden').checked=false;assert.equal(c.run("query().has('hide_hidden')"),false);
});
test('individual pages accumulate all AP identities and expose loading progress',async()=>{
  const c=controller(),pages=[{features:[{id:'a',count:1}],total:2,next:'next',revision:'r'},{features:[{id:'b',count:1}],total:2,next:'',revision:'r'}];
  c.context.fetch=async()=>({ok:true,json:async()=>pages.shift()});await c.run('load()');
  assert.equal(c.run('fits.loaded'),2);assert.equal(c.run('fits.features.length'),2);assert.match(c.node('status').textContent,/2 APs in view/);
});

test('pan/live refreshes retain APs until a current page replaces them',async()=>{
  const c=controller();c.context.fetch=async()=>({ok:true,json:async()=>({features:[{id:'visible',count:1}],total:1,next:'',revision:'1'})});await c.run('load()');
  let resolve;c.context.fetch=()=>new Promise(r=>{resolve=r;});const pending=c.run('load()');
  assert.equal(c.run('fits.features[0].id'),'visible');
  c.run('soon()');resolve({ok:true,json:async()=>({features:[{id:'cancelled',count:1}],total:1,next:'',revision:'2'})});await pending;
  assert.equal(c.run('fits.features[0].id'),'visible');
  c.context.fetch=async()=>({ok:true,json:async()=>({features:[],total:0,next:'',revision:'3'})});await c.run('load()');assert.equal(c.run('fits.loaded'),0);
});

test('new filters and solver namespaces clear APs before replacement',async()=>{
  const c=controller();c.context.fetch=async()=>({ok:true,json:async()=>({features:[{id:'old',count:1}],total:1,next:'',revision:'1'})});await c.run('load()');
  c.context.fetch=()=>new Promise(()=>{});c.node('ssid').value='different';c.run('load()');assert.equal(c.run('fits.loaded'),0);
  c.run(`fits={loaded:1,features:[{id:'wrong-solver',count:1}]};summarySolver='opencl64-fast-v1';load();`);assert.equal(c.run('fits.loaded'),0);
});
test('new saved fits retain unchanged multi-million-point observation buffers',async()=>{
  const c=controller();let summary={revision:'fit-new',observation_revision:'observations-same',located_ssids:1,observed_ssids:2,observations:10000000};
  c.context.fetch=async()=>({ok:true,json:async()=>summary});
  c.run(`summaryRevision='fit-old';observationRevision='observations-same';pointKey='retained';pointData={n:2000000};hideDetails=()=>{};`);
  await c.run('refreshSummary(true)');assert.equal(c.run('pointKey'),'retained');assert.equal(c.run('pointData.n'),2000000);
  summary={...summary,revision:'new-data',observation_revision:'observations-new'};
  await c.run('refreshSummary(true)');assert.equal(c.run('pointKey'),'');assert.equal(c.run('pointData'),null);
});

test('camera redraws reuse observation and ring data objects on the GPU',()=>{
  const c=controller();class Layer{constructor(props){Object.assign(this,props);}}
  c.context.deck={ScatterplotLayer:Layer,TextLayer:Layer,COORDINATE_SYSTEM:{LNGLAT_OFFSETS:1}};
  c.node('showObs').checked=true;
  c.run(`overlay={setProps:p=>{globalThis.layers=p.layers;}};points={n:1,xy:new Float32Array([0,0]),colors:new Uint8Array([0,0,0]),lon0:0,lat0:0};fits={loaded:1,features:[{id:'a',count:1,lon:0,lat:0}]};renderDraw();globalThis.oldPointData=layers.find(l=>l.id==='obs').data;globalThis.oldRingData=layers.find(l=>l.id==='rings').data;renderDraw();`);
  assert.equal(c.run(`oldPointData===layers.find(l=>l.id==='obs').data`),true);
  assert.equal(c.run(`oldRingData===layers.find(l=>l.id==='rings').data`),true);
  c.run(`points={...points};delete points.data;renderDraw();`);
  assert.equal(c.run(`oldPointData===layers.find(l=>l.id==='obs').data`),false);
});


test('background preparation refresh keeps counts without flashing loading text',async()=>{
  const c=controller();c.context.fetch=async()=>({ok:true,json:async()=>({features:[{id:'cluster',count:12},{id:'one',count:1}],total:13,next:'',revision:'1'})});await c.run('load()');
  const stable=c.node('status').textContent;
  assert.match(stable,/13 APs in view/);assert.match(stable,/1 cluster · 1 individual AP/);assert.doesNotMatch(stable,/symbols|loading/i);
  let resolve;c.context.fetch=()=>new Promise(r=>{resolve=r;});const pending=c.run('load()');
  await Promise.resolve();assert.equal(c.node('status').textContent,stable);
  resolve({ok:true,json:async()=>({features:[{id:'cluster',count:14}],total:14,next:'',revision:'2'})});await pending;
  assert.match(c.node('status').textContent,/14 APs in view/);assert.doesNotMatch(c.node('status').textContent,/loading/i);
});

test('map legend contains no fake nearby-AP count',()=>{
  const html=fs.readFileSync('ui/map.html','utf8');
  assert.doesNotMatch(html,/cluster-key|Individual AP|12<\/span> nearby/);
  assert.match(html,/Planning radius is not a confidence interval/);
});

test('viewport scheduling hints advance without changing observation requests',async()=>{
  const c=controller(),requests=[];
  c.node('showObs').checked=true;
  c.context.fetch=async url=>{
    requests.push(new URL(url,'http://map.local'));
    if(url.startsWith('/api/app/map/points'))return {ok:true,headers:{get:()=> '0'},arrayBuffer:async()=>new ArrayBuffer(0)};
    return {ok:true,json:async()=>({features:[],total:0,next:'',revision:'1'})};
  };
  await c.run('load()');await c.run('load()');
  const features=requests.filter(u=>u.pathname.endsWith('/features'));
  assert.equal(features.length,2);
  assert.ok(features[0].searchParams.get('priority_client'));
  assert.equal(features[0].searchParams.get('priority_client'),features[1].searchParams.get('priority_client'));
  assert.ok(Number(features[1].searchParams.get('priority_seq'))>Number(features[0].searchParams.get('priority_seq')));
  assert.equal(requests.filter(u=>u.pathname.endsWith('/points')).length,1);
  for(const url of requests.filter(u=>!u.pathname.endsWith('/features'))){
    assert.equal(url.searchParams.has('priority_seq'),false);
    assert.equal(url.searchParams.has('priority_client'),false);
  }
});
