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

async function fixture({models} = {}) {
  const state = {instance:randomUUID(),ready:true,accounts:[{id:7,name:'Fixture GPT 账号',schedulable:true},{id:8,name:'暂停账号',schedulable:false}],prompts,stored:[],bytes:0,results:0,revision:1};
  let config = {}, record = null, modelsFail = false, healthy = true, generation = 0;
  const outputs = new Map(), calls = [], timers = new Set();
  function update() {
    const b = record?.pending || record?.latest;
    state.bytes = b ? [...outputs.values()].reduce((sum,b) => sum+b.length, 1000) : 0;
    state.results = b ? b.items.filter(i=>i.parts).length : 0;
    state.stored = b ? [{account_id:7,name:'Fixture GPT 账号',bytes:state.bytes,results:state.results,batch_id:b.id,state:b.state,created_at:b.created_at}] : [];
    state.revision++;
  }
  function later(fn, ms) { const timer = setTimeout(()=>{timers.delete(timer);fn();},ms); timers.add(timer); }
  async function dispatch(c) {
    calls.push(structuredClone(c));
    switch(c.action) {
      case 'accounts': return state.accounts;
      case 'models': if(modelsFail) throw new Error('fixture catalog unavailable'); return models ? models(c) : [{id:'fixture-model',name:'Fixture Model',efforts:['none','low','high']}];
      case 'start': {
        const b={...c,id:c.id,account_name:'Fixture GPT 账号',state:'running',created_at:new Date().toISOString(),items:[]};
        for(let round=1;round<=c.rounds;round++)for(const prompt of c.prompts)b.items.push({prompt_id:prompt,round,state:'queued',session_id:randomUUID()});
        record={account_id:7,name:'Fixture GPT 账号',pending:b,latest:record?.latest};state.active=b;update();const gen=++generation;
        function next(index) {
          if(gen!==generation)return;
          if(index===b.items.length){b.state='completed';record.latest=b;delete record.pending;delete state.active;update();return;}
          const item=b.items[index];item.state='running';update();
          later(()=>{if(gen!==generation)return;const prompt=prompts.find(p=>p.id===item.prompt_id);const answer={prompt,text:prompt.kind==='html'?pelican:'原始回答：iPhone fixture <script>不执行</script>',response_id:'fixture-response',usage:{input_tokens:20,output_tokens:50}};const blob=gzipSync(JSON.stringify(answer));outputs.set(b.id+':'+index,blob);item.parts=1;item.sha256=createHash('sha256').update(blob).digest('hex');item.state='completed';item.duration_ms=700;update();next(index+1);},700);
        }
        next(0);return {batch_id:b.id};
      }
      case 'read_batch': return record || {account_id:c.account_id};
      case 'read_result': {const blob=outputs.get(c.batch_id+':'+c.item);if(!blob)throw new Error('fixture result missing');return {encoding:'gzip-base64',data:blob.toString('base64')};}
      case 'clear_all': case 'clear_account': generation++;record=null;delete state.active;outputs.clear();update();return {clearing:true};
      case 'stop': generation++;if(state.active){state.active.state='interrupted';for(const item of state.active.items)if(['running','queued'].includes(item.state))item.state='interrupted';record.latest=record.pending;delete record.pending;delete state.active;update();}return {stopping:true};
      default: throw new Error('unknown fixture command');
    }
  }
  const server = http.createServer(async (req,res) => {
    try {
      if(req.url==='/rpc') {
        let body='';for await(const chunk of req)body+=chunk;const m=JSON.parse(body);let reply;
        if(m.type==='plugin.status')reply={ok:true,result:{healthy,status_json:healthy?JSON.stringify(state):''}};
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
  return {url:`http://127.0.0.1:${server.address().port}`,state,calls,setModelsFail(value){modelsFail=value},setHealthy(value){healthy=value},async close(){for(const t of timers)clearTimeout(t);await new Promise(resolve=>server.close(resolve));}};
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
    await ui.locator('#account').selectOption('7');await ui.locator('#model option[value="fixture-model"]').waitFor({state:'attached'});assert.equal(await ui.locator('#model').inputValue(),'');
    await ui.locator('#model').selectOption('fixture-model');assert.equal(await ui.locator('#effort').inputValue(),'');await ui.locator('#effort').selectOption('none');
    await ui.locator('#model').selectOption('');assert.equal(await ui.locator('#effort').inputValue(),'');await ui.locator('#model').selectOption('fixture-model');await ui.locator('#effort').selectOption('high');
    await ui.locator('#prompts input[value="pelican"]').check();await ui.locator('#prompts input[value="iphone"]').check();await ui.locator('#rounds').fill('2');
    assert.match(await ui.locator('#request-count').textContent(),/2 道题 × 2 轮 = 4 次请求/);await ui.locator('#start').click();await waitUntil(()=>host.calls.some(c=>c.action==='start'));
    await page.locator('#close').click();await waitUntil(()=>host.state.results===4&&!host.state.active);await page.locator('#open').click();ui=page.frameLocator('#plugin');
    await ui.locator('#account option[value="7"]').waitFor({state:'attached'});assert.equal(await ui.locator('#model').inputValue(),'');assert.equal(await ui.locator('#effort').inputValue(),'');
    await fs.mkdir(path.join(root,'test-results'),{recursive:true});
    await page.screenshot({path:path.join(root,'test-results','reopened.png'),fullPage:true});
    await ui.locator('#results-tab').click();await ui.locator('#preview iframe').waitFor();
    let animation;await waitUntil(()=>{animation=page.frames().find(f=>f.url()==='about:srcdoc');return animation});
    await waitUntil(()=>animation.evaluate(()=>document.body.dataset.executed==='yes'));
    assert.equal(await animation.evaluate(()=>document.body.dataset.isolated),'yes');
    const before=await animation.evaluate(()=>document.querySelector('#wheel').cx.animVal.value);await page.waitForTimeout(180);const after=await animation.evaluate(()=>document.querySelector('#wheel').cx.animVal.value);assert.notEqual(before,after);
    assert.equal(await page.evaluate(()=>document.body.dataset.pwned),undefined);assert.deepEqual(external,[]);
    await fs.mkdir(path.join(root,'test-results'),{recursive:true});
    await ui.locator('#answer-title').scrollIntoViewIfNeeded();
    await ui.locator('body').screenshot({path:path.join(root,'test-results','results-preview.png')});
    await ui.locator('#show-source').click();assert.equal(await ui.locator('#answer-text').textContent(),pelican);
    await page.screenshot({path:path.join(root,'test-results','results-source.png'),fullPage:true});
    await ui.locator('#items button').filter({hasText:'第 1 轮 · 最新苹果手机'}).click();await waitUntil(async()=> (await ui.locator('#answer-text').textContent()).startsWith('原始回答'));
    assert.equal(await ui.locator('#answer-text script').count(),0);
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
  host.state.accounts.push({id:9,name:'Fixture B',schedulable:true});
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
