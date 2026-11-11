'use strict';
const $ = id => document.getElementById(id);
const fi = n => Number(n || 0).toLocaleString();
const esc = s => String(s ?? '').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const pins = new Map();
let map, overlay, fallbackLayer, isFallback = false, points = null, fits = null;
let seq = 0, timer = null, controller = null, detailController = null, detailSeq = 0;
let display = {...MapDisplay.defaults}, prefRevision = 0, prefLoaded = false, prefSaving = false, prefTimer;
let summaryRevision = '', observationRevision='', pointKey = '', pointData = null, initialized = false, retryTimer;
const incoming = new URLSearchParams(location.search);
let inheritedBands = incoming.getAll('band'), inheritedDates = incoming.getAll('date');
const textCanvas = document.createElement('canvas'), textContext = textCanvas.getContext('2d'), textWidths = new Map();
textContext.font = '12px sans-serif';
const measure = text => { if (!textWidths.has(text)) { if (textWidths.size > 4096) textWidths.clear(); textWidths.set(text,textContext.measureText(text).width); } return textWidths.get(text); };
const position = r => [r.lon,r.lat];
const clusterRadius=r=>Math.min(27,([64,52,48,44,40][Math.max(0,Math.min(4,Math.floor(map.getZoom()-(isFallback?1:0))-16))]-6)/2,13+Math.log10(r.count)*3);
const clusterTextSize=r=>Math.min(12,Math.max(8,(clusterRadius(r)*2-4)/(fi(r.count).length*.6)));
let rowCache = null, labelCache = null;
const emptyLabels=[];
let rendered=null;
const apColor = r => r.pinned ? [255,201,82] : r.band === '5' ? [159,125,255] : r.band === '6' ? [255,112,157] : [61,224,185];
async function json(url, options = {}) {
  const r = await fetch(url,{cache:'no-store',...options}), j = await r.json();
  if (!r.ok) { const e = new Error(j.error || r.statusText); e.status=r.status; throw e; } return j;
}
const post = (url,body) => json(url,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body || {})});
function preferencesStatus(text,error=false) { $('preferences').textContent=text; $('preferences').dataset.error=String(error); $('retryPrefs').hidden=!error; }
function applyDisplay() {
  $('clustering').checked=display.clustering; $('labelZoom').value=display.label_min_zoom;
  $('radius').value=display.radius_m; $('rings').checked=display.rings;
}
async function saveDisplay() {
  if (!prefLoaded || prefSaving) return;
  prefSaving=true; const revision=prefRevision, value={...display}; preferencesStatus('Saving…');
  try { await post('/api/app/map/display',value); if (revision===prefRevision) preferencesStatus('Saved with survey'); }
  catch(e) { if(revision===prefRevision)preferencesStatus('Not saved · '+e.message,true); }
  finally { prefSaving=false; if(revision!==prefRevision)scheduleSave(); }
}
function scheduleSave() { clearTimeout(prefTimer); prefTimer=setTimeout(saveDisplay,200); }
async function loadDisplay() {
  try { const d=await json('/api/app/map/display'); if(!prefRevision){display=MapDisplay.normalize(d);applyDisplay();preferencesStatus('Saved with survey');} }
  catch(e) { preferencesStatus('Preferences unavailable · '+e.message,true); }
  finally { prefLoaded=true;if(prefRevision)scheduleSave(); }
}
function changedDisplay(cluster) {
  prefRevision++; display=MapDisplay.normalize({clustering:$('clustering').checked,label_min_zoom:$('labelZoom').value,radius_m:$('radius').value,rings:$('rings').checked});
  applyDisplay();draw();scheduleSave();if(cluster)soon();
  if($('detailRadius'))$('detailRadius').textContent=display.radius_m+' m planning radius · not a confidence interval';
}
function bounds() {
  const b=map.getBounds();return [Math.max(-180,b.getWest()),Math.max(-85,b.getSouth()),Math.min(180,b.getEast()),Math.min(85,b.getNorth())];
}
function query() {
  // Leaflet uses 256 px world tiles; MapLibre uses 512 px. Normalize the
  // clustering scale so badges also stay separated in the canvas fallback.
  const p=new URLSearchParams({bbox:bounds().map(v=>v.toFixed(6)).join(','),min_rssi:$('minRssi').value || '-150',zoom:Math.min(22,Math.max(0,map.getZoom()-(isFallback?1:0))).toFixed(2),clustering:display.clustering?'1':'0'});
  if($('band').value==='none')p.set('none','1');else if($('band').value==='inherited'){for(const band of inheritedBands)p.append('band',band);}else if($('band').value)p.append('band',$('band').value);
  if($('ssid').value.trim())p.set('ssid_query',$('ssid').value.trim());
  if($('dateFrom').value)p.set('date_from',$('dateFrom').value);
  if($('dateTo').value)p.set('date_to',$('dateTo').value);
  for(const date of inheritedDates)p.append('date',date);
  p.set('individual',$('model').value==='shared'?'0':'1');p.set('shared',$('model').value==='individual'?'0':'1');
  for(const id of pins.keys())p.append('pin',id);
  return p;
}
function allRows() {
	const source=$('showFits').checked?fits?.features:null,loaded=fits?.loaded || 0;
	const pinKey=Array.from(pins.values(),p=>p.id+':'+p.lon+':'+p.lat).join('|');
	if(rowCache&&rowCache.source===source&&rowCache.loaded===loaded&&rowCache.pinKey===pinKey)return rowCache.rows;
  const rows=$('showFits').checked ? [...(fits?.features || [])] : [];
  for(const p of pins.values())rows.push({...p,count:1,label:p.ssid,pinned:true});
	rowCache={source,loaded,pinKey,rows,individuals:rows.filter(r=>r.count===1),clusters:rows.filter(r=>r.count>1)};return rows;
}
function labelRows(rows) {
  // Any zoom is represented by the minimum setting, including views below z2.
  if(display.label_min_zoom!==2 && map.getZoom()<display.label_min_zoom)return emptyLabels;
  const node=$('map'),project=p=>isFallback?map.latLngToContainerPoint([p[1],p[0]]):map.project(p),zoom=map.getZoom();
  if(labelCache&&labelCache.rows===rows&&labelCache.zoom===zoom&&labelCache.width===node.clientWidth&&labelCache.height===node.clientHeight){
    const p=project(labelCache.center);if(Math.abs(p.x-node.clientWidth/2)<32&&Math.abs(p.y-node.clientHeight/2)<32)return labelCache.labels;
  }
  const labels=MapDisplay.labels(rows,project,node.clientWidth,node.clientHeight,measure),c=map.getCenter();
  labelCache={rows,zoom,width:node.clientWidth,height:node.clientHeight,center:[c.lng,c.lat],labels};return labels;
}
function draw() {
  if(!map)return;
  $('zoom').textContent='Zoom '+map.getZoom().toFixed(1);
  $('clearPins').hidden=!pins.size;
  if(isFallback){fallbackLayer?.redraw();return;}
  const rows=allRows(), {individuals,clusters}=rowCache, layers=[];
  const labels=labelRows(rows),stamp=[rows,points,$('showObs').checked,labels,display.rings,display.radius_m,Math.floor(map.getZoom())];
  // The overlay already follows MapLibre's camera. Submitting unchanged layers
  // on every move schedules a second expensive draw of multi-million buffers.
  if(rendered&&stamp.every((v,i)=>v===rendered[i]))return;
  rendered=stamp;
  if(points && $('showObs').checked && points.n) {
    // deck.gl treats a new data object as a changed dataset. Retain this object
    // across camera moves so millions of unchanged points stay on the GPU.
    points.data ||= {length:points.n,attributes:{getPosition:{value:points.xy,size:2},getFillColor:{value:points.colors,size:3}}};
    layers.push(new deck.ScatterplotLayer({id:'obs',data:points.data,coordinateSystem:deck.COORDINATE_SYSTEM.LNGLAT_OFFSETS,coordinateOrigin:[points.lon0,points.lat0,0],radiusUnits:'pixels',getRadius:1.6,radiusMinPixels:1,opacity:.7,pickable:false}));
  }
  if(display.rings && individuals.length) {
    for(const casing of [true,false])layers.push(new deck.ScatterplotLayer({id:casing?'ring-casing':'rings',data:individuals,getPosition:position,getRadius:display.radius_m,radiusUnits:'meters',stroked:true,filled:false,getLineColor:casing?[8,30,26,240]:apColor,lineWidthUnits:'pixels',getLineWidth:casing?5:2.5,pickable:false}));
  }
  layers.push(new deck.ScatterplotLayer({id:'fits',data:rows,getPosition:position,getFillColor:r=>r.count>1?[67,213,179]:apColor(r),getLineColor:[8,30,26],stroked:true,lineWidthUnits:'pixels',getLineWidth:2,radiusUnits:'pixels',getRadius:r=>r.count>1?clusterRadius(r):r.pinned?7:5,updateTriggers:{getRadius:[Math.floor(map.getZoom())]},pickable:true,autoHighlight:true,onClick:info=>{if(info.object)pick(info.object);}}));
  layers.push(new deck.TextLayer({id:'cluster-counts',data:clusters,getPosition:position,getText:r=>fi(r.count),getColor:[5,35,27],getSize:clusterTextSize,updateTriggers:{getSize:[Math.floor(map.getZoom())]},fontWeight:'bold',getTextAnchor:'middle',getAlignmentBaseline:'center',pickable:false}));
  layers.push(new deck.TextLayer({id:'ap-labels',data:labels,getPosition:position,getText:r=>r.text,getPixelOffset:[8,0],getTextAnchor:'start',getAlignmentBaseline:'center',getSize:12,getColor:[241,255,250],background:true,getBackgroundColor:[10,31,27,235],backgroundPadding:[4,2],outlineWidth:0,pickable:false}));
  overlay.setProps({layers});
}

function pointColor(v,colors,i) { const t=Math.max(0,Math.min(1,(v+95)/55));colors[i]=40+215*t;colors[i+1]=80+140*Math.sin(t*Math.PI);colors[i+2]=200-140*t; }
async function loadPoints(p,signal) {
  if(!$('showObs').checked)return null;
  const pp=new URLSearchParams(p); for(const k of ['zoom','clustering','pin','shared','individual'])pp.delete(k);pp.set('max',$('max').value);
  const key=pp.toString()+observationRevision;if(key===pointKey && pointData)return pointData;
  const r=await fetch('/api/app/map/points?'+pp,{signal,cache:'no-store'});if(!r.ok)throw new Error((await r.json()).error || r.statusText);
  const buf=await r.arrayBuffer(),n=Number(r.headers.get('X-Shown')),xy=new Float32Array(buf,0,2*n),rssi=new Float32Array(buf,8*n,n),colors=new Uint8Array(3*n);
  for(let i=0;i<n;i++)pointColor(rssi[i],colors,3*i);
  const pt={n,xy,colors,lon0:Number(r.headers.get('X-Origin-Lon')),lat0:Number(r.headers.get('X-Origin-Lat')),total:Number(r.headers.get('X-Total'))};
  if(!signal.aborted){pointKey=key;pointData=pt;}return pt;
}
function statusText(loading=false) {
  const parts=[];
  if(fits)parts.push(`${fi(fits.loaded)} of ${fi(fits.total)} APs · ${fi(fits.features.length)} map symbols${loading?' · loading…':''}`);
  if(points)parts.push(`${fi(points.n)}${points.n<points.total?' of '+fi(points.total):''} sightings (point limit)`);
  if(pins.size)parts.push(`${fi(pins.size)} pinned`);
  $('status').textContent=parts.join(' · ') || (loading?'loading…':'No saved APs in this view. Fit every AP or adjust filters.');
}
async function load() {
  if(!initialized)return;
  const my=++seq;clearTimeout(retryTimer);controller?.abort();controller=new AbortController();const signal=controller.signal,p=query();
  $('status').textContent='loading…';
  // Keep observation and AP work independent: APs appear without waiting for a
  // multi-million-point buffer. Both share cancellation and stale-response guards.
  const pointTask=loadPoints(p,signal).then(pt=>{if(my===seq){points=pt;draw();statusText(true);}return null;},e=>e);
  try {
    fits=$('showFits').checked?{count:0,total:0,loaded:0,features:[]}:null;
    if(fits) {
      let cursor='',revision='',lastDraw=-Infinity;
      do {
        const pp=new URLSearchParams(p);if(cursor)pp.set('cursor',cursor);
        const page=await json('/api/app/map/features?'+pp,{signal});if(my!==seq)return;
        if(revision && revision!==page.revision)throw new Error('Map changed; restart pagination');revision=page.revision;
        fits.features.push(...page.features);fits.total=page.total;fits.count=page.total;fits.loaded+=page.features.reduce((n,r)=>n+r.count,0);
        // Upload progressive buffers at most ten times a second. Re-uploading
        // the entire growing GPU buffer for each small page wastes most of the
        // loading time on dense unclustered surveys.
        if(!page.next || performance.now()-lastDraw>=100){draw();lastDraw=performance.now();}
        statusText(!!page.next);cursor=page.next;
        // Yield so progressive results and cancellation remain interactive.
        if(cursor)await new Promise(r=>requestAnimationFrame(r));
      } while(cursor && !signal.aborted);
    }
    const pointError=await pointTask;if(pointError)throw pointError;
    if(my===seq){draw();statusText();}
  }catch(e){
    // Observe rejection even if AP pagination failed first.
    pointTask.catch(()=>{});
    if(my!==seq||e.name==='AbortError')return;
    $('status').textContent=e.message;
    if(e.status===409||e.message.includes('Map changed;'))retryTimer=setTimeout(load,350);
  }
}
const soon=()=>{if(!initialized)return;clearTimeout(timer);controller?.abort();seq++;timer=setTimeout(load,180);};

function hideDetails(){detailSeq++;detailController?.abort();$('details').hidden=true;}
async function showAP(id) {
  detailController?.abort();detailController=new AbortController();const signal=detailController.signal,my=++detailSeq;
  $('details').hidden=false;$('detailBody').textContent='Loading AP…';
  try {
    const p=query();p.set('id',id);const r=await json('/api/app/map/detail?'+p,{signal});if(my!==detailSeq)return;
    $('detailBody').innerHTML=`<h2>${esc(r.ssid || '(hidden SSID)')}</h2><p>${esc(r.mac)} · ${esc(r.band || 'unknown')} GHz</p><p>${r.shared?'Shared AP estimate':'Individual radio estimate'}<br>${fi(r.used)} observations · residual ${Number(r.rms).toFixed(1)} dB${r.edge?'<br>At the search edge':''}</p><p>${r.lat.toFixed(7)}, ${r.lon.toFixed(7)}</p><p class="muted" id="detailRadius">${display.radius_m} m planning radius · not a confidence interval</p><div class="actions"><button id="pinAP">${pins.has(id)?'Unpin AP':'Pin AP'}</button><button id="centerAP">Center AP</button><a id="analyzeAP" href="/atlas/">Analyze observations</a></div>`;
    $('pinAP').onclick=()=>{if(pins.has(id))pins.delete(id);else pins.set(id,r);draw();soon();showAP(id);};
    $('centerAP').onclick=()=>{if(isFallback)map.setView([r.lat,r.lon],Math.max(map.getZoom(),18));else map.easeTo({center:[r.lon,r.lat],zoom:Math.max(map.getZoom(),18)});};
    // The reference explorer remains available with its fitting/selection tools.
    const analysis=query();for(const key of ['bbox','zoom','clustering','pin'])analysis.delete(key);analysis.set('ssid',r.ssid);analysis.set('mac',r.mac);analysis.set('individual',r.shared?'0':'1');
    $('analyzeAP').href='/atlas/?'+analysis;
  }catch(e){if(my===detailSeq&&e.name!=='AbortError')$('detailBody').textContent=e.message;}
}
function pick(r){
  if(r.count===1){
    if(!r.pinned){const coincident=allRows().filter(a=>a.count===1&&!a.pinned&&Math.abs(a.lon-r.lon)<1e-9&&Math.abs(a.lat-r.lat)<1e-9);if(coincident.length>1)return showList({coincident:true,count:coincident.length,bounds:[r.lon-1e-9,r.lat-1e-9,r.lon+1e-9,r.lat+1e-9]});}
    return showAP(r.id);
  }
  showList(r);
}
async function showList(cluster=null) {
  detailController?.abort();detailController=new AbortController();const signal=detailController.signal,my=++detailSeq;
  const p=query();p.set('clustering','0');p.set('limit','20');if(cluster){if(cluster.coincident)p.set('bbox',cluster.bounds.join(','));else p.set('cell',cluster.id);}
  $('details').hidden=false;$('detailBody').innerHTML='<h2>'+ (cluster?fi(cluster.count)+' nearby APs':'APs in view')+'</h2>'+(cluster?'<button id="zoomCluster">Zoom to cluster</button>':'')+'<p id="listProgress">Loading…</p><div id="apList"></div><button id="moreAPs" hidden>Load more APs</button>';
  if(cluster)$('zoomCluster').onclick=()=>{
    const b=cluster.bounds,z=Math.min(22,map.getZoom()+2);
    if(b[0]===b[2]&&b[1]===b[3]){if(isFallback)map.setView([b[1],b[0]],z);else map.easeTo({center:[b[0],b[1]],zoom:z});}
    else if(isFallback)map.fitBounds([[b[1],b[0]],[b[3],b[2]]],{maxZoom:z});else map.fitBounds([[b[0],b[1]],[b[2],b[3]]],{padding:80,maxZoom:z});
  };
  let shown=0,cursor='';
  async function next(){
    try {
      $('moreAPs').disabled=true;if(cursor)p.set('cursor',cursor);
      const page=await json('/api/app/map/features?'+p,{signal});if(my!==detailSeq)return;
      for(const r of page.features){const button=document.createElement('button');button.className='ap-row';button.textContent=(r.label || '(hidden SSID)')+' · '+(r.band || '?')+' GHz';button.onclick=()=>showAP(r.id);$('apList').appendChild(button);}
      shown+=page.features.length;cursor=page.next;$('listProgress').textContent=`${fi(shown)} of ${fi(page.total)} APs`;$('moreAPs').hidden=!cursor;$('moreAPs').disabled=false;
    }catch(e){if(my===detailSeq&&e.name!=='AbortError')$('listProgress').textContent=e.message;}
  }
  $('moreAPs').onclick=next;await next();
}

function makeFallback() {
  isFallback=true;$('fallback').hidden=false;
  map=L.map('map',{maxZoom:22,preferCanvas:true,zoomControl:false}).setView([20,0],2);
  L.control.zoom({position:'topright'}).addTo(map);
  const tiles=L.tileLayer('/tiles/osm/{z}/{x}/{y}.png',{maxNativeZoom:19,maxZoom:22,attribution:'© OpenStreetMap contributors'}).addTo(map);
  const Canvas=L.Layer.extend({
    onAdd(m){this.canvas=L.DomUtil.create('canvas','sa-canvas',m.getPanes().overlayPane);this.canvas.setAttribute('aria-hidden','true');m.on('move zoom resize',this.redraw,this);m.on('click',this.click,this);this.redraw();},
    redraw(){cancelAnimationFrame(this.frame);this.frame=requestAnimationFrame(()=>this.paint());},
    paint(){
      const size=map.getSize(),ratio=devicePixelRatio||1,c=this.canvas;c.width=size.x*ratio;c.height=size.y*ratio;c.style.width=size.x+'px';c.style.height=size.y+'px';L.DomUtil.setPosition(c,map.containerPointToLayerPoint([0,0]));
      const ctx=c.getContext('2d');ctx.scale(ratio,ratio);const rows=allRows();this.hits=[];
      if(points&&$('showObs').checked)for(let i=0;i<points.n;i++){const p=map.latLngToContainerPoint([points.lat0+points.xy[i*2+1],points.lon0+points.xy[i*2]]);if(p.x<0||p.y<0||p.x>size.x||p.y>size.y)continue;ctx.fillStyle=`rgb(${points.colors.slice(i*3,i*3+3).join(',')})`;ctx.fillRect(p.x,p.y,2,2);}
      for(const r of rows){
        const p=map.latLngToContainerPoint([r.lat,r.lon]);if(p.x< -30||p.y< -30||p.x>size.x+30||p.y>size.y+30)continue;
        const rgb=apColor(r),fill=r.count>1?'#43d5b3':`rgb(${rgb})`,radius=r.count>1?clusterRadius(r):r.pinned?7:5;
        if(display.rings&&r.count===1){const other=map.latLngToContainerPoint([r.lat+display.radius_m/111195,r.lon]);ctx.beginPath();ctx.arc(p.x,p.y,Math.abs(other.y-p.y),0,Math.PI*2);ctx.lineWidth=5;ctx.strokeStyle='#081e1a';ctx.stroke();ctx.lineWidth=2.5;ctx.strokeStyle=fill;ctx.stroke();}
        ctx.beginPath();ctx.arc(p.x,p.y,radius,0,Math.PI*2);ctx.fillStyle=fill;ctx.fill();ctx.lineWidth=2;ctx.strokeStyle='#081e1a';ctx.stroke();
        if(r.count>1){ctx.font='bold '+clusterTextSize(r)+'px sans-serif';ctx.fillStyle='#05231b';ctx.textAlign='center';ctx.textBaseline='middle';ctx.fillText(fi(r.count),p.x,p.y);}
        this.hits.push({r,x:p.x,y:p.y,radius});
      }
      ctx.font='12px sans-serif';ctx.textAlign='left';ctx.textBaseline='middle';
      for(const r of labelRows(rows)){const p=map.latLngToContainerPoint([r.lat,r.lon]),w=measure(r.text);ctx.fillStyle='#0a1f1b';ctx.fillRect(p.x+8,p.y-10,w+8,20);ctx.fillStyle='#f1fffa';ctx.fillText(r.text,p.x+12,p.y);}
    },
    click(e){const p=e.containerPoint;for(let i=this.hits.length-1;i>=0;i--){const h=this.hits[i];if((p.x-h.x)**2+(p.y-h.y)**2<=(h.radius+4)**2){pick(h.r);break;}}}
  });
  fallbackLayer=new Canvas().addTo(map);
  $('basemap').onchange=()=>{tiles.getContainer().style.filter=$('basemap').value==='dark'?'invert(1) hue-rotate(180deg) brightness(.85)':'none';};
}

async function fitSurvey(){const b=await json('/api/app/map/bounds');if(b.empty)return;if(isFallback)map.fitBounds([[b.south,b.west],[b.north,b.east]],{maxZoom:16,padding:[40,40]});else map.fitBounds([[b.west,b.south],[b.east,b.north]],{maxZoom:16,padding:40,duration:0});}
async function refreshSummary(live=false) {
  const s=await json('/api/app/map/summary');$('located').textContent=fi(s.located_ssids);$('observed').textContent=fi(s.observed_ssids);$('stored').textContent=fi(s.observations);
  if(summaryRevision && summaryRevision!==s.revision){
    if(observationRevision!==(s.observation_revision||s.revision)){pointKey='';pointData=null;}hideDetails();
    // Keep pins separate from clustering while retiring fits invalidated by an
    // import or band edit. A background fit must not clear the user's pins.
    for(const id of pins.keys()){
      try{const p=query();p.set('id',id);pins.set(id,await json('/api/app/map/detail?'+p));}
      catch(e){pins.delete(id);}
    }
    if(live)queueLiveLoad();else soon();
  }
  summaryRevision=s.revision;
  observationRevision=s.observation_revision||s.revision;
}
let liveLoadTimer=null,liveLoadRunning=false,liveLoadDirty=false;
function queueLiveLoad(){
  liveLoadDirty=true;if(liveLoadTimer||liveLoadRunning)return;
  liveLoadTimer=setTimeout(async()=>{liveLoadTimer=null;liveLoadDirty=false;liveLoadRunning=true;try{await load();}finally{liveLoadRunning=false;if(liveLoadDirty)queueLiveLoad();}},40);
}
async function pollJob(){
  try { const [j]=await Promise.all([json('/api/job'),refreshSummary()]);const running=j.status==='running';$('fitAll').disabled=running;$('cancelJob').hidden=!running;
    $('job').textContent=running?`${j.kind}: ${fi(j.completed)} / ${fi(j.total)} · ${fi(j.fitted)} fitted`:j.status==='failed'?(j.error || 'Job failed'):'';
  }catch(e){$('job').textContent=e.message;}
}
let preparationState=null,preparationRefresh=null;
async function refreshPreparation(){
  try{const s=await json('/api/app/preparation/status');preparationState=s;const p=s.progress||{},name=s.namespace?.solver||'';
    $('preparation').textContent=s.status==='complete'?`Locations ready · ${fi(p.completed)} networks`:s.status==='running'?`Preparing locations · ${fi(p.completed)} / ${fi(p.total)} · ${name}`:s.status==='failed'?`Preparation: ${s.error}`:s.status==='limited'?`Preparation waiting · ${fi(p.blocked)} networks exceed memory budget`:`Background: ${s.status}`;
    $('pausePreparation').textContent=s.settings?.paused?'Resume background':'Pause background';$('pausePreparation').disabled=!s.settings?.enabled;
  }catch(e){$('preparation').textContent=e.message;}
}
function preparationEvent(){
  if(preparationRefresh)return;
  preparationRefresh=setTimeout(async()=>{try{await refreshSummary(true);await refreshPreparation();}catch(e){}finally{preparationRefresh=null;}},20);
}
async function start(){
  const summaryTask=refreshSummary().catch(e=>{$('job').textContent=e.message;});
  $('labelZoom').innerHTML=Array.from({length:21},(_,i)=>i+2).map(z=>`<option value="${z}">${z===2?'Any zoom':'Zoom '+z}${z===18?' · default':''}</option>`).join('');applyDisplay();
  // Preferences resolve before the first map request; late startup reads cannot
  // overwrite controls the user has already changed.
  for(const id of ['labelZoom','radius','rings'])$(id).onchange=()=>changedDisplay(false);
  $('clustering').onchange=()=>changedDisplay(true);$('retryPrefs').onclick=saveDisplay;
  await loadDisplay();
  for(const [param,id] of [['ssid_query','ssid'],['min_rssi','minRssi'],['date_from','dateFrom'],['date_to','dateTo']])if(incoming.has(param))$(id).value=incoming.get(param);
  if(inheritedBands.length===1)$('band').value=inheritedBands[0];
  else if(inheritedBands.length>1){const option=document.createElement('option');option.value='inherited';option.textContent=inheritedBands.length+' bands from analysis';$('band').appendChild(option);$('band').value='inherited';}
  if(incoming.get('none')==='1')$('band').value='none';
  if(inheritedDates.length){$('dateScope').hidden=false;$('dateScope').textContent=inheritedDates.length+' survey dates ×';}
  $('dateScope').onclick=()=>{inheritedDates=[];$('dateScope').hidden=true;hideDetails();pins.clear();soon();};
  if(incoming.get('individual')==='1')$('model').value=incoming.get('shared')==='1'?'both':'individual';
  try {
    if(new URLSearchParams(location.search).get('renderer')==='canvas'||!window.maplibregl||!window.deck)throw new Error('WebGL unavailable');
    map=new maplibregl.Map({container:'map',center:[0,20],zoom:2,maxZoom:22,maxPitch:0,dragRotate:false,renderWorldCopies:false,style:{version:8,sources:{osm:{type:'raster',tiles:[location.origin+'/tiles/osm/{z}/{x}/{y}.png'],tileSize:256,maxzoom:19,attribution:'© <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors'}},layers:[{id:'bg',type:'background',paint:{'background-color':'#172928'}},{id:'osm',type:'raster',source:'osm'}]}});
    map.addControl(new maplibregl.NavigationControl({showCompass:false}),'top-right');overlay=new deck.MapboxOverlay({interleaved:false,layers:[]});map.addControl(overlay);
    // AP data must not wait for online raster tiles. style.load is sufficient
    // to attach GPU layers and also fires when the basemap cannot be reached.
    await new Promise(resolve=>map.on('style.load',resolve));
    $('basemap').onchange=()=>{const dark=$('basemap').value==='dark';map.setPaintProperty('osm','raster-brightness-min',dark?1:0);map.setPaintProperty('osm','raster-brightness-max',dark?.12:1);map.setPaintProperty('osm','raster-hue-rotate',dark?180:0);};
  }catch(e){window.mapFallbackReason=String(e.stack || e);if(map?.remove)map.remove();makeFallback();$('fallback').title=e.message;}
  // View storage is scoped to this survey directory, rather than a global map.
  const info=await json('/api/app/info');const viewKey='signal-atlas.map.view:'+(info.survey_path||info.data);
  map.on('moveend',()=>{draw();const c=map.getCenter();try{localStorage.setItem(viewKey,JSON.stringify({lng:c.lng,lat:c.lat,zoom:map.getZoom()}));}catch(e){}soon();});
  if(!isFallback)map.on('zoomend',draw);
  let view;try{view=JSON.parse(localStorage.getItem(viewKey)||'null');}catch(e){}
  if(incoming.has('lng')&&incoming.has('lat'))view={lng:Number(incoming.get('lng')),lat:Number(incoming.get('lat')),zoom:Number(incoming.get('zoom') || 15)};
  if(view&&Number.isFinite(view.lng)&&Number.isFinite(view.lat)&&Number.isFinite(view.zoom)){if(isFallback)map.setView([view.lat,view.lng],view.zoom);else map.jumpTo({center:[view.lng,view.lat],zoom:view.zoom});}else await fitSurvey();
  for(const id of ['band','minRssi','dateFrom','dateTo','model'])$(id).onchange=()=>{hideDetails();pins.clear();soon();};
  for(const id of ['dateFrom','dateTo'])$(id).onchange=()=>{inheritedDates=[];$('dateScope').hidden=true;hideDetails();pins.clear();soon();};
  $('ssid').oninput=()=>{hideDetails();pins.clear();soon();};
  for(const id of ['max','showObs','showFits'])$(id).onchange=soon;
  $('fitBounds').onclick=()=>fitSurvey().catch(e=>$('status').textContent=e.message);
  $('browse').onclick=()=>showList();$('clearPins').onclick=()=>{pins.clear();draw();soon();};$('closeDetails').onclick=hideDetails;
  document.addEventListener('keydown',e=>{if(e.key==='Escape')hideDetails();});
  $('fitAll').onclick=async()=>{try{await post('/api/app/fit-all?'+query());pollJob();}catch(e){$('job').textContent=e.message;}};
  $('cancelJob').onclick=()=>post('/api/cancel').then(pollJob).catch(e=>$('job').textContent=e.message);
  $('pausePreparation').onclick=async()=>{try{await post('/api/app/preparation/settings',{paused:!preparationState?.settings?.paused});await refreshPreparation();}catch(e){$('preparation').textContent=e.message;}};
  window.SignalAtlasApp?.listenJobs(preparationEvent);
  initialized=true;await summaryTask;await load();pollJob();refreshPreparation();setInterval(pollJob,4000);setInterval(refreshPreparation,1000);
}
start().catch(e=>$('status').textContent='Map unavailable · '+e.message);
