const { test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const http = require('node:http');
const fs = require('node:fs/promises');
const path = require('node:path');
const { randomUUID, createHash } = require('node:crypto');
const { gzipSync } = require('node:zlib');

const root = path.resolve(__dirname, '..');
const uiRoot = process.env.INSPECTOR_UI_DIR || path.join(root,'ui');
const hostCSP = "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'; navigate-to 'none'";
const pelican = '<!doctype html><html><body style="margin:0;background:#edf5fc"><svg width="100%" height="300" viewBox="0 0 600 300"><rect width="600" height="300" fill="#edf5fc"/><text x="200" y="60" font-size="20">动画测试样本</text><circle id="wheel" cx="120" cy="160" r="30" fill="#315ed6"><animate attributeName="cx" values="120;460;120" dur="2s" repeatCount="indefinite"/></circle></svg><script>document.body.dataset.executed="yes";try{parent.document.body.dataset.pwned="yes"}catch(e){document.body.dataset.isolated="yes"}fetch("https://example.invalid/blocked").catch(()=>{});</script></body></html>';
const prompts = [{id:'pelican',title:'鹈鹕骑自行车',kind:'html',text:'固定鹈鹕动画提示词（浏览器夹具）'}, {id:'candy',title:'摸糖问题',kind:'text',text:'摸糖问题（浏览器夹具）'}, {id:'iphone',title:'最新苹果手机',kind:'text',text:'苹果手机问题（浏览器夹具）'}, {id:'japan_pm',title:'日本首相',kind:'text',text:'日本首相问题（浏览器夹具）'}];
const fixturePage = `<!doctype html><html><head><meta charset="utf-8"></head><body style="margin:0"><button id="close">关闭弹窗</button><button id="open">重新打开</button><div id="mount"></div><script>
function mount(){const f=document.createElement('iframe');f.id='plugin';f.sandbox='allow-scripts';f.src='/plugin/index.html#bridge_token=fixture-token';f.style='width:100%;height:1100px;border:0';document.getElementById('mount').replaceChildren(f)}
document.getElementById('close').onclick=()=>document.getElementById('mount').replaceChildren();document.getElementById('open').onclick=mount;
addEventListener('message',async event=>{const m=event.data;const f=document.getElementById('plugin');if(!f||event.source!==f.contentWindow||event.origin!=='null'||m.source!=='sub2api-plugin-ui'||m.bridge_token!=='fixture-token')return;if(m.type==='sub2api.plugin.ready')return;const r=await fetch('/rpc',{method:'POST',body:JSON.stringify(m)});const result=await r.json();event.source.postMessage({...result,source:'sub2api-plugin-host',bridge_token:'fixture-token',request_id:m.request_id},'*')});mount();
</script></body></html>`;

async function fixture({models, delay=700, hold=false, answerFor} = {}) {
  const state = {instance:randomUUID(),ready:true,accounts:[{id:7,name:'Fixture GPT 账号',schedulable:true},{id:8,name:'暂停账号',schedulable:false},{id:9,name:'Fixture B',schedulable:true}],prompts,stored:[],active:[],bytes:0,results:0,revision:1};
  let config = {}, modelsFail = false, healthy = true, frozenStatus = null;
  const records = new Map(), outputs = new Map(), calls = [], timers = new Set();
  let pendingCompletions = [];
  function update() {
    state.stored = [...records.values()].map(record=>{
      const batches=[record.latest,record.pending].filter(Boolean), b=batches.at(-1);
      const blobs=[...outputs.entries()].filter(([key])=>batches.some(b=>key.startsWith(b.id+':')));
      return {account_id:record.account_id,name:record.name,bytes:blobs.reduce((sum,[,blob])=>sum+blob.length,1000),results:batches.reduce((sum,b)=>sum+b.items.filter(i=>i.parts).length,0),batch_id:b.id,state:b.state,created_at:b.created_at};
    });
    state.bytes=state.stored.reduce((sum,r)=>sum+r.bytes,0);state.results=state.stored.reduce((sum,r)=>sum+r.results,0);
    state.revision++;
  }
  function dropOutput(batch) { if(batch)for(const key of outputs.keys())if(key.startsWith(batch.id+':'))outputs.delete(key); }
  function finish(record,b) { dropOutput(record.latest);record.latest=b;delete record.pending;state.active=state.active.filter(active=>active.id!==b.id);update(); }
  function later(account,fn) {
    if(hold){pendingCompletions.push({account,fn});return;}
    const timer = setTimeout(()=>{timers.delete(timer);fn();},delay);timers.add(timer);
  }
  async function dispatch(c) {
    calls.push(structuredClone(c));
    switch(c.action) {
      case 'accounts': return state.accounts;
      case 'models': if(modelsFail) throw new Error('fixture catalog unavailable'); return models ? models(c) : [{id:'fixture-model',name:'Fixture Model',efforts:['none','low','high']}];
      case 'start': {
        if(state.active.some(b=>b.account_id===c.account_id))throw new Error('此账号已有测试批次正在运行');
        const account=state.accounts.find(a=>a.id===c.account_id);
        const b={...c,id:c.id,account_name:account.name,state:'running',created_at:new Date().toISOString(),items:[]};
        for(let round=1;round<=c.rounds;round++)for(const prompt of c.prompts)b.items.push({prompt_id:prompt,round,state:'queued',session_id:randomUUID()});
        const record={account_id:c.account_id,name:account.name,pending:b,latest:records.get(c.account_id)?.latest};records.set(c.account_id,record);state.active.push(b);update();
        const running=()=>records.get(c.account_id)?.pending===b;
        let remaining=c.prompts.length;
        function next(index) {
          if(!running())return;
          if(index>=b.items.length){if(--remaining===0){b.state='completed';finish(record,b);}return;}
          const item=b.items[index];item.state='running';item.started_at=new Date().toISOString();update();
          later(c.account_id,()=>{if(!running())return;const prompt=prompts.find(p=>p.id===item.prompt_id);const answer={prompt,text:prompt.kind==='html'?pelican:`原始回答：账号 ${c.account_id} · ${prompt.id} · 第 ${item.round} 轮 <script>不执行</script>`,response_id:`response-${b.id}-${index}`,usage:{input_tokens:20,output_tokens:50}};if(answerFor)Object.assign(answer,answerFor(answer,item));const blob=gzipSync(JSON.stringify(answer));outputs.set(b.id+':'+index,blob);item.parts=1;item.sha256=createHash('sha256').update(blob).digest('hex');item.state=answer.error?'failed':'completed';item.error=answer.error;item.attempt=answer.attempts?.length||1;item.duration_ms=delay;update();next(index+c.prompts.length);});
        }
        for(let lane=0;lane<c.prompts.length;lane++)next(lane);return {batch_id:b.id};
      }
      case 'read_batch': return records.get(c.account_id) || {account_id:c.account_id};
      case 'read_result': {const blob=outputs.get(c.batch_id+':'+c.item);if(!blob)throw new Error('fixture result missing');return {encoding:'gzip-base64',data:blob.toString('base64')};}
      case 'clear_all': case 'clear_account': {
        for(const [id,record] of records)if(c.action==='clear_all'||id===c.account_id){dropOutput(record.latest);dropOutput(record.pending);records.delete(id);}
        state.active=state.active.filter(b=>records.has(b.account_id));update();return {clearing:true};
      }
      case 'stop': {
        const record=records.get(c.account_id),b=record?.pending;
        if(b){if(b.id!==c.batch_id)throw new Error('运行批次已变化');b.state='interrupted';for(const item of b.items)if(['running','queued'].includes(item.state))item.state='interrupted';finish(record,b);}return {stopping:true};
      }
      default: throw new Error('unknown fixture command');
    }
  }
  const server = http.createServer(async (req,res) => {
    try {
      if(req.url==='/rpc') {
        let body='';for await(const chunk of req)body+=chunk;const m=JSON.parse(body);let reply;
        if(m.type==='plugin.status')reply={ok:true,result:{healthy,status_json:healthy?JSON.stringify(frozenStatus||state):''}};
        else if(m.type==='config.save'){config=m.config;reply={ok:true,config};}
        else if(m.type==='config.test'){
          try{const command=config.command;reply={ok:true,result:{success:true,status_json:JSON.stringify({command_id:command.id,data:await dispatch(command)})}};}
          catch(e){reply={ok:false,result:{success:false,message:e.message}};}
        } else reply={ok:false,error:'unsupported fixture bridge method'};
        res.setHeader('Content-Type','application/json');res.end(JSON.stringify(reply));return;
      }
      if(req.url.startsWith('/plugin/')) {
        const relative=req.url.slice('/plugin/'.length);if(relative.includes('..'))throw new Error('invalid path');
        const content=await fs.readFile(path.join(uiRoot,relative));
        res.setHeader('Content-Security-Policy',hostCSP);res.setHeader('Cross-Origin-Resource-Policy','cross-origin');
        res.setHeader('Content-Type',relative.endsWith('.js')?'text/javascript':relative.endsWith('.css')?'text/css':'text/html');res.end(content);return;
      }
      res.setHeader('Content-Type','text/html');res.end(fixturePage);
    } catch(e) {res.statusCode=500;res.end(e.message);}
  });
  await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
  return {url:`http://127.0.0.1:${server.address().port}`,state,calls,freezeStatus(){frozenStatus=structuredClone(state)},resumeStatus(){frozenStatus=null},advance(id){const due=pendingCompletions.filter(c=>c.account===id);pendingCompletions=pendingCompletions.filter(c=>c.account!==id);for(const c of due)c.fn();},setModelsFail(value){modelsFail=value},setHealthy(value){healthy=value},async close(){for(const t of timers)clearTimeout(t);await new Promise(resolve=>server.close(resolve));}};
}
async function waitUntil(fn, timeout=15000) { const deadline=Date.now()+timeout;while(Date.now()<deadline){if(await fn())return;await new Promise(r=>setTimeout(r,30));}throw new Error('condition timed out'); }
async function launch() {
  const mac='/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
  const executablePath=process.env.CHROME_BIN || (process.platform==='darwin'?mac:undefined);
  // Avoid macOS headless GPU screenshots dropping nested opaque-frame surfaces.
  // This only affects the test browser, not the shipped UI or host permissions.
  return chromium.launch({headless:true,executablePath,args:['--disable-gpu']});
}

test('选择规则、关闭后完成、重新查看、动画隔离与清空', {timeout:60000}, async () => {
  const host=await fixture(),browser=await launch();const page=await browser.newPage({viewport:{width:1120,height:1100}});
  const pageErrors=[],external=[];page.on('pageerror',e=>pageErrors.push(e.message));page.on('request',r=>{if(!r.url().startsWith(host.url))external.push(r.url())});
  try {
    await page.goto(host.url);let ui=page.frameLocator('#plugin');await ui.locator('#account option[value="7"]').waitFor({state:'attached'});
    assert.equal(await ui.locator('#prompts input:checked').count(),0);assert.equal(await ui.locator('#model').inputValue(),'');assert.equal(await ui.locator('#effort').inputValue(),'');assert(await ui.locator('#start').isDisabled());
    assert.equal(await ui.locator('#timeout').inputValue(),'30');
    await ui.locator('#account').selectOption('7');await ui.locator('#model option[value="fixture-model"]').waitFor({state:'attached'});assert.equal(await ui.locator('#model').inputValue(),'');
    await ui.locator('#model').selectOption('fixture-model');assert.equal(await ui.locator('#effort').inputValue(),'');await ui.locator('#effort').selectOption('none');
    await ui.locator('#model').selectOption('');assert.equal(await ui.locator('#effort').inputValue(),'');await ui.locator('#model').selectOption('fixture-model');await ui.locator('#effort').selectOption('high');
    await ui.locator('#prompts input[value="pelican"]').check();await ui.locator('#prompts input[value="iphone"]').check();await ui.locator('#rounds').fill('2');
    assert.match(await ui.locator('#request-count').textContent(),/2 道题 × 2 轮 = 4 次请求/);await ui.locator('#start').click();await waitUntil(()=>host.calls.some(c=>c.action==='start'));
    assert.equal(host.calls.find(c=>c.action==='start').timeout_minutes,30);
    await page.locator('#close').click();await waitUntil(()=>host.state.results===4&&host.state.active.length===0);await page.locator('#open').click();ui=page.frameLocator('#plugin');
    await ui.locator('#account option[value="7"]').waitFor({state:'attached'});assert.equal(await ui.locator('#model').inputValue(),'');assert.equal(await ui.locator('#effort').inputValue(),'');
    await fs.mkdir(path.join(root,'test-results'),{recursive:true});
    await page.screenshot({path:path.join(root,'test-results','reopened.png'),fullPage:true});
    await ui.locator('#results-tab').click();await ui.locator('#preview iframe').waitFor();
    let animation;await waitUntil(()=>{animation=page.frames().find(f=>f.url()==='about:srcdoc');return animation});
    await waitUntil(()=>animation.evaluate(()=>document.body.dataset.executed==='yes'));
    assert.equal(await animation.evaluate(()=>document.body.dataset.isolated),'yes');
    const before=await animation.evaluate(()=>document.querySelector('#wheel').cx.animVal.value);await page.waitForTimeout(180);const after=await animation.evaluate(()=>document.querySelector('#wheel').cx.animVal.value);assert.notEqual(before,after);
    assert.equal(await page.evaluate(()=>document.body.dataset.pwned),undefined);assert.deepEqual(external,[]);
    await ui.locator('#new-tab').click();await ui.locator('#results-tab').click();await ui.locator('#preview iframe').waitFor();assert(await ui.locator('#answer-text').isHidden());
    await fs.mkdir(path.join(root,'test-results'),{recursive:true});
    await ui.locator('#answer-title').scrollIntoViewIfNeeded();
    await ui.locator('body').screenshot({path:path.join(root,'test-results','results-preview.png')});
    await ui.locator('#show-source').click();assert.equal(await ui.locator('#answer-text').textContent(),pelican);
    await page.screenshot({path:path.join(root,'test-results','results-source.png'),fullPage:true});
    await ui.locator('#items button').filter({hasText:'第 1 轮 · 最新苹果手机'}).click();await waitUntil(async()=> (await ui.locator('#answer-text').textContent()).startsWith('原始回答'));
    assert.equal(await ui.locator('#answer-text script').count(),0);
    const firstText=await ui.locator('#answer-text').textContent(),firstInfo=JSON.parse(await ui.locator('#answer-usage').textContent());
    await ui.locator('#items button').filter({hasText:'第 2 轮 · 最新苹果手机'}).click();
    await waitUntil(async()=> (await ui.locator('#answer-text').textContent()).includes('第 2 轮'));
    const secondInfo=JSON.parse(await ui.locator('#answer-usage').textContent());
    assert.notEqual(await ui.locator('#answer-text').textContent(),firstText);assert.notEqual(secondInfo.response_id,firstInfo.response_id);assert.notEqual(secondInfo.session_id,firstInfo.session_id);
    await ui.locator('#items button').filter({hasText:'第 1 轮 · 最新苹果手机'}).click();await waitUntil(async()=>await ui.locator('#answer-text').textContent()===firstText);
    await page.screenshot({path:path.join(root,'test-results','results-text.png'),fullPage:true});
    await ui.locator('#clear-all').click();await ui.locator('#confirm-ok').click();await waitUntil(()=>host.state.bytes===0);await waitUntil(async()=>await ui.locator('#storage-total').textContent()==='0 B');
    assert.equal(await ui.locator('#empty').isVisible(),true);assert.equal(host.calls.filter(c=>c.action==='start').length,1);assert.deepEqual(pageErrors,[]);
  } finally {await browser.close();await host.close();}
});

test('模型获取失败无静默回退，切换账号清空模型和档位', {timeout:30000}, async () => {
  const host=await fixture(),browser=await launch();const page=await browser.newPage();
  try {
    host.setModelsFail(true);await page.goto(host.url);const ui=page.frameLocator('#plugin');await ui.locator('#account option[value="7"]').waitFor({state:'attached'});
    await ui.locator('#account').selectOption('7');await waitUntil(async()=> (await ui.locator('#model-note').textContent()).includes('获取失败'));
    assert.equal(await ui.locator('#model').inputValue(),'');assert(await ui.locator('#start').isDisabled());
    host.setModelsFail(false);await ui.locator('#retry-models').click();await ui.locator('#model option[value="fixture-model"]').waitFor({state:'attached'});
    await ui.locator('#model').selectOption('fixture-model');await ui.locator('#effort').selectOption('none');await ui.locator('#account').selectOption('');
    assert.equal(await ui.locator('#model').inputValue(),'');assert.equal(await ui.locator('#effort').inputValue(),'');assert(await ui.locator('#start').isDisabled());
    await fs.mkdir(path.join(root,'test-results'),{recursive:true});await page.screenshot({path:path.join(root,'test-results','new-test.png'),fullPage:true});
  } finally {await browser.close();await host.close();}
});

test('快速切换 A→B→A 时丢弃旧目录，模型和档位保持未选', {timeout:30000}, async () => {
  let release, requests=0;
  const gate=new Promise(resolve=>{release=resolve});
  const host=await fixture({models:async c=>{
    if(++requests===1){await gate;return [{id:'stale-model',efforts:['low']}];}
    return [{id:c.account_id===7?'fresh-model':'other-model',efforts:['none','high']}];
  }}),browser=await launch();const page=await browser.newPage();
  try {
    await page.goto(host.url);const ui=page.frameLocator('#plugin');await ui.locator('#account option[value="7"]').waitFor({state:'attached'});
    await ui.locator('#account').selectOption('7');await waitUntil(()=>requests===1);
    await ui.locator('#account').selectOption('9');await ui.locator('#account').selectOption('7');release();
    await ui.locator('#model option[value="fresh-model"]').waitFor({state:'attached'});
    assert.deepEqual(await ui.locator('#model option').evaluateAll(options=>options.map(o=>o.value)),['','fresh-model']);
    assert.equal(await ui.locator('#model').inputValue(),'');assert.equal(await ui.locator('#effort').inputValue(),'');
    assert(await ui.locator('#start').isDisabled());
  } finally {release();await browser.close();await host.close();}
});

test('插件停用后清除过期模型和档位，重启必须重新选择', {timeout:30000}, async () => {
  const host=await fixture(),browser=await launch();const page=await browser.newPage();
  try {
    await page.goto(host.url);const ui=page.frameLocator('#plugin');await ui.locator('#account option[value="7"]').waitFor({state:'attached'});
    await ui.locator('#account').selectOption('7');await ui.locator('#model option[value="fixture-model"]').waitFor({state:'attached'});
    await ui.locator('#model').selectOption('fixture-model');await ui.locator('#effort').selectOption('high');
    host.setHealthy(false);await waitUntil(async()=> (await ui.locator('#notice').textContent()).includes('请先在插件管理页启用'));
    assert.equal(await ui.locator('#model').inputValue(),'');assert.equal(await ui.locator('#effort').inputValue(),'');
    host.state.instance=randomUUID();host.setHealthy(true);
    await waitUntil(async()=>!(await ui.locator('#refresh-accounts').isDisabled()));
    assert.equal(await ui.locator('#model').inputValue(),'');assert.equal(await ui.locator('#effort').inputValue(),'');assert(await ui.locator('#start').isDisabled());
  } finally {await browser.close();await host.close();}
});

test('三题并行状态、已用时间与不限时选项', {timeout:30000}, async () => {
  const host=await fixture({delay:10000}),browser=await launch();const page=await browser.newPage({viewport:{width:1120,height:1100}});
  try {
    await page.goto(host.url);const ui=page.frameLocator('#plugin');await ui.locator('#account option[value="7"]').waitFor({state:'attached'});
    await ui.locator('#account').selectOption('7');await ui.locator('#model option[value="fixture-model"]').waitFor({state:'attached'});
    await ui.locator('#model').selectOption('fixture-model');await ui.locator('#effort').selectOption('high');
    for(const id of ['pelican','iphone','japan_pm'])await ui.locator(`#prompts input[value="${id}"]`).check();
    await ui.locator('#rounds').fill('2');await ui.locator('#timeout').selectOption('0');
    assert.match(await ui.locator('#request-count').textContent(),/6 次请求 · 最多 3 题同时运行/);
    await ui.locator('#start').click();await waitUntil(()=>host.calls.some(c=>c.action==='start'));
    assert.equal(host.calls.find(c=>c.action==='start').timeout_minutes,0);
    await waitUntil(async()=> (await ui.locator('.task-card[data-account-id="7"] .task-detail').textContent()).includes('同时运行 3 题'));
    const detail=await ui.locator('.task-card[data-account-id="7"] .task-detail').textContent();
    for(const title of ['鹈鹕骑自行车','最新苹果手机','日本首相','已用','单题不限时'])assert(detail.includes(title),detail);
    assert(!detail.includes('NaN'));
    await fs.mkdir(path.join(root,'test-results'),{recursive:true});await page.screenshot({path:path.join(root,'test-results','parallel-running.png'),fullPage:true});
    await ui.locator('.task-card[data-account-id="7"] [data-stop-batch]').click();await waitUntil(()=>host.state.active.length===0);
  } finally {await browser.close();await host.close();}
});

test('运行中添加另一账号、重开弹窗、单账号完成刷新及结果隔离', {timeout:45000}, async () => {
  const host=await fixture({hold:true}),browser=await launch();const page=await browser.newPage({viewport:{width:1120,height:1200}});
  const pageErrors=[];page.on('pageerror',e=>pageErrors.push(e.message));
  try {
    await page.goto(host.url);let ui=page.frameLocator('#plugin');await ui.locator('#account option[value="7"]').waitFor({state:'attached'});
    for(const id of [7,9]) {
      await ui.locator('#account').selectOption(String(id));await ui.locator('#model option[value="fixture-model"]').waitFor({state:'attached'});
      await ui.locator('#model').selectOption('fixture-model');await ui.locator('#effort').selectOption('high');
      await ui.locator('#prompts input[value="iphone"]').check();await ui.locator('#rounds').fill('2');
      await waitUntil(async()=>!(await ui.locator('#start').isDisabled()));await ui.locator('#start').click();
      await waitUntil(()=>host.state.active.some(b=>b.account_id===id));
    }
    await waitUntil(async()=>await ui.locator('.task-card').count()===2);
    assert.equal(host.calls.filter(c=>c.action==='start').length,2);
    await page.locator('#close').click();assert.equal(host.state.active.length,2);await page.locator('#open').click();ui=page.frameLocator('#plugin');
    await waitUntil(async()=>await ui.locator('.task-card').count()===2);
    await ui.locator('#account').selectOption('7');await ui.locator('#model option[value="fixture-model"]').waitFor({state:'attached'});
    await ui.locator('#model').selectOption('fixture-model');await ui.locator('#effort').selectOption('high');await ui.locator('#prompts input[value="iphone"]').check();
    assert(await ui.locator('#start').isDisabled());assert.equal(await ui.locator('#start').textContent(),'该账号测试中');
    await fs.mkdir(path.join(root,'test-results'),{recursive:true});await page.screenshot({path:path.join(root,'test-results','multiple-accounts.png'),fullPage:true});
    await ui.locator('#results-tab').click();await waitUntil(async()=>await ui.locator('#items button').count()===2);
    host.advance(7);host.advance(7);
    await waitUntil(async()=> (await ui.locator('#batch-info').textContent()).includes('已完成'));
    assert.equal(host.state.active.length,1);assert.equal(host.state.active[0].account_id,9);
    await waitUntil(async()=> (await ui.locator('#answer-text').textContent()).includes('账号 7'));
    await ui.locator('#items button').filter({hasText:'第 2 轮'}).click();await waitUntil(async()=> (await ui.locator('#answer-text').textContent()).includes('第 2 轮'));
    const aText=await ui.locator('#answer-text').textContent();
    host.advance(9);host.advance(9);
    await waitUntil(async()=>await ui.locator('.task-card').count()===0);
    await ui.locator('#result-account').selectOption('9');await waitUntil(async()=> (await ui.locator('#answer-text').textContent()).includes('账号 9'));
    await ui.locator('#items button').filter({hasText:'第 2 轮'}).click();await waitUntil(async()=> (await ui.locator('#answer-text').textContent()).includes('账号 9 · iphone · 第 2 轮'));
    assert.notEqual(await ui.locator('#answer-text').textContent(),aText);
    await ui.locator('#result-account').selectOption('7');await ui.locator('#items button').filter({hasText:'第 2 轮'}).click();await waitUntil(async()=>await ui.locator('#answer-text').textContent()===aText);
    assert.deepEqual(pageErrors,[]);
  } finally {await browser.close();await host.close();}
});

test('停止和清理一个账号不影响另一个，全清停止所有账号', {timeout:45000}, async () => {
  const host=await fixture({hold:true}),browser=await launch();const page=await browser.newPage();
  try {
    await page.goto(host.url);const ui=page.frameLocator('#plugin');await ui.locator('#account option[value="7"]').waitFor({state:'attached'});
    async function start(id) {
      await ui.locator('#account').selectOption(String(id));await ui.locator('#model option[value="fixture-model"]').waitFor({state:'attached'});
      await ui.locator('#model').selectOption('fixture-model');await ui.locator('#effort').selectOption('high');await ui.locator('#prompts input[value="iphone"]').check();
      await ui.locator('#start').click();await waitUntil(()=>host.state.active.some(b=>b.account_id===id));
    }
    await start(7);await start(9);await waitUntil(async()=>await ui.locator('.task-card').count()===2);
    const aBatch=host.state.active.find(b=>b.account_id===7).id;
    await ui.locator('.task-card[data-account-id="7"] [data-stop-batch]').click();
    await waitUntil(async()=>await ui.locator('.task-card').count()===1);
    const stop=host.calls.find(c=>c.action==='stop');assert.equal(stop.account_id,7);assert.equal(stop.batch_id,aBatch);assert.equal(host.state.active[0].account_id,9);
    await start(7);await waitUntil(async()=>await ui.locator('.task-card').count()===2);
    await ui.locator('#results-tab').click();await ui.locator('#result-account').selectOption('7');
    await ui.locator('#clear-account').click();await ui.locator('#confirm-ok').click();
    await waitUntil(async()=>await ui.locator('.task-card').count()===1);
    assert.equal(host.state.active[0].account_id,9);assert(!host.state.stored.some(r=>r.account_id===7));
    await ui.locator('#new-tab').click();await start(7);await waitUntil(async()=>await ui.locator('.task-card').count()===2);
    await ui.locator('#results-tab').click();await ui.locator('#clear-all').click();await ui.locator('#confirm-ok').click();
    await waitUntil(async()=>await ui.locator('#storage-total').textContent()==='0 B');
    host.advance(7);host.advance(9);assert.equal(host.state.active.length,0);assert.equal(host.state.results,0);assert.equal(host.state.bytes,0);
  } finally {await browser.close();await host.close();}
});

test('鹈鹕默认渲染 HTML 片段和代码围栏，切回结果恢复预览，原文保持不变', {timeout:45000}, async () => {
  const fragment='<div id="scene">鹈鹕动画片段</div><script>document.querySelector("#scene").dataset.playing="yes"</script>';
  const samples=[fragment,'下面是动画：\n```html\n'+fragment+'\n```\n以上是源码。','```html\n'+fragment];
  const host=await fixture({answerFor:(_,item)=>({text:samples[item.round-1]})}),browser=await launch();const page=await browser.newPage();
  try {
    await page.goto(host.url);let ui=page.frameLocator('#plugin');await ui.locator('#account option[value="7"]').waitFor({state:'attached'});
    await ui.locator('#account').selectOption('7');await ui.locator('#model option[value="fixture-model"]').waitFor({state:'attached'});
    await ui.locator('#model').selectOption('fixture-model');await ui.locator('#effort').selectOption('high');await ui.locator('#prompts input[value="pelican"]').check();await ui.locator('#rounds').fill('3');
    await ui.locator('#start').click();await waitUntil(()=>host.state.results===3&&host.state.active.length===0);
    await page.locator('#close').click();await page.locator('#open').click();ui=page.frameLocator('#plugin');await ui.locator('#account option[value="7"]').waitFor({state:'attached'});
    await ui.locator('#results-tab').click();
    for(let round=1;round<=3;round++) {
      await ui.locator('#items button').filter({hasText:`第 ${round} 轮`}).click();await ui.locator('#preview iframe').waitFor();
      const animation=ui.frameLocator('#preview iframe');await animation.locator('#scene[data-playing="yes"]').waitFor();
      assert(await ui.locator('#answer-text').isHidden());assert.equal((await animation.locator('body').innerText()).trim(),'鹈鹕动画片段');
      await ui.locator('#show-source').click();assert.equal(await ui.locator('#answer-text').textContent(),samples[round-1]);
      await ui.locator('#new-tab').click();await ui.locator('#results-tab').click();await animation.locator('#scene[data-playing="yes"]').waitFor();assert(await ui.locator('#answer-text').isHidden());
    }
  } finally {await browser.close();await host.close();}
});

test('重试成功和耗尽均展示尝试次数与历史错误', {timeout:30000}, async () => {
  const attempts=[{session_id:randomUUID(),response_id:'failed-response',duration_ms:1000,error_code:'stream_interrupted',error:'上游响应流读取中断（unexpected EOF）'},{session_id:randomUUID(),response_id:'last-response',duration_ms:1500}];
  const host=await fixture({answerFor:(_,item)=>item.round===1?{text:'重试成功的回答',session_id:attempts[1].session_id,attempts}:{text:'',session_id:attempts[1].session_id,error:'上游响应流读取中断（unexpected EOF）',error_code:'stream_interrupted',attempts:[attempts[0],{...attempts[1],error_code:'stream_interrupted',error:'上游响应流读取中断（unexpected EOF）'}]}}),browser=await launch();const page=await browser.newPage();
  try {
    await page.goto(host.url);let ui=page.frameLocator('#plugin');await ui.locator('#account option[value="7"]').waitFor({state:'attached'});
    await ui.locator('#account').selectOption('7');await ui.locator('#model option[value="fixture-model"]').waitFor({state:'attached'});
    await ui.locator('#model').selectOption('fixture-model');await ui.locator('#effort').selectOption('high');await ui.locator('#prompts input[value="iphone"]').check();await ui.locator('#rounds').fill('2');
    await ui.locator('#start').click();await waitUntil(()=>host.state.results===2&&host.state.active.length===0);
    await page.locator('#close').click();await page.locator('#open').click();ui=page.frameLocator('#plugin');await ui.locator('#account option[value="7"]').waitFor({state:'attached'});await ui.locator('#results-tab').click();
    await waitUntil(async()=>await ui.locator('#answer-text').textContent()==='重试成功的回答');
    assert.match(await ui.locator('#answer-retry').textContent(),/共尝试 2 次.*已收到完整回答/);
    const info=JSON.parse(await ui.locator('#answer-usage').textContent());assert.equal(info.session_id,attempts[1].session_id);assert.equal(info.attempts[0].error_code,'stream_interrupted');assert(await ui.locator('#answer-error').isHidden());
    await ui.locator('#items button').filter({hasText:'第 2 轮'}).click();await waitUntil(async()=> (await ui.locator('#answer-retry').textContent()).includes('最后一次未完成'));
    assert(await ui.locator('#answer-error').isVisible());assert.match(await ui.locator('#answer-error').textContent(),/unexpected EOF/);
  } finally {await browser.close();await host.close();}
});

test('新批次在两次轮询之间完成时自动加载新回答，不停留在上一批', {timeout:35000}, async () => {
  let answerNumber=0;
  const host=await fixture({hold:true,answerFor:()=>({text:`本次上游实际回答 ${++answerNumber}`})}),browser=await launch();const page=await browser.newPage();
  try {
    await page.goto(host.url);const ui=page.frameLocator('#plugin');await ui.locator('#account option[value="7"]').waitFor({state:'attached'});
    await ui.locator('#account').selectOption('7');await ui.locator('#model option[value="fixture-model"]').waitFor({state:'attached'});
    await ui.locator('#prompts input[value="iphone"]').check();
    async function startAndFinish() {
      await ui.locator('#model').selectOption('fixture-model');await ui.locator('#effort').selectOption('high');
      const count=host.calls.filter(c=>c.action==='start').length;
      await ui.locator('#start').click();await waitUntil(()=>host.calls.filter(c=>c.action==='start').length===count+1);
      host.advance(7);assert.equal(host.state.active.length,0);
    }
    await startAndFinish();await waitUntil(async()=> (await ui.locator('#result-count').textContent())==='1');
    await ui.locator('#results-tab').click();await waitUntil(async()=>await ui.locator('#answer-text').textContent()==='本次上游实际回答 1');
    const oldInfo=JSON.parse(await ui.locator('#answer-usage').textContent());
    // Simulate a short request (or background-tab throttling): no poll ever
    // observes the new batch while it is active.
    const reads=host.calls.filter(c=>c.action==='read_batch').length;
    host.freezeStatus();await ui.locator('#new-tab').click();await startAndFinish();host.resumeStatus();
    await waitUntil(()=>host.calls.filter(c=>c.action==='read_batch').length>reads,9000);
    await ui.locator('#results-tab').click();
    await waitUntil(async()=>await ui.locator('#answer-text').textContent()==='本次上游实际回答 2',9000);
    const newInfo=JSON.parse(await ui.locator('#answer-usage').textContent());
    assert.notEqual(newInfo.session_id,oldInfo.session_id);assert.notEqual(newInfo.response_id,oldInfo.response_id);
    assert.equal(host.calls.filter(c=>c.action==='start').length,2);
  } finally {await browser.close();await host.close();}
});
