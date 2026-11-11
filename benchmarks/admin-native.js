(async()=>{
  const wait=async predicate=>{for(let i=0;i<400;i++){if(predicate())return;await new Promise(r=>setTimeout(r,50));}throw new Error('Native admin timeout');};
  const report={schema:1,checks:[],user_agent:navigator.userAgent};
  try{
    await SignalAtlasApp.ready;
    await wait(()=>document.getElementById('adminName').textContent!=='Connecting…');
    const status=await(await fetch('/api/app/admin/status')).json();
    if(!(status.database.observations>0))throw new Error('Fixture is empty');
    report.observations=status.database.observations;
    document.querySelector('[data-tab=maintenance]').click();document.getElementById('adminPreview').click();
    await wait(()=>!document.getElementById('adminPreviewResult').hidden);
    report.preview=document.getElementById('adminPreviewText').textContent;
    document.querySelector('[data-task=validate]').click();
    await wait(()=>document.getElementById('adminOperation').dataset.status==='complete');
    if(!document.getElementById('adminResult').textContent.includes('Validation passed'))throw new Error(document.getElementById('adminResult').textContent);
    if(document.getElementById('adminError').textContent)throw new Error(document.getElementById('adminError').textContent);
    report.checks.push('Wails/WebView2 admin loads real Go engine data','Preview and validation work through native UI');
    report.success=true;
  }catch(e){report.success=false;report.error=String(e.stack||e);}
  await fetch('/__bench/report',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(report)});
})();
