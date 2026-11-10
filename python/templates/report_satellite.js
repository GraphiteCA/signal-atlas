let reportImagery=null, previousReportBasemap=state.basemap;
function switchReportBasemap(value){
 if(value!=='satellite'){
  reportImagery?.hide();state.basemap=value;drawLocalBasemap();setBasemap();return;
 }
 if(!window.L){toast('Satellite needs an internet connection and the Leaflet map library.');$('basemap').value=state.basemap;return;}
 previousReportBasemap=state.basemap==='satellite'?previousReportBasemap:state.basemap;
 if(!map){state.basemap='satellite';initMap();renderMap();}
 if(!map){state.basemap=previousReportBasemap;$('basemap').value=state.basemap;return;}
 if(!reportImagery)reportImagery=new SatelliteLayer(map,message=>{state.basemap=previousReportBasemap;$('basemap').value=state.basemap;drawLocalBasemap();setBasemap();toast(message);});
 if(!reportImagery.connected){$('satellite-dialog').showModal();return;}
 state.basemap='satellite';drawLocalBasemap();reportImagery.show();setBasemap();
}
$('satellite-form').addEventListener('submit',ev=>{ev.preventDefault();reportImagery.connect($('satellite-key').value);$('satellite-key').value='';$('satellite-dialog').close();switchReportBasemap('satellite');});
function cancelReportSatellite(){
 $('satellite-key').value='';$('satellite-dialog').close();state.basemap=previousReportBasemap;$('basemap').value=state.basemap;drawLocalBasemap();setBasemap();
}
$('satellite-cancel').addEventListener('click',cancelReportSatellite);
$('satellite-dialog').addEventListener('cancel',()=>{state.basemap=previousReportBasemap;$('basemap').value=state.basemap;$('satellite-key').value='';setBasemap();});
