// Shared, renderer-independent label placement and preference validation.
(function (root) {
  'use strict';
  const defaults = {clustering: true, label_min_zoom: 18, radius_m: 10, rings: true};
  function normalize(value = {}) {
    const z = Number(value.label_min_zoom), r = Number(value.radius_m);
    return {clustering: typeof value.clustering === 'boolean' ? value.clustering : true,
      label_min_zoom: Number.isInteger(z) && z >= 2 && z <= 22 ? z : 18,
      radius_m: Number.isInteger(r) && r >= 5 && r <= 50 ? r : 10,
      rings: typeof value.rings === 'boolean' ? value.rings : true};
  }
  // A screen-space hash avoids O(n²) label collisions. Symbols reserve space too;
  // pinned APs take priority. Renderer coordinates remain geographic while panning.
  function labels(rows, project, width, height, measure) {
    // Store occupied 4 px cells rather than lists of rectangles. A dense cell
    // can contain tens of thousands of coincident symbols; scanning those lists
    // for each label degenerates to quadratic work even with a spatial hash.
    const size = 4, stride = Math.ceil(height/size)+64, columns = Math.ceil(width/size)+64;
    const cells = new Int32Array(stride*columns), result = [];
    function occupy(b, owner, check) {
      for (let x=Math.floor(b[0]/size)+32;x<=Math.floor(b[2]/size)+32;x++) {
        for (let y=Math.floor(b[1]/size)+32;y<=Math.floor(b[3]/size)+32;y++) {
          const k=x*stride+y, old=cells[k];
          if (check) { if (old!==0 && old!==owner) return true; }
          else cells[k]=old===0?owner:old===owner?old:-1;
        }
      }
      return false;
    }
    const candidates = rows.map((r,i) => ({r, owner:i+1, p: project([r.lon,r.lat])})).filter(({p}) => p.x >= 0 && p.y >= 0 && p.x <= width && p.y <= height);
    for (const {r,p,owner} of candidates) { const radius = r.count > 1 ? 27 : 6; occupy([p.x-radius,p.y-radius,p.x+radius,p.y+radius],owner,false); }
    candidates.sort((a,b) => Number(!!b.r.pinned)-Number(!!a.r.pinned) || a.r.id.localeCompare(b.r.id));
    for (const {r,p,owner} of candidates) {
      if (r.count > 1) continue;
      const text = (r.label || r.ssid || '(hidden SSID)').slice(0,80), w = measure(text)+12;
      const box = [p.x+8,p.y-11,p.x+8+w,p.y+11,r.id];
      if (box[2] <= width && !occupy(box,owner,true)) { occupy(box,owner,false); result.push({...r,text}); }
    }
    return result;
  }
  const api = {defaults, normalize, labels};
  if (typeof module !== 'undefined') module.exports = api;
  else root.MapDisplay = api;
})(typeof window !== 'undefined' ? window : globalThis);
