(()=>{
 'use strict';
 const $=id=>document.getElementById(id),fi=n=>Number(n).toLocaleString();
 let survey='',schema=null,result=null,busy=false,controller=null,queryID='',saved=null,offsets=[];
 const json=async r=>{const j=await r.json();if(!r.ok)throw Error(j.error||r.statusText);return j;};
 const error=s=>{$('sqlError').textContent=s;$('sqlError').hidden=!s;};
 const quote=s=>'"'+s.replaceAll('"','""')+'"';
 function controls(){for(const id of ['sqlRun','sqlExplain','sqlRefresh','sqlLimit','sqlTimeout','sqlText'])$(id).disabled=busy;$('sqlCancel').hidden=!busy;$('sqlPrev').disabled=busy||!result||result.offset===0;$('sqlNext').disabled=busy||!result?.has_more;$('sqlExport').disabled=busy||!result?.rows.length;}
 async function refresh(){
  try{error('');const info=await fetch('/api/app/info',{cache:'no-store'}).then(json);survey=info.survey_id;$('sqlPath').textContent=info.survey_path;$('sqlName').textContent=info.survey_path.split(/[\\/]/).filter(Boolean).at(-1);
   schema=await fetch('/api/app/sql/schema',{cache:'no-store',headers:{'X-Survey-ID':survey}}).then(json);$('sqlCount').textContent=fi(schema.observations);
   const tables=new Map();for(const c of schema.columns){if(!tables.has(c.table_name))tables.set(c.table_name,[]);tables.get(c.table_name).push(c);}
   $('sqlSchema').replaceChildren();for(const [table,cols]of [...tables].sort((a,b)=>a[0]==='observations'?-1:b[0]==='observations'?1:a[0].localeCompare(b[0]))){const box=document.createElement('div');box.className='schema-table';const button=document.createElement('button');button.textContent=table;button.onclick=()=>{if(busy)return;$('sqlText').value=`SELECT * FROM ${quote(table)} LIMIT 1000;`;$('sqlText').focus();};box.append(button);const d=document.createElement('details'),s=document.createElement('summary');s.textContent=cols.length+' columns';d.append(s);for(const c of cols){const line=document.createElement('div');line.className='schema-column';line.textContent=c.column_name+' · '+c.data_type;d.append(line);}box.append(d);$('sqlSchema').append(box);}
  }catch(e){error(e.message);}
 }
 function render(){const table=document.createElement('table'),head=document.createElement('thead'),tr=document.createElement('tr');for(const c of result.columns){const th=document.createElement('th');th.textContent=c.name;th.title=c.type;tr.append(th);}head.append(tr);table.append(head);const body=document.createElement('tbody');for(const row of result.rows){const tr=document.createElement('tr');for(const cell of row){const td=document.createElement('td');td.textContent=cell===null?'NULL':cell;if(cell===null)td.className='sql-null';tr.append(td);}body.append(tr);}table.append(body);$('sqlResults').replaceChildren(table);$('sqlStatus').textContent=`${fi(result.rows.length)} rows · ${(result.elapsed_seconds*1000).toFixed(1)} ms${result.offset?' · starting at row '+fi(result.offset+1):''}${result.has_more?' · more available':''}${result.truncated_cells?' · '+fi(result.truncated_cells)+' shortened cells':''}`;$('sqlRevision').textContent=`Snapshot ${result.revision} · ${fi(result.observations)} observations. Paging stops if the survey changes.`;}
 async function run(explain=false,offset=0,paging=false){
  if(busy)return;if(!paging){offsets=[];saved={sql:$('sqlText').value,limit:Number($('sqlLimit').value),timeout_seconds:Number($('sqlTimeout').value),explain};result=null;}
  busy=true;error('');controls();$('sqlStatus').textContent='Running query…';queryID=globalThis.crypto?.randomUUID?.()||Date.now().toString(36)+Math.random().toString(36).slice(2);controller=new AbortController();const request={...saved,offset,revision:paging?result?.revision:'',id:queryID};
  try{result=await fetch('/api/app/sql/query',{method:'POST',headers:{'Content-Type':'application/json','X-Survey-ID':survey},body:JSON.stringify(request),signal:controller.signal}).then(json);render();}catch(e){error(e.name==='AbortError'?'Query cancelled.':e.message);$('sqlStatus').textContent='Query stopped. Run again to refresh results.';result=null;$('sqlResults').replaceChildren();}finally{busy=false;controller=null;controls();}
 }
 async function cancel(){if(!busy)return;try{await fetch('/api/app/sql/cancel?id='+encodeURIComponent(queryID),{method:'POST'});}finally{controller?.abort();}}
 function csv(){const field=v=>v===null?'':('"'+String(v).replaceAll('"','""')+'"');const text=[result.columns.map(c=>field(c.name)).join(','),...result.rows.map(r=>r.map(field).join(','))].join('\r\n');return text;}
 $('sqlRun').onclick=()=>run();$('sqlExplain').onclick=()=>run(true);$('sqlCancel').onclick=cancel;$('sqlRefresh').onclick=refresh;$('sqlNext').onclick=()=>{offsets.push(result.offset);run(saved.explain,result.next_offset,true);};$('sqlPrev').onclick=()=>run(saved.explain,offsets.pop()??0,true);
 $('sqlText').onkeydown=e=>{if(e.key==='Enter'&&(e.ctrlKey||e.metaKey)){e.preventDefault();run();}};
 $('sqlExport').onclick=async()=>{const blob=new Blob([csv()],{type:'text/csv;charset=utf-8'});const url=URL.createObjectURL(blob),a=document.createElement('a');a.href=url;a.download='query-page.csv';a.click();setTimeout(()=>URL.revokeObjectURL(url),1000);};
 addEventListener('pagehide',()=>{if(busy){navigator.sendBeacon('/api/app/sql/cancel?id='+encodeURIComponent(queryID));controller?.abort();}});
 window.SignalAtlasApp?.listenJobs?.(()=>{});
 refresh();controls();
})();
