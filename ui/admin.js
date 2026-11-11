(function(){
  'use strict';
  const $=id=>document.getElementById(id),fi=n=>Number(n||0).toLocaleString(),esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  let active=null,operation=null,preview=null,historyOffset=0,historyTotal=0,native=false,pending='',refreshing=false;
  let prepSettings=null;
  const solverLabel=s=>s==='opencl64-reference-v1'?'GPU reference':s==='opencl64-fast-v1'?'GPU fast':s==='fast'?'Fast':'Reference';
  function renderAcceleration(j){const s=j.acceleration||{};$('adminGPUStatus').textContent=!j.settings?.gpu_fits?'GPU acceleration is off.':s.available?`GPU ready · ${s.device}${s.fallbacks?` · ${fi(s.fallbacks)} CPU fallbacks`:''}`:s.reason||'GPU will be checked when calculations start.';}
  async function refreshPreparationAdmin(force=false){
    const config=await(await fetch('/api/app/settings',{cache:'no-store'})).json();renderAcceleration(config);
    const r=await fetch('/api/app/preparation/status',{cache:'no-store'});if(!r.ok)return;const s=await r.json(),p=s.progress||{};
    $('adminPrepStatus').textContent=s.status==='complete'?`Locations ready · ${fi(p.completed)} networks prepared.`:s.status==='running'?`Preparing ${fi(p.completed)} of ${fi(p.total)} networks · ${solverLabel(s.namespace?.solver)} solver.`:s.status==='limited'?`${fi(p.blocked)} networks remain above the background memory allowance.`:s.status==='failed'?s.error:`Background preparation: ${s.status}.`;
    if(!prepSettings||force){const d=s.settings;$('adminPrepEnabled').checked=d.enabled;$('adminPrepMode').value=d.fit_mode;$('adminPrepGrouping').value=d.grouping;$('adminPrepWorkers').value=d.workers;$('adminPrepMemory').value=d.memory_mb;}
    prepSettings=s.settings;$('adminPrepPause').textContent=s.settings.paused?'Resume background':'Pause background';
  }
  async function savePreparationAdmin(body){
    try{error('');const r=await fetch('/api/app/preparation/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}),j=await r.json();if(!r.ok)throw new Error(j.error||r.statusText);await refreshPreparationAdmin(true);}catch(e){error(e.message);}
  }
  const titles={delete:'Deleting workspace',new:'Creating a new survey',open:'Opening survey',convert:'Converting saved survey',import:'Importing scans',wigle:'Importing from WiGLE',cleanup:'Removing 0,0 rows',restore:'Restoring previous revision',snapshot:'Saving snapshot',maintenance:'Survey maintenance'};
  function error(message){$('adminError').textContent=message;$('adminError').hidden=!message;}
  async function request(action,body){
    const r=await fetch('/api/app/admin/'+action,body===undefined?{cache:'no-store'}:{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}),j=await r.json();if(!r.ok)throw new Error(j.error||r.statusText);return j;
  }
  function tab(name){for(const button of document.querySelectorAll('[data-tab]'))button.setAttribute('aria-selected',String(button.dataset.tab===name));for(const panel of document.querySelectorAll('[data-panel]'))panel.hidden=panel.dataset.panel!==name;try{history.replaceState(null,'','/admin?tab='+encodeURIComponent(name));}catch(e){}if(name==='imports')loadHistory();}
  function resultHTML(result){
    if(!result)return '';
    if(Array.isArray(result.files))return '<div class="table-scroll"><table><thead><tr><th>Source</th><th>Accepted</th><th>Zero pairs excluded</th><th>Invalid</th><th>Other rows</th><th>Outcome</th></tr></thead><tbody>'+result.files.map(r=>`<tr><td>${esc(r.filename)}</td><td>${fi(r.duplicate?0:r.valid_rows)}</td><td>${fi(r.zero_excluded_rows)}</td><td>${fi(r.invalid_rows)}</td><td>${fi(r.non_wifi_rows)}</td><td>${esc(r.error|| (r.duplicate?'Already imported':r.empty?'No rows added':'Committed'))}</td></tr>`).join('')+'</tbody></table></div>';
    if(result.removed_rows!==undefined)return `<strong>${fi(result.removed_rows)} rows removed.</strong> ${result.recovery_available?'The prior revision is available under Recovery.':''}`;
    if(result.deleted)return 'Permanently deleted workspace: <code>'+esc(result.deleted)+'</code>';
    if(result.saved)return 'Snapshot saved to <code>'+esc(result.path)+'</code>';
    if(result.opened)return 'Active survey: <code>'+esc(result.opened)+'</code>';
    if(result.restored)return 'Previous survey revision restored.';
    if(result.valid)return 'Validation passed · '+fi(result.observations)+' observations.';
    if(result.rebuilt)return 'Lookup indexes rebuilt. Saved locations and comparisons were kept.';
    return '<pre>'+esc(JSON.stringify(result,null,2))+'</pre>';
  }
  function renderOperation(op){
    operation=op;const running=op?.status==='running';$('adminOperation').hidden=!op||op.status==='idle';
    $('adminOperation').dataset.id=op?.id||'';$('adminOperation').dataset.status=op?.status||'idle';
    if(!op||op.status==='idle')return;
    $('adminOperationTitle').textContent=(titles[op.kind]||'Administration')+' · '+op.status;
    $('adminOperationStage').textContent=op.error||op.stage||'';
    $('adminCancel').hidden=!running;$('adminCancel').disabled=!op.can_cancel;
    const progress=$('adminProgress');if(op.total>0){progress.max=op.total;progress.value=op.completed||0;$('adminProgressText').textContent=fi(op.completed)+' / '+fi(op.total);}else{progress.removeAttribute('value');$('adminProgressText').textContent=running?'Working…':'';}
    if(!running&&op.status==='complete')progress.value=progress.max||1;
    renderWiGLE();
    $('adminResult').innerHTML=resultHTML(op.result);
    if(!running&&pending===op.id){
      pending='';preview=null;$('adminPreviewResult').hidden=true;
      if(op.result?.reload){sessionStorage.setItem('signal-atlas.admin.notice',JSON.stringify({id:op.id,text:op.status==='cancelled'?'Import cancelled. Completed files were retained.':(titles[op.kind]||'Operation')+' complete.'}));location.reload();return;}
      loadHistory();
    }
  }
  function renderWiGLE(){
    const w=operation?.wigle;
    $('adminWigleCounts').hidden=!w;
    if(w)$('adminWigleCounts').textContent=`${fi(w.found)} uploads found · ${fi(w.captured)} captured · ${fi(w.remaining)} remaining${w.failed?` · ${fi(w.failed)} failed`:''}`;
    const retry=operation?.status==='running'&&operation?.retry_at;
    $('adminWigleBackoff').hidden=!retry;
    if(retry){const seconds=Math.max(0,Math.ceil((retry-Date.now())/1000));$('adminWigleBackoff').textContent=`${operation.retry_reason}. ${seconds?`Retrying in ${Math.floor(seconds/60)}:${String(seconds%60).padStart(2,'0')}`:'Retrying…'}`;}
  }
  function recentButtons(){
    const selected=!!$('adminRecent').value;
    $('adminRecentOpen').disabled=!selected||operation?.status==='running';
    $('adminRecentForget').disabled=!selected;
    $('adminRecentDelete').disabled=!selected||operation?.status==='running'||$('adminRecent').value===active?.path;
  }
  async function refresh(){
    if(refreshing)return;refreshing=true;
    try{
      const s=await request('status');active=s;native=!!s.dialogs;
      $('adminName').textContent=s.path.split(/[\\/]/).filter(Boolean).at(-1)||'Survey';$('adminPath').textContent=s.path;
      const d=s.database||{};$('adminCounts').innerHTML=[['Stored observations',d.observations],['Saved estimates',d.cached_estimates],['Imports',d.imports]].map(([label,n])=>`<div><b>${fi(n)}</b><span>${label}</span></div>`).join('');
      const selected=$('adminRecent').value;$('adminRecent').innerHTML='<option value="">Choose a workspace…</option>'+(s.recent||[]).map(path=>`<option value="${esc(path)}">${esc(path)}</option>`).join('');$('adminRecent').value=selected;
      const running=s.operation.status==='running',surveyBusy=s.job.status==='running';
      for(const b of document.querySelectorAll('[data-operation]'))b.disabled=running;
      $('adminCleanup').disabled=running||!preview?.rows;
      $('adminRestore').disabled=running||!s.recovery_available;$('adminRecoveryDetail').textContent=s.recovery_available?'A previous complete revision is available.':'No prior revision has been retained for this workspace.';
      $('adminSurveyJob').hidden=!surveyBusy;$('adminJobText').textContent=surveyBusy?`${s.job.kind}: ${fi(s.job.completed)} / ${fi(s.job.total)} · finish or cancel before snapshots, imports or maintenance.`:'';
      $('adminSnapshotPathField').hidden=native;$('adminConvertFields').hidden=native;$('adminPaths').open=!native;
      if(s.warning){$('adminNotice').textContent=s.warning;$('adminNotice').hidden=false;}
      renderOperation(s.operation);recentButtons();
      await refreshPreparationAdmin();
    }catch(e){error(e.message);}finally{refreshing=false;}
  }
  async function run(action,body={}){
    error('');try{const op=await request(action,body);if(op.cancelled)return;pending=op.id||'';renderOperation(op);await refresh();}catch(e){error(e.message);}
  }
  async function loadHistory(){
    try{const h=await request('imports?offset='+historyOffset);historyTotal=h.total;$('adminHistory').innerHTML=h.rows.length?h.rows.map(r=>`<tr><td>${esc(r.filename)}</td><td>${esc(r.imported_utc)}</td><td>${fi(r.active_rows)}</td><td>${fi(r.valid_rows)}</td><td>${fi(r.zero_excluded_rows)}</td><td>${fi(r.zero_removed_rows)}</td><td>${fi(r.invalid_rows)}</td><td>${fi(r.non_wifi_rows)}</td></tr>`).join(''):'<tr><td colspan="8">No imports in this survey.</td></tr>';$('adminHistoryPage').textContent=h.total?`${fi(h.offset+1)}–${fi(h.offset+h.rows.length)} of ${fi(h.total)}`:'0 imports';$('adminHistoryPrev').disabled=historyOffset===0;$('adminHistoryNext').disabled=historyOffset+100>=historyTotal;}catch(e){error(e.message);}
  }
  function confirm(title,text,label,apply){$('adminConfirmTitle').textContent=title;$('adminConfirmText').textContent=text;$('adminConfirmApply').textContent=label;$('adminConfirmApply').onclick=()=>{$('adminConfirm').close();apply();};$('adminConfirm').showModal();}
  async function acceptFiles(files){
    if(!files.length)return;error('');const paths=[];
    try{for(const file of files){const path=await SignalAtlasApp.pathOf(file);if(path){paths.push(path);continue;}
      $('adminNotice').hidden=false;$('adminNotice').textContent='Copying '+file.name+' to the local importer…';
      const r=await fetch('/api/app/admin/upload',{method:'POST',headers:{'X-Filename':encodeURIComponent(file.name)},body:await file.arrayBuffer()}),j=await r.json();if(!r.ok)throw new Error(j.error||r.statusText);paths.push(j.path);}
      await run('import',{paths,skip_invalid:$('adminSkipInvalid').checked});
    }catch(e){error(e.message);}
  }
  async function init(){
    const info=await SignalAtlasApp.ready;native=info.window;
    for(const button of document.querySelectorAll('[data-tab]'))button.onclick=()=>tab(button.dataset.tab);
    $('adminRefresh').onclick=refresh;
    $('adminPrepSave').onclick=()=>savePreparationAdmin({enabled:$('adminPrepEnabled').checked,fit_mode:$('adminPrepMode').value,grouping:$('adminPrepGrouping').value,workers:Number($('adminPrepWorkers').value),memory_mb:Number($('adminPrepMemory').value)});
    $('adminPrepPause').onclick=()=>savePreparationAdmin({paused:!prepSettings?.paused});
    $('adminNew').onclick=()=>native?run('new'):($('adminPaths').open=true,$('adminWorkspacePath').focus());
    $('adminOpen').onclick=()=>native?run('open'):($('adminPaths').open=true,$('adminWorkspacePath').focus());
    $('adminNewPath').onclick=()=>run('new',{path:$('adminWorkspacePath').value.trim()});$('adminOpenPath').onclick=()=>run('open',{path:$('adminWorkspacePath').value.trim()});
    $('adminRecent').onchange=recentButtons;
    $('adminRecentRefresh').onclick=refresh;
    $('adminRecentForget').onclick=async()=>{
      const path=$('adminRecent').value;if(!path)return;
      try{error('');await request('forget',{path});$('adminRecent').value='';recentButtons();await refresh();$('adminNotice').textContent='Workspace removed from the recent list. Its files are unchanged.';$('adminNotice').hidden=false;}catch(e){error(e.message);}
    };
    $('adminRecentDelete').onclick=()=>{
      const path=$('adminRecent').value;if(!path)return;
      confirm('ARE YOU REALLY SURE?',`Permanently delete this workspace?\n\n${path}\n\nThe entire folder and everything inside it will be deleted, including observations, saved locations, comparisons and recovery revisions. This cannot be undone and does not move files to the Recycle Bin.`, 'Delete workspace permanently',()=>run('delete',{path,confirmed:true}));
      $('adminConfirmCancel').focus();
    };
    $('adminRecentOpen').onclick=()=>{const path=$('adminRecent').value;if(path)run('open',{path});};
    $('adminSave').onclick=()=>run('snapshot',{format:$('adminSnapshotFormat').value,path:$('adminSnapshotPath').value.trim()});
    $('adminConvert').onclick=()=>run('convert',{path:$('adminConvertTarget').value.trim(),source:$('adminConvertSource').value.trim()});
    $('adminImport').onclick=()=>native?run('import',{skip_invalid:$('adminSkipInvalid').checked}):$('adminFiles').click();
    $('adminWigleImport').onclick=async()=>{
      const token=$('adminWigleToken').value.trim();
      if(!token){error('Enter your WiGLE Encoded for use token.');$('adminWigleToken').focus();return;}
      $('adminWigleToken').value='';
      await run('wigle',{token,skip_invalid:$('adminSkipInvalid').checked});
    };
    setInterval(renderWiGLE,250);
    $('adminFiles').onchange=async e=>{await acceptFiles(Array.from(e.target.files));e.target.value='';};
    $('adminImportByPath').onclick=()=>run('import',{paths:$('adminImportPaths').value.split(/\r?\n/).map(s=>s.trim()).filter(Boolean),skip_invalid:$('adminSkipInvalid').checked});
    const drop=$('adminDropZone');drop.ondragover=e=>{e.preventDefault();drop.classList.add('dragging');};drop.ondragleave=()=>drop.classList.remove('dragging');drop.ondrop=e=>{e.preventDefault();drop.classList.remove('dragging');acceptFiles(Array.from(e.dataTransfer.files));};
    $('adminHistoryRefresh').onclick=loadHistory;$('adminHistoryPrev').onclick=()=>{historyOffset=Math.max(0,historyOffset-100);loadHistory();};$('adminHistoryNext').onclick=()=>{historyOffset+=100;loadHistory();};
    $('adminPreview').onclick=async()=>{try{error('');preview=await request('preview');$('adminPreviewResult').hidden=false;$('adminPreviewText').textContent=`${fi(preview.rows)} rows at 0,0 · ${fi(preview.imports)} imports · ${fi(preview.networks)} networks touched.`;$('adminCleanup').disabled=!preview.rows;}catch(e){error(e.message);}};
    $('adminCleanup').onclick=()=>{if(!preview?.rows)return;confirm('Remove '+fi(preview.rows)+' rows at 0,0?',`Active survey: ${active.path}. Surviving record IDs, source history and pinned comparisons are kept. Locations of the ${fi(preview.networks)} touched networks are invalidated; other saved locations stay. A recovery revision is retained.`,'Remove '+fi(preview.rows)+' rows',()=>run('cleanup',{token:preview.token}));};
    $('adminRestore').onclick=()=>confirm('Restore the previous survey revision?',`Restore the prior complete revision of ${active.path}. The current revision remains available for recovery.`,'Restore revision',()=>run('restore'));
    $('adminConfirmCancel').onclick=()=>$('adminConfirm').close();
    for(const b of document.querySelectorAll('[data-task]'))b.onclick=()=>run('maintenance',{task:b.dataset.task});
    $('adminCancel').onclick=()=>request('cancel',{id:operation?.id}).then(renderOperation).catch(e=>error(e.message));
    $('adminCancelJob').onclick=()=>fetch('/api/cancel',{method:'POST'}).then(refresh).catch(e=>error(e.message));
    const settings=await(await fetch('/api/app/settings',{cache:'no-store'})).json();$('adminFastFits').checked=!!settings.settings.fast_fits;$('adminGPUFits').checked=!!settings.settings.gpu_fits;renderAcceleration(settings);
    const saveCalculation=async()=>{try{const r=await fetch('/api/app/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({fast_fits:$('adminFastFits').checked,gpu_fits:$('adminGPUFits').checked})}),j=await r.json();if(!r.ok)throw new Error(j.error);renderAcceleration(j);$('adminSettingsStatus').textContent='Saved for this app';}catch(e){error(e.message);}};
    $('adminFastFits').onchange=saveCalculation;$('adminGPUFits').onchange=saveCalculation;
    try{const notice=JSON.parse(sessionStorage.getItem('signal-atlas.admin.notice')||'null');if(notice){$('adminNotice').textContent=notice.text;$('adminNotice').hidden=false;sessionStorage.removeItem('signal-atlas.admin.notice');}}catch(e){}
    const panel=new URLSearchParams(location.search).get('tab');tab(['survey','imports','maintenance'].includes(panel)?panel:'survey');await refresh();setInterval(refresh,1000);
  }
  init().catch(e=>error(e.message));
})();
