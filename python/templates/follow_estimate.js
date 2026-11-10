// Recenter only when the selected fit changes, never on ordinary pan/zoom or
// record paging. Later radio/date changes preserve the user's zoom level.
let focusedEstimateSignature=null,focusedEstimateNetworks=null;
function focusSelectedEstimate(force=false){
  if(!$('follow-estimate').checked&&!force)return false;
  const fitted=state.estimates.filter(e=>e.status==='fitted');
  const sharedNetworks=new Set(fitted.filter(e=>e.co_location_assumed).map(e=>e.ssid));
  const targets=fitted.filter(e=>e.co_location_assumed||!sharedNetworks.has(e.ssid));
  if(!targets.length){if(!state.summary?.count){focusedEstimateSignature=null;focusedEstimateNetworks=null;}return false;}
  const networks=JSON.stringify([...new Set(targets.map(e=>e.ssid))].sort());
  const signature=query().toString()+JSON.stringify(targets.map(e=>[e.radio_key,e.latitude,e.longitude]));
  if(!force&&signature===focusedEstimateSignature)return false;
  const preserveZoom=targets.length===1&&networks===focusedEstimateNetworks;
  focusedEstimateSignature=signature;focusedEstimateNetworks=networks;
  if(map){
    map.stop();
    if(preserveZoom)map.setView([targets[0].latitude,targets[0].longitude],map.getZoom(),{animate:false});
    else{
      const bounds=L.latLngBounds([]);
      for(const e of targets)bounds.extend(L.latLng(e.latitude,e.longitude).toBounds(Math.max(120,state.radius*6)));
      map.fitBounds(bounds,{maxZoom:20,padding:[35,35],animate:false});
      if(targets.length===1)map.panTo([targets[0].latitude,targets[0].longitude],{animate:false});
    }
  }else{
    const pad=Math.max(120,state.radius*6)/2/111195,lat=targets[0].latitude,dx=pad/Math.cos(lat*Math.PI/180);
    state.fallbackBounds=[Math.min(...targets.map(e=>e.longitude))-dx,Math.min(...targets.map(e=>e.latitude))-pad,
      Math.max(...targets.map(e=>e.longitude))+dx,Math.max(...targets.map(e=>e.latitude))+pad];
    drawFallback();
  }
  return true;
}
function bindEstimateFollow(){
  $('follow-estimate').onchange=()=>{if($('follow-estimate').checked){focusSelectedEstimate(true);loadPoints();}};
  $('center-ap').onclick=()=>{focusSelectedEstimate(true);loadPoints();};
}
