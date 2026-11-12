const fs=require('node:fs'),vm=require('node:vm'),assert=require('node:assert/strict');
const html=fs.readFileSync('ui/admin.html','utf8'),source=fs.readFileSync('ui/admin.js','utf8');
const ids=[...html.matchAll(/id="([^"]+)"/g)].map(m=>m[1]);assert.equal(ids.length,new Set(ids).size,'IDs must be unique');
for(const [,id] of source.matchAll(/\$\('([^']+)'\)/g)) assert.ok(ids.includes(id),`missing element ${id}`);
const start=source.indexOf('function resultHTML('),end=source.indexOf('function renderOperation(',start);
const ctx={fi:n=>Number(n||0).toLocaleString(),esc:s=>String(s).replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('>','&gt;')};
vm.createContext(ctx);vm.runInContext(source.slice(start,end),ctx);
for(const format of ['csv','days']) {
 const output=ctx.resultHTML({saved:true,path:'export<test>',format,observations:42,day_files:['wigle_2026-01-02.csv']});
 assert.ok(output.includes('42 observations exported'));assert.ok(output.includes('export&lt;test&gt;'));
}
assert.ok(!html.includes('adminArchive'));assert.ok(!html.includes('/app/admin-archive.js'));
console.log('Admin UI checks passed: control IDs and survey CSV result rendering.');
const duplicateImport=ctx.resultHTML({files:[{filename:'overlap.csv',valid_rows:0,duplicate_rows:3,empty:true}]});
assert.ok(duplicateImport.includes('Duplicates skipped'));
assert.ok(duplicateImport.includes('<td>3</td>'));
assert.ok(duplicateImport.includes('All sightings already stored'));
