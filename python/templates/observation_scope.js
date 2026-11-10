let observationDateRequest = 0;

function applyObservationDates(params) {
  if(state.dateMode==='range'){
    if($('date-from').value)params.set('date_from',$('date-from').value);
    if($('date-to').value)params.set('date_to',$('date-to').value);
  }else if(state.dateMode==='dates'){
    if(!state.selectedDates.size)params.set('none','1');
    for(const day of state.selectedDates)params.append('date',day);
  }
}

function estimateScopeLabel(scope) {
  if(!scope)return 'Legacy observation scope';
  if(scope.dates?.length)return scope.dates.join(', ');
  if(scope.date_from||scope.date_to)return `${scope.date_from||'earliest'} through ${scope.date_to||'latest'}`;
  return 'All recorded dates';
}

async function loadObservationDates() {
  const request=++observationDateRequest;
  try{
    const result=await api('/api/dates',query(true,false));
    if(request!==observationDateRequest)return;
    const days=new Map(result.dates.map(row=>[row.date,row.count]));
    for(const day of state.selectedDates)if(!days.has(day))days.set(day,0);
    $('survey-dates').innerHTML=[...days.entries()].sort((a,b)=>a[0].localeCompare(b[0])).map(([day,count])=>
      `<option value="${esc(day)}" ${state.selectedDates.has(day)?'selected':''}>${esc(day)} · ${number(count)} observations</option>`).join('');
    $('survey-date-count').textContent=`${state.selectedDates.size} survey dates selected`;
  }catch(e){if(request===observationDateRequest)error(e.message);}
}

function bindObservationScope() {
  function changed(){state.recordOffset=0;state.radioOffset=0;loadSelection(true);}
  $('date-mode').onchange=()=>{
    state.dateMode=$('date-mode').value;
    $('date-range-fields').hidden=state.dateMode!=='range';
    $('specific-date-fields').hidden=state.dateMode!=='dates';
    changed();
  };
  for(const id of ['date-from','date-to'])$(id).onchange=changed;
  $('survey-dates').onchange=()=>{
    state.selectedDates=new Set([...$('survey-dates').selectedOptions].map(option=>option.value));
    $('survey-date-count').textContent=`${state.selectedDates.size} survey dates selected`;
    changed();
  };
  $('fit-mode').onchange=()=>{state.fitMode=$('fit-mode').value;loadSelection();};
}
