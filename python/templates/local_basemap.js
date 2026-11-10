// Local GeoJSON rendering. This module never performs network requests.
function mapPalette(){return state.basemap==='street'
 ? {background:'#e5e9e5',outside:'#ccd4d1',building:'#c7cecc',outline:'#a9b7b5',park:'#c6dac9',water:'#a9cfdf',road:'#f6f4ed',casing:'#bac4c2',path:'#acb7a2',text:'#455b63'}
 : {background:'#111e2a',outside:'#09121d',building:'#233442',outline:'#3a4d59',park:'#142e29',water:'#173a50',road:'#526575',casing:'#293e4e',path:'#60796b',text:'#b7cbd6'};}
function featureStyle(feature,pixelsPerMetre=1){
 const p=mapPalette(),tags=feature.properties||{};
 if(feature.geometry.type==='Polygon'){
  const water=tags.natural==='water',building=!!tags.building;
  return {color:building?p.outline:water?p.water:p.park,weight:building?.7:1,fillColor:building?p.building:water?p.water:p.park,fillOpacity:1,opacity:1,interactive:false};
 }
 const kind=tags.highway,path=['footway','path','cycleway','steps','pedestrian'].includes(kind);
 const width=({motorway:16,trunk:14,primary:12,secondary:10,tertiary:9,residential:7,unclassified:6,service:4,footway:1.4,path:1.4,cycleway:2,steps:1.4})[kind]||2;
 return {color:tags.waterway?p.water:path?p.path:p.road,weight:Math.max(path?1:1.5,Math.min(45,width*pixelsPerMetre)),opacity:1,dashArray:path?'3 4':null,lineCap:'round',lineJoin:'round',interactive:false};
}
function roadLabels(){
 if(!report.basemap)return[];
 const chosen=new Map(),bbox=report.basemap.metadata.bbox;
 for(const feature of report.basemap.features){
  const tags=feature.properties||{};if(feature.geometry.type!=='LineString'||!tags.highway||!tags.name||['footway','path','cycleway','steps','service'].includes(tags.highway))continue;
  const coords=feature.geometry.coordinates;
  for(let i=1;i<coords.length;i++){
   const a=coords[i-1],b=coords[i],lon=(a[0]+b[0])/2,lat=(a[1]+b[1])/2;
   if(lon<bbox[0]||lon>bbox[2]||lat<bbox[1]||lat>bbox[3])continue;
   const length=distance([a[1],a[0]],[b[1],b[0]]);if(length<12)continue;
   const score=length/(1+distance(center(),[lat,lon])/250),old=chosen.get(tags.name);
   if(!old||score>old.score)chosen.set(tags.name,{name:tags.name,latitude:lat,longitude:lon,score,a,b});
  }
 }
 return [...chosen.values()];
}
function drawLocalBasemap(){
 if(!map||!report.basemap)return;
 if(basemapLayer)map.removeLayer(basemapLayer);
 basemapLayer=null;
 if(state.basemap==='satellite')return;
 basemapLayer=L.layerGroup().addTo(map);
 const p=mapPalette(),ppm=2**(map.getZoom()+8)/(40075016.686*Math.cos(est.latitude*Math.PI/180));
 $('map').style.background=p.outside;
 const bbox=report.basemap.metadata.bbox,area=[[bbox[1],bbox[0]],[bbox[3],bbox[2]]];
 L.rectangle(area,{pane:'localBasemap',stroke:false,fillColor:p.background,fillOpacity:1,interactive:false}).addTo(basemapLayer);
 const features=report.basemap.features.slice().sort((a,b)=>{
  const rank=f=>f.geometry.type==='Polygon'?(f.properties.building?1:0):2;return rank(a)-rank(b);
 });
 L.geoJSON({type:'FeatureCollection',features},{pane:'localBasemap',interactive:false,style:f=>featureStyle(f,ppm)}).addTo(basemapLayer);
 for(const label of roadLabels()){
  const a=map.latLngToLayerPoint([label.a[1],label.a[0]]),b=map.latLngToLayerPoint([label.b[1],label.b[0]]);
  let angle=Math.atan2(b.y-a.y,b.x-a.x)*180/Math.PI;if(angle>90)angle-=180;if(angle< -90)angle+=180;
  L.marker([label.latitude,label.longitude],{pane:'localLabels',interactive:false,keyboard:false,
   icon:L.divIcon({className:'local-road-label',html:`<span style="color:${p.text};transform:translate(-50%,-50%) rotate(${angle}deg);text-shadow:0 1px 3px ${p.background},0 -1px 3px ${p.background}">${esc(label.name)}</span>`,iconSize:[0,0]})}).addTo(basemapLayer);
 }
 L.rectangle(area,{pane:'localBasemap',color:p.outline,weight:1,dashArray:'5 6',fill:false,interactive:false}).addTo(basemapLayer);
}
function localMapMarkup(project,scale){
 if(!report.basemap||state.basemap==='grid')return'';
 const p=mapPalette(),bbox=report.basemap.metadata.bbox,sw=project(bbox[1],bbox[0]),ne=project(bbox[3],bbox[2]);
 let shapes='',labels='';
 const features=report.basemap.features.slice().sort((a,b)=>{
  const rank=f=>f.geometry.type==='Polygon'?(f.properties.building?1:0):2;return rank(a)-rank(b);
 });
 for(const feature of features){
  const polygon=feature.geometry.type==='Polygon',coords=polygon?feature.geometry.coordinates[0]:feature.geometry.coordinates;
  const d=coords.map((c,i)=>{const point=project(c[1],c[0]);return`${i?'L':'M'}${point[0].toFixed(2)},${point[1].toFixed(2)}`;}).join(' ')+(polygon?'Z':'');
  const style=featureStyle(feature,scale);
  shapes+=`<path d="${d}" fill="${polygon?style.fillColor:'none'}" stroke="${style.color}" stroke-width="${style.weight}" stroke-linecap="round" stroke-linejoin="round"${style.dashArray?' stroke-dasharray="3 4"':''}/>`;
 }
 for(const label of roadLabels()){
  const a=project(label.a[1],label.a[0]),b=project(label.b[1],label.b[0]),point=project(label.latitude,label.longitude);
  let angle=Math.atan2(b[1]-a[1],b[0]-a[0])*180/Math.PI;if(angle>90)angle-=180;if(angle< -90)angle+=180;
  labels+=`<text transform="translate(${point[0]},${point[1]}) rotate(${angle})" text-anchor="middle" fill="${p.text}" font-family="Arial,sans-serif" font-size="11" style="paint-order:stroke;stroke:${p.background};stroke-width:3px">${esc(label.name)}</text>`;
 }
 return `<defs><clipPath id="excerpt-clip"><rect x="${sw[0]}" y="${ne[1]}" width="${ne[0]-sw[0]}" height="${sw[1]-ne[1]}"/></clipPath></defs><g clip-path="url(#excerpt-clip)" pointer-events="none"><rect x="${sw[0]}" y="${ne[1]}" width="${ne[0]-sw[0]}" height="${sw[1]-ne[1]}" fill="${p.background}"/>${shapes}${labels}</g>`;
}
