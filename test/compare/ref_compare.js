// Run the compare page's own analysis worker (web/compare.html, script#wk) in Node
// on the given logs and configurations; print every result (and the first drive's export) as JSON.
//   node ref_compare.js <page> <dir with the logs> <cases.json>
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const [page, dir, casesFile] = process.argv.slice(2);
const src = /<script type="text\/plain" id="wk">([\s\S]*?)<\/script>/.exec(fs.readFileSync(page, 'utf8'))[1];
let waiting = null;
globalThis.postMessage = (m) => { if (m.type !== 'progress' && waiting) waiting(m); };
vm.runInThisContext(src);
const send = (m) => new Promise((resolve) => {
  const got = [];
  waiting = (r) => { got.push(r); if (r.type === 'result' || r.type === 'exported' || r.type === 'error') { waiting = null; resolve(got); } };
  globalThis.onmessage({ data: m });
});

(async () => {
  const cases = JSON.parse(fs.readFileSync(casesFile, 'utf8'));
  const out = { results: [], exports: [] };
  const files = await Promise.all(cases.files.map(async (n) => {
    const b = await fs.openAsBlob(path.join(dir, n));
    return { name: n, size: b.size, stream: () => b.stream() };
  }));
  const added = await send({ type: 'add', files, cfg: cases.cfgs[0] });
  out.added = added.find((m) => m.type === 'added');
  for (const cfg of cases.cfgs) {
    const r = await send({ type: 'analyse', cfg });
    const res = r[r.length - 1];
    if (res.result) delete res.result.ms;
    out.results.push(res);
    if (res.result && res.result.drives.length) {
      const ex = await send({ type: 'export', key: res.result.drives[0].key, id: 1 });
      const m = ex[ex.length - 1];
      out.exports.push(m.blob ? { rows: m.rows, dropped: m.dropped, text: await m.blob.text() } : m);
    }
  }
  process.stdout.write(JSON.stringify(out));
})();
