// Selected live candidates and immutable, SQLite-backed date comparisons.
const comparisonColors=['#f2c17c','#b399f4','#64b5ff','#f18fac','#b1df76','#ff9866'];
let comparisonRows=[], visibleComparisons=new Set(), autoEstimateTimer=null, autoSuppressedSignature=null;

function bandPills(bands){return [...new Set(bands||[])].sort().map(b=>`<span class="pill band-${esc(b.replace('.','-'))}" ${b==='unknown'?'title="The source has no usable frequency or band, or conflicting band metadata. Assign a known band under the MAC dropdown."':''}>${band(b)}</span>`).join(' ');}
function observationBandTags(row){return bandPills([row.band])+((row.band_source||row.band_sources||'').includes('manual assignment')?' <span class="pill manual-band" title="Band supplied by you for missing source metadata">Manual</span>':'');}
function comparisonDistance(a,b){
  const radians=x=>x*Math.PI/180, p=radians(b.latitude-a.latitude), q=radians(b.longitude-a.longitude);
  const h=Math.sin(p/2)**2+Math.cos(radians(a.latitude))*Math.cos(radians(b.latitude))*Math.sin(q/2)**2;
  return 6371008.8*2*Math.asin(Math.sqrt(Math.min(1,h)));
}
function estimateLayers(){
  return [{id:'current',name:'Current selection',color:'#49e4c4',estimates:state.estimates.filter(e=>e.status==='fitted')},
    ...comparisonRows.filter(r=>visibleComparisons.has(r.id)).map(r=>({...r,color:comparisonColors[comparisonRows.indexOf(r)%comparisonColors.length]}))];
}
function estimateLinks(){
  const seen=new Map(),links=[];
  // Snapshot order is the pin order; the live selection follows the snapshots.
  const layers=estimateLayers(),ordered=[...layers.slice(1),layers[0]];
  for(const layer of ordered)for(const e of layer.estimates){
    const previous=seen.get(e.radio_key);
    if(previous&&JSON.stringify(previous.e.observation_scope)!==JSON.stringify(e.observation_scope)){
      links.push({a:previous.e,b:e,from:previous.name,to:layer.name,color:layer.color,distance:comparisonDistance(previous.e,e)});
    }
    seen.set(e.radio_key,{e,name:layer.name});
  }
  return links;
}
// Opaque white and dark casings keep fits visible over any basemap or imagery.
function planningCircle(position,options,group){
  for(const [color,weight] of [['#101820',7],['#ffffff',5],[options.color,2]])
    L.circle(position,{...options,renderer:estimateRenderer,color,weight,opacity:1,fill:false,interactive:false}).addTo(group);
}
function estimateIcon(tint,small=false){
  return L.divIcon({className:'candidate-marker',html:`<span class="target${small?' target-small':''}" style="--target-color:${tint}"></span>`,iconSize:[26,26],iconAnchor:[13,13]});
}
function drawEstimateLayers(){
  if(!map){drawFallback();return;}
  estimateLayer.clearLayers();
  for(const layer of estimateLayers())for(const e of layer.estimates){
    const tint=e.co_location_assumed&&layer.id==='current'?'#f2c17c':layer.color;
    planningCircle([e.latitude,e.longitude],{radius:state.radius,color:tint,dashArray:layer.id==='current'?'6 5':'3 5'},estimateLayer);
    L.marker([e.latitude,e.longitude],{icon:estimateIcon(tint),zIndexOffset:1000,title:e.ssid||'(hidden SSID)'})
      .bindPopup(`<b>${esc(layer.name)}</b><br>${esc(e.ssid)}<br>${esc(e.mac)} · ${band(e.band)}<br>${bandPills(e.seen_bands)}<br>${esc(estimateScopeLabel(e.observation_scope))}<br>${e.co_location_assumed?'Shared location · colocation assumed':'Independent radio estimate'}<br>Planning radius ${state.radius} m; not a confidence interval.`)
      .bindTooltip(esc(e.ssid||'(hidden SSID)'),{permanent:true,opacity:1,direction:'top',offset:[0,-16],className:'ssid-label'}).addTo(estimateLayer);
  }
  for(const link of estimateLinks()){
    const text=`${link.distance.toFixed(1)} m · ${link.from} → ${link.to}`;
    for(const [color,weight] of [['#101820',7],['#ffffff',5]])
      L.polyline([[link.a.latitude,link.a.longitude],[link.b.latitude,link.b.longitude]],{renderer:estimateRenderer,color,weight,opacity:1,dashArray:'5 7',interactive:false}).addTo(estimateLayer);
    L.polyline([[link.a.latitude,link.a.longitude],[link.b.latitude,link.b.longitude]],{renderer:estimateRenderer,color:link.color,weight:2,opacity:1,dashArray:'5 7'})
      .bindTooltip(esc(text),{permanent:true,opacity:1,direction:'center',className:'distance-label'})
      .bindPopup(`${esc(link.b.ssid)}<br>${esc(link.b.mac)} · ${band(link.b.band)}<br>${esc(text)}<br>Model displacement; not proof of physical movement.`).addTo(estimateLayer);
  }
  drawAreaEstimates();
}
function scheduleAutoEstimates(){
  clearTimeout(autoEstimateTimer);
  const count=state.eligibleRadios||0,eligible=state.eligibleFits||0,missing=state.missingEstimates||0;
  const paused=autoSuppressedSignature===query().toString();
  $('estimate-status').textContent=!state.summary?.count?'Select an SSID or MAC to locate an AP.':!eligible?'Not enough data for this location model. Select more observations or adjust the radio selection.':!missing?'Estimates are up to date for these radios and dates.':
    paused?'Automatic updates paused after cancellation. Use Locate selected radios to resume.':
    !$('auto-estimate').checked?'Selection changed. Locate selected radios to update.':count>20?`${number(count)} eligible radios selected. Narrow to 20 for automatic updates, or use Locate selected radios.`:
    `${number(missing)} estimates need updating for this selection…`;
  if(state.sharedWarning)$('estimate-status').textContent+=' '+state.sharedWarning;
  if(paused||!$('auto-estimate').checked||!missing||count>20||!eligible)return;
  const signature=query().toString();
  autoEstimateTimer=setTimeout(async()=>{
    if(signature!==query().toString())return;
    try{
      const job=await api('/api/job');
      if(job.status==='running'){pollJob();return;}
      const p=query();p.set('missing','1');
      await api('/api/estimate',p,{method:'POST'});pollJob();
    }catch(e){error(e.message);}
  },500);
}
async function loadComparisons(){
  const result=await api('/api/comparisons');comparisonRows=result.comparisons;renderComparisons();
}
function renderComparisons(){
  $('comparison-list').innerHTML=comparisonRows.map((row,i)=>`<div class="comparison-row" style="--layer-color:${comparisonColors[i%comparisonColors.length]}">
    <label><input type="checkbox" data-comparison-visible="${i}" ${visibleComparisons.has(row.id)?'checked':''}><span><strong>${esc(row.name)}</strong><small>${esc(estimateScopeLabel(row.observation_scope))} · ${number(row.estimates.length)} fitted candidates</small></span></label>
    <button data-comparison-fit="${i}">Show</button><button data-comparison-remove="${i}" aria-label="Remove ${esc(row.name)}">Remove</button></div>`).join('')||'<p class="tiny">No pinned estimates yet. Pin an early date range, then compare a later one.</p>';
  drawEstimateLayers();
}
function fitComparisonLayers(rows=estimateLayers()){
  const fits=rows.flatMap(r=>r.estimates).filter(e=>e.status==='fitted');
  if(!fits.length)return;
  if(map){const bounds=L.latLngBounds(fits.map(e=>[e.latitude,e.longitude]));for(const e of fits)bounds.extend(L.latLng(e.latitude,e.longitude).toBounds(state.radius*2));map.fitBounds(bounds.pad(.15),{maxZoom:20,animate:!reduced});}
  else{const pad=state.radius/111195;state.fallbackBounds=[Math.min(...fits.map(e=>e.longitude))-pad*2,Math.min(...fits.map(e=>e.latitude))-pad,Math.max(...fits.map(e=>e.longitude))+pad*2,Math.max(...fits.map(e=>e.latitude))+pad];loadPoints();drawFallback();}
}
function comparisonFeatures(){
  const features=[];
  for(const layer of estimateLayers().slice(1))for(const e of layer.estimates){
    features.push({type:'Feature',geometry:{type:'Point',coordinates:[e.longitude,e.latitude]},properties:{...e,kind:'pinned_estimate',comparison:layer.name,planning_radius_m:state.radius}},
      {type:'Feature',geometry:{type:'Polygon',coordinates:[searchRing(e.latitude,e.longitude,state.radius)]},properties:{kind:'comparison_search_area',comparison:layer.name,radio_key:e.radio_key,radius_m:state.radius,confidence_interval:false}});
  }
  for(const link of estimateLinks())features.push({type:'Feature',geometry:{type:'LineString',coordinates:[[link.a.longitude,link.a.latitude],[link.b.longitude,link.b.latitude]]},properties:{kind:'model_displacement',radio_key:link.b.radio_key,from:link.from,to:link.to,distance_m:link.distance}});
  return features;
}
function bindEstimateComparison(){
  $('auto-estimate').onchange=()=>{autoSuppressedSignature=null;scheduleAutoEstimates();};
  $('individual-estimates').onchange=()=>{
    const enabled=$('individual-estimates').checked;
    $('location-model-field').hidden=!enabled;$('location-model').disabled=!enabled;
    clearTimeout(autoEstimateTimer);autoSuppressedSignature=null;
    scheduleAreaEstimates();loadSelection();
  };
  $('location-model').onchange=()=>loadSelection();
  $('fit-comparisons').onclick=()=>fitComparisonLayers();
  $('pin-comparison').onclick=async()=>{
    try{
      const name=$('comparison-name').value.trim();
      const result=await api('/api/comparisons',query(),{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name})});
      const previous=new Set(comparisonRows.map(r=>r.id));
      comparisonRows=result.comparisons;for(const row of comparisonRows)if(!previous.has(row.id))visibleComparisons.add(row.id);
      $('comparison-name').value='';renderComparisons();error('');
    }catch(e){error(e.message);}
  };
  $('comparison-list').onchange=ev=>{
    const i=ev.target.dataset.comparisonVisible;if(i==null)return;
    ev.target.checked?visibleComparisons.add(comparisonRows[+i].id):visibleComparisons.delete(comparisonRows[+i].id);drawEstimateLayers();
  };
  $('comparison-list').onclick=async ev=>{
    const fit=ev.target.closest('[data-comparison-fit]');if(fit){const row=comparisonRows[+fit.dataset.comparisonFit];visibleComparisons.add(row.id);renderComparisons();fitComparisonLayers([row]);return;}
    const remove=ev.target.closest('[data-comparison-remove]');if(!remove)return;
    try{const result=await api('/api/comparisons/delete',null,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({id:comparisonRows[+remove.dataset.comparisonRemove].id})});comparisonRows=result.comparisons;renderComparisons();}catch(e){error(e.message);}
  };
}
