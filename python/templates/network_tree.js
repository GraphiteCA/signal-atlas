// Selection is scoped by SSID. A MAC choice under one SSID never removes a
// different SSID's observations, even when both advertise the same MAC value.
const expandedNetworks = new Set();
const macPages = new Map();
const macRequests = new Map();

function applyMacSelection(params) {
  if (!state.macSelections.size) return;
  const choices = Object.create(null);
  for (const [ssid, choice] of state.macSelections) {
    choices[ssid] = {mode: choice.mode, macs: [...choice.macs]};
  }
  params.set('mac_selection', JSON.stringify(choices));
}

function selectedMac(ssid, mac) {
  if (!selectedNetwork(ssid)) return false;
  const choice = state.macSelections.get(ssid);
  return !choice || (choice.mode === 'include' ? choice.macs.has(mac) : !choice.macs.has(mac));
}

function activateParent(ssid) {
  if (state.all) state.excluded.delete(ssid);
  else state.ssids.add(ssid);
}

function renderNetworkTree(result) {
  networkRows = result.rows;
  $('network-list').innerHTML = networkRows.map((r, i) => {
    const opened = expandedNetworks.has(r.ssid);
    return `<div class="network-node" data-network-node="${i}">
      <div class="network">
        <button class="tree-expand" data-expand-network="${i}" aria-expanded="${opened}" aria-controls="mac-children-${i}" aria-label="${opened?'Collapse':'Expand'} MAC addresses for ${esc(r.ssid||'hidden SSID')}">${opened?'▾':'▸'}</button>
        <input type="checkbox" data-network="${i}" aria-label="Select ${esc(r.ssid||'hidden SSID')}">
        <span><span class="network-name" style="border-left:3px solid ${selectedNetwork(r.ssid)?networkColor(r.ssid):'transparent'};padding-left:6px" title="${esc(r.ssid)}">${esc(r.ssid||'(hidden SSID)')}</span>
        <small>${number(r.count)} stored observations · ${number(r.macs)} MACs · ${number(r.radios)} bands/radios</small><span class="mac-bands">${bandPills(r.bands.split(','))}</span></span>
        <button class="quiet" data-only-network="${i}">Only</button>
      </div>
      <section class="mac-children" id="mac-children-${i}" ${opened?'':'hidden'} aria-label="MAC addresses under ${esc(r.ssid||'hidden SSID')}">
        <label class="sr-only" for="mac-search-${i}">Find a MAC under ${esc(r.ssid||'hidden SSID')}</label>
        <input id="mac-search-${i}" type="search" data-mac-search="${i}" placeholder="Find a MAC…" value="${esc(macPages.get(r.ssid)?.query||'')}">
        <div class="mac-list"><p class="tiny">Loading MAC addresses from SQLite…</p></div>
        <div class="pager"><button data-mac-prev="${i}">←</button><span class="mac-page"></span><button data-mac-next="${i}">→</button></div>
      </section></div>`;
  }).join('') || '<p class="tiny">No matching SSIDs.</p>';
  for (const [i, row] of networkRows.entries()) {
    const input = $('network-list').querySelector(`[data-network="${i}"]`);
    const choice = state.macSelections.get(row.ssid);
    const total = row.macs || 0;
    const count = !selectedNetwork(row.ssid) ? 0 : !choice ? total : choice.mode==='include' ? choice.macs.size : Math.max(0,total-choice.macs.size);
    input.checked = count > 0;
    input.indeterminate = count > 0 && count < total;
    if (expandedNetworks.has(row.ssid)) loadMacChildren(i);
  }
  $('network-prev').disabled = !state.networkOffset;
  $('network-next').disabled = !result.more;
  $('network-page').textContent = networkRows.length ? `${state.networkOffset+1}–${state.networkOffset+networkRows.length}` : '0';
  $('network-selection').textContent = state.all ? (state.excluded.size ? `All except ${state.excluded.size} SSIDs` : 'Showing all SSIDs') : `${state.ssids.size} SSIDs selected`;
  if (state.macSelections.size) $('network-selection').textContent += ` · ${state.macSelections.size} MAC sub-selections`;
}

async function loadMacChildren(index) {
  const parent = networkRows[index];
  const node = $('network-list').querySelector(`[data-network-node="${index}"]`);
  if (!parent || !node) return;
  const page = macPages.get(parent.ssid) || {offset:0, query:''};
  macPages.set(parent.ssid, page);
  const request = (macRequests.get(parent.ssid)||0)+1;
  macRequests.set(parent.ssid, request);
  try {
    const result = await api('/api/transmitters', new URLSearchParams({ssid:parent.ssid, q:page.query, offset:page.offset, limit:25}));
    if (macRequests.get(parent.ssid)!==request || !node.isConnected) return;
    page.rows = result.rows;
    node.querySelector('.mac-list').innerHTML = result.rows.map((r, j) => `<div class="mac-child">
      <input type="checkbox" data-mac-child="${j}" aria-label="Select MAC ${esc(r.mac)} under ${esc(parent.ssid||'hidden SSID')}" ${selectedMac(parent.ssid,r.mac)?'checked':''}>
      <span><span class="mac-name mono">${esc(r.mac)}</span><span class="mac-bands">${bandPills(r.bands.split(','))}${r.manual_band?'<span class="pill manual-band" title="Band supplied by you for observations without frequency metadata">Manual</span>':''}</span><small>${number(r.count)} stored observations · ${number(r.radio_count)} ${r.radio_count===1?'band':'bands'}</small>
      ${r.editable_band_count?`<label class="band-assignment">Set missing band<select data-band-assignment="${j}" aria-label="Set missing band for ${esc(r.mac)}"><option value="">Not assigned</option>${['2.4','5','6'].map(b=>`<option value="${b}" ${r.manual_band===b?'selected':''}>${band(b)} · manual</option>`).join('')}</select></label>`:''}</span>
      <button class="quiet" data-locate-mac="${j}" aria-label="Locate MAC ${esc(r.mac)} under ${esc(parent.ssid||'hidden SSID')}">Locate</button></div>`).join('') || '<p class="tiny">No matching MAC addresses.</p>';
    node.querySelector('[data-mac-prev]').disabled = !page.offset;
    node.querySelector('[data-mac-next]').disabled = !result.more;
    node.querySelector('.mac-page').textContent = result.rows.length ? `${page.offset+1}–${page.offset+result.rows.length} MACs` : '0 MACs';
  } catch (e) {
    if (node.isConnected) node.querySelector('.mac-list').textContent = e.message;
  }
}

async function locateMac(ssid, mac) {
  const catalogMac=macPages.get(ssid)?.rows?.find(row=>row.mac===mac);
  state.all=false;
  state.ssids=new Set([ssid]);
  state.excluded.clear();
  state.macSelections=new Map([[ssid,{mode:'include',macs:new Set([mac])}]]);
  expandedNetworks.add(ssid);
  resetRadios();
  await loadNetworks();
  await loadSelection(true);
  if(!state.summary?.count){error('This MAC has no observations within the current band, date or signal filters. Broaden those filters to locate it.');return;}
  if(catalogMac&&catalogMac.count<4){error('This MAC is selected, but at least four observations per band and three distinct receiver positions are needed to fit a location.');return;}
  try {
    await api('/api/estimate',query(),{method:'POST'});
    pollJob();
  } catch (e) { error(e.message); }
}

async function loadDatabaseInfo() {
  try {
    const info=await api('/api/database');
    $('database-name').textContent=`SQLite · ${info.filename}`;
    $('database-detail').textContent=`${number(info.observations)} stored observations · ${number(info.imports)} imports · ${number(info.cached_estimates)} saved estimates`;
  } catch (e) { error(e.message); }
}

function bindNetworkTree() {
  const root=$('network-list');
  root.addEventListener('click',ev=>{
    const expand=ev.target.closest('[data-expand-network]');
    if(expand){
      const index=+expand.dataset.expandNetwork,ssid=networkRows[index].ssid;
      expandedNetworks.has(ssid)?expandedNetworks.delete(ssid):expandedNetworks.add(ssid);
      expand.setAttribute('aria-expanded',String(expandedNetworks.has(ssid)));
      expand.textContent=expandedNetworks.has(ssid)?'▾':'▸';
      $('mac-children-'+index).hidden=!expandedNetworks.has(ssid);
      if(expandedNetworks.has(ssid))loadMacChildren(index);
      return;
    }
    const only=ev.target.closest('[data-only-network]');
    if(only){const ssid=networkRows[+only.dataset.onlyNetwork].ssid;state.all=false;state.ssids=new Set([ssid]);state.excluded.clear();state.macSelections.delete(ssid);changed();return;}
    const node=ev.target.closest('[data-network-node]');
    if(!node)return;
    const index=+node.dataset.networkNode,ssid=networkRows[index].ssid,page=macPages.get(ssid);
    const locate=ev.target.closest('[data-locate-mac]');
    if(locate){locateMac(ssid,page.rows[+locate.dataset.locateMac].mac);return;}
    if(ev.target.closest('[data-mac-prev]')){page.offset=Math.max(0,page.offset-25);loadMacChildren(index);}
    if(ev.target.closest('[data-mac-next]')){page.offset+=25;loadMacChildren(index);}
  });
  root.addEventListener('change',ev=>{
    if(ev.target.dataset.bandAssignment!=null){
      const index=+ev.target.closest('[data-network-node]').dataset.networkNode;
      const ssid=networkRows[index].ssid,mac=macPages.get(ssid).rows[+ev.target.dataset.bandAssignment].mac;
      saveRadioBand(ssid,mac,ev.target.value,ev.target);return;
    }
    if(ev.target.dataset.network!=null){
      const ssid=networkRows[+ev.target.dataset.network].ssid;
      state.macSelections.delete(ssid);
      if(state.all)ev.target.checked?state.excluded.delete(ssid):state.excluded.add(ssid);
      else ev.target.checked?state.ssids.add(ssid):state.ssids.delete(ssid);
      changed();return;
    }
    if(ev.target.dataset.macChild==null)return;
    const index=+ev.target.closest('[data-network-node]').dataset.networkNode,ssid=networkRows[index].ssid;
    const mac=macPages.get(ssid).rows[+ev.target.dataset.macChild].mac;
    let choice=state.macSelections.get(ssid);
    if(!selectedNetwork(ssid)){activateParent(ssid);choice={mode:'include',macs:new Set()};}
    if(!choice)choice={mode:'exclude',macs:new Set()};
    if(choice.mode==='include')ev.target.checked?choice.macs.add(mac):choice.macs.delete(mac);
    else ev.target.checked?choice.macs.delete(mac):choice.macs.add(mac);
    if(choice.mode==='exclude'&&!choice.macs.size)state.macSelections.delete(ssid);
    else state.macSelections.set(ssid,choice);
    changed();
  });
  root.addEventListener('input',debounce(ev=>{
    if(!ev.target.isConnected)return;
    if(ev.target.dataset.macSearch==null)return;
    const index=+ev.target.dataset.macSearch,ssid=networkRows[index]?.ssid;
    if(ssid==null)return;
    macPages.set(ssid,{offset:0,query:ev.target.value});loadMacChildren(index);
  }));
  $('all-networks').onclick=()=>{state.all=true;state.ssids.clear();state.excluded.clear();state.macSelections.clear();changed();};
  $('clear-networks').onclick=()=>{state.all=false;state.ssids.clear();state.excluded.clear();state.macSelections.clear();changed();};
}

async function saveRadioBand(ssid,mac,bandValue,input){
  input.disabled=true;
  try{
    await api('/api/band-assignment',null,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({ssid,mac,band:bandValue})});
    resetRadios();await loadDatabaseInfo();await loadNetworks();await loadSelection(true);
  }catch(e){error(e.message);input.disabled=false;input.value=macPages.get(ssid)?.rows?.find(r=>r.mac===mac)?.manual_band||'';}
}
