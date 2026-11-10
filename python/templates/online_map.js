// Live map requests use only the visible viewport and the normal browser cache.
// No tile proxy, prefetch, cache bypass, bulk download, or automatic retry.
function mapProblem(message){$('map-problem-text').textContent=message;$('map-problem').hidden=!message;}
function watchMapTiles(layer,message){
  const failed=new Set(),key=e=>`${e.coords.z}/${e.coords.x}/${e.coords.y}`;
  const active=()=>baseLayer===layer||imagery?.layer===layer;
  function refresh(){if(active())mapProblem(failed.size?`${message} (${failed.size} tiles). Retry tiles or choose another map style.`:'');}
  layer.on('tileerror',e=>{failed.add(key(e));e.tile.style.opacity='0';refresh();});
  layer.on('tileload',e=>{failed.delete(key(e));refresh();});
  layer.on('tileunload',e=>{failed.delete(key(e));refresh();});
  layer.on('loading',()=>{if(active())$('map-status').textContent='Loading visible map tiles…';});
  layer.on('load',()=>{
    if(!active())return;
    $('map-status').textContent=state.basemap==='satellite'?'Satellite · Esri · online':
      'Live OpenStreetMap · browser cache · source detail through zoom 19; enlarged above it';
    refresh();
  });
}
function onlineStreet(){
  const layer=L.tileLayer('https://tile.openstreetmap.org/{z}/{x}/{y}.png',{
    maxNativeZoom:19,maxZoom:22,updateWhenIdle:true,updateWhenZooming:false,keepBuffer:1,
    referrerPolicy:'strict-origin-when-cross-origin',
    attribution:'&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors · <a href="https://www.openstreetmap.org/fixthemap" target="_blank" rel="noopener noreferrer">Report a map issue</a>'
  });
  baseLayer=layer;watchMapTiles(layer,'Some street tiles are unavailable or blocked');
  layer.addTo(map);
  $('map-status').textContent='Live OpenStreetMap · viewed area sent to the map provider · no survey records sent';
}
function bindOnlineMap(){
  $('retry-tiles').onclick=()=>{
    const layer=state.basemap==='satellite'?imagery?.layer:baseLayer;
    if(layer?.redraw){mapProblem('');layer.redraw();}
  };
}
