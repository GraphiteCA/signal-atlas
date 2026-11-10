// Nearby networks are independent of the detailed checkbox selection.
let areaLayer=null,areaRows=[],areaController=null,areaSequence=0,areaTimer=null,areaSuppressed=null;
const areaMarks=new Map();
let areaDisplayScope=null;
function areaQuery(){
  const p=new URLSearchParams({zoom:map?map.getZoom():0});
  p.set('individual',$('individual-estimates').checked?'1':'0');
  p.set('shared',!$('individual-estimates').checked||$('location-model').value==='both'?'1':'0');
  const box=mapBox();if(box)p.set('bbox',box.join(','));
  if(state.networkQuery)p.set('ssid_query',state.networkQuery);
  if(state.bands.size!==5){if(!state.bands.size)p.set('none','1');for(const b of state.bands)p.append('band',b);}
  p.set('min_rssi',state.minimum);applyObservationDates(p);return p;
}
function visibleAreaEstimates(){
  if(!map||!$('nearby-networks').checked)return [];
  const bounds=map.getBounds();
  const selected=state.estimates.filter(e=>e.status==='fitted');
  const selectedNetworks=new Set(selected.filter(e=>e.ssid).map(e=>e.ssid));
  const selectedKeys=new Set(selected.map(e=>e.radio_key));
  return areaRows.filter(e=>bounds.contains([e.latitude,e.longitude])&&
    !selectedNetworks.has(e.ssid)&&!selectedKeys.has(e.radio_key));
}
function drawAreaEstimates(){
  if(!map)return;
  if(!areaLayer)areaLayer=L.layerGroup().addTo(map);
  const visible=visibleAreaEstimates();
  const keep=new Set(visible.map(e=>e.radio_key));
  for(const [key,mark] of areaMarks){
    if(!keep.has(key)){areaLayer.removeLayer(mark.group);areaMarks.delete(key);}
  }
  updateMapBadge();
  for(const e of visible){
    const signature=JSON.stringify([state.radius,e.latitude,e.longitude,e.fit_mode,e.count,e.used_observations,e.mac,e.band]);
    const previous=areaMarks.get(e.radio_key);
    if(previous?.signature===signature)continue;
    if(previous)areaLayer.removeLayer(previous.group);
    const group=L.layerGroup().addTo(areaLayer);
    areaMarks.set(e.radio_key,{signature,group});
    const tint=networkColor(e.ssid),position=[e.latitude,e.longitude];
    planningCircle(position,{radius:state.radius,color:tint,dashArray:'3 5'},group);
    L.marker(position,{icon:estimateIcon(tint,true),title:e.ssid||'(hidden SSID)'})
      .bindTooltip(esc(e.ssid||'(hidden SSID)'),{permanent:true,opacity:1,direction:'top',offset:[0,-16],className:'ssid-label'})
      .bindPopup(`<b>${esc(e.ssid||'(hidden SSID)')}</b><br>${esc(e.mac)} · ${band(e.band)}<br>${e.co_location_assumed?'Shared AP estimate':'Single-radio estimate'} · ${e.fit_mode==='all'?'all matching observations':'fast sample'}<br>${number(e.used_observations)} used / ${number(e.count)} matching observations<br>${esc(estimateScopeLabel(e.observation_scope))}<br>Planning radius ${state.radius} m; not a confidence interval.`).addTo(group);
  }
  // Keep selected candidates above the area overview.
  estimateLayer?.eachLayer(layer=>layer.bringToFront?.());
}
function scheduleAreaEstimates(){
  clearTimeout(areaTimer);++areaSequence;areaController?.abort();
  const scope=areaQuery();
  scope.delete('bbox');scope.delete('zoom');
  const signature=scope.toString();
  if(signature!==areaDisplayScope||!$('nearby-networks').checked){
    areaRows=[];
    for(const mark of areaMarks.values())areaLayer.removeLayer(mark.group);
    areaMarks.clear();
  }
  areaDisplayScope=signature;
  drawAreaEstimates();
  if(!$('nearby-networks').checked){$('area-status').textContent='Nearby APs hidden.';return;}
  if(!map){$('area-status').textContent='Nearby APs require the interactive map.';return;}
  $('area-status').textContent='Loading nearby AP estimates…';
  areaTimer=setTimeout(loadAreaEstimates,50);
}
async function loadAreaEstimates(){
  if(!$('nearby-networks').checked)return;
  const sequence=++areaSequence,p=areaQuery(),signature=p.toString();
  areaController?.abort();areaController=new AbortController();
  try{
    const saved=new URLSearchParams(p);saved.set('saved_only','1');
    const result=await api('/api/area-estimates',saved,{signal:areaController.signal});
    if(sequence!==areaSequence||signature!==areaQuery().toString()||!$('nearby-networks').checked)return;
    areaRows=result.estimates;drawAreaEstimates();
    const paused=areaSuppressed===signature;
    const status = [number(visibleAreaEstimates().length) + ' nearby AP estimates in view'];
    if(result.zoom_required)status.push('zoom in to level 16 to calculate more');
    if(paused)status.push('automatic fits paused');
    $('area-status').textContent=status.join(' · ');
    if(result.zoom_required||p.has('none')||paused)return;
    clearTimeout(areaTimer);
    areaTimer=setTimeout(async()=>{
      if(sequence!==areaSequence||signature!==areaQuery().toString()||!$('nearby-networks').checked)return;
      try{
        const job=await api('/api/job');
        if(sequence!==areaSequence)return;
        if(job.status==='running'){if(currentJob?.status!=='running')pollJob();return;}
        // Give the detailed selection's automatic fits priority.
        if($('auto-estimate').checked&&state.missingEstimates&&state.eligibleRadios<=20&&autoSuppressedSignature!==query().toString()){
          areaTimer=setTimeout(loadAreaEstimates,1200);return;
        }
        const started=await api('/api/area-estimate',p,{method:'POST'});
        if(sequence!==areaSequence){
          if(started.area_started||started.status==='running')pollJob();
          return;
        }
        if(started.area_more)status.push('discovery limited to 60 candidates in this area');
        if(started.area_started||started.status==='running'){status.push('calculating nearby locations…');pollJob();}
        $('area-status').textContent=status.join(' · ');
      }catch(e){if(sequence===areaSequence){areaSuppressed=signature;$('area-status').textContent=e.message;}}
    },900);
  }catch(e){if(e.name!=='AbortError'&&sequence===areaSequence)$('area-status').textContent=e.message;}
}
function areaFeatures(){return visibleAreaEstimates().flatMap(e=>[
  {type:'Feature',geometry:{type:'Point',coordinates:[e.longitude,e.latitude]},properties:{...e,kind:'nearby_estimated_ap',planning_radius_m:state.radius}},
  {type:'Feature',geometry:{type:'Polygon',coordinates:[searchRing(e.latitude,e.longitude,state.radius)]},properties:{kind:'nearby_search_area',radio_key:e.radio_key,radius_m:state.radius,confidence_interval:false}}
]);}
function bindAreaEstimates(){
  $('nearby-networks').onchange=()=>{areaSuppressed=null;scheduleAreaEstimates();};
}
