// Wails injects its runtime only on folder/index routes. Extensionless pages
// load those same built-in assets explicitly; browser mode never includes this.
window.SignalAtlasNativeReady=(async()=>{
  if(window.runtime?.EventsOn)return true;
  const load=src=>new Promise((resolve,reject)=>{const script=document.createElement('script');script.src=src;script.onload=resolve;script.onerror=()=>reject(new Error('Native runtime unavailable'));document.head.appendChild(script);});
  await load('/wails/ipc.js');await load('/wails/runtime.js');
  return !!window.runtime?.EventsOn;
})();
// Polling remains available if a platform cannot load the native adapter.
window.SignalAtlasNativeReady.catch(()=>{});
