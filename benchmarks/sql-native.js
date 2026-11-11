// Exercises the real SQL page through Wails/WebView2; the host closes itself.
(async()=>{
 const wait=async predicate=>{for(let i=0;i<400;i++){if(predicate())return;await new Promise(r=>setTimeout(r,50));}throw Error('Native SQL timeout');};
 const $=id=>document.getElementById(id),report={schema:1,checks:[],user_agent:navigator.userAgent};
 const query=async text=>{$('sqlText').value=text;$('sqlRun').click();await wait(()=>$('sqlCancel').hidden);if(!$('sqlError').hidden)throw Error($('sqlError').textContent);};
 try{
  await SignalAtlasApp.ready;await wait(()=>$('sqlCount').textContent&&$('sqlSchema').children.length>0);
  const expected=Number($('sqlCount').textContent.replaceAll(',',''));
  await query('SELECT count(*) AS scans FROM observations');if($('sqlResults').querySelector('td').textContent!==String(expected))throw Error('Observation adapter count mismatch');
  report.observations=expected;report.checks.push('Native SQL queries actual survey observations');
  await query("SELECT 9223372036854775807 AS exact, NULL AS missing, '<script>bad</script>' AS markup");
  const cells=[...$('sqlResults').querySelectorAll('td')].map(n=>n.textContent);if(cells[0]!=='9223372036854775807'||cells[1]!=='NULL'||$('sqlResults').querySelector('script'))throw Error('Value rendering failure');
  report.checks.push('Exact integers, NULL and markup rendered safely');
  $('sqlExport').click();let exported=false;for(let i=0;i<100;i++){const e=await(await fetch('/__bench/export-status')).json();if(e.saved&&e.has_exact_integer){report.export=e;exported=true;break};await new Promise(r=>setTimeout(r,50));}if(!exported)throw Error('Native CSV export did not reach save-bytes');report.checks.push('Native page export through production save-bytes bridge to an owned destination');

  $('sqlText').value='SELECT * FROM observations';$('sqlExplain').click();await wait(()=>$('sqlCancel').hidden);if(!$('sqlError').hidden||!$('sqlResults').querySelector('td'))throw Error('Explain failed');
  $('sqlText').value='DELETE FROM observations';$('sqlRun').click();await wait(()=>$('sqlCancel').hidden);if($('sqlError').hidden)throw Error('Write accepted');
  $('sqlText').value='WITH RECURSIVE n(v) AS (VALUES(1) UNION ALL SELECT v+1 FROM n) SELECT sum(v) FROM n';$('sqlRun').click();await wait(()=>!$('sqlCancel').hidden);await new Promise(r=>setTimeout(r,100));$('sqlCancel').click();await wait(()=>$('sqlCancel').hidden);if($('sqlError').hidden)throw Error('Cancel did not stop query');
  await query('SELECT count(*) FROM observations');report.checks.push('Explain, write rejection, native cancellation and subsequent query');
  report.success=true;
 }catch(e){report.success=false;report.error=String(e.stack||e);}
 await fetch('/__bench/report',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(report)});
})();
