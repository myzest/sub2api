const test=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const crypto=require('node:crypto').webcrypto;
const model=require('../ui/assets/model.js');

test('imported config removes persisted action and defaults to no routing',()=>{
  const c=model.config({command:{action:'probe'}});
  assert.equal(c.route_enabled,false);assert.deepEqual(c.account_ids,[]);assert.equal(c.command,undefined);
  assert.deepEqual(model.models('gpt-5.6-sol\ngpt-5.6-sol, gpt-6-astra'),['gpt-5.6-sol','gpt-6-astra']);
});
test('probe result distinguishes reachability from quality and shows real model',()=>{
  const text=model.probeText({state:'succeeded',account_id:7,model:'gpt-5.6-sol',effort:'high',returned_model:'actual-model',http_status:200,message:'可访问；不代表模型质量验收',latency_ms:1200});
  assert.match(text,/actual-model/);assert.match(text,/不代表模型质量/);assert.match(text,/1.2 秒/);
});
test('cached images do not hide a later upload without an HTTP response',()=>{
  const text=model.requestText({account_id:7,attachment:{uploaded:1,reused:1,attempts:2,images:[{path:'input[1].content[1]',state:'failed'}]}});
  assert.match(text,/上传尝试 2 次/);
  assert.match(text,/附件 HTTP：未收到上传响应/);
  assert.doesNotMatch(text,/缓存复用，无上传请求/);
  assert.match(text,/上传失败 · 未收到上传响应/);
});
test('bridge rejects forged messages and resolves only the matching parent/token/request',async()=>{
  const handlers={},sent=[],timers=new Map();let timerID=0;
  const parent={postMessage:(message)=>sent.push(message)};
  const context={location:{hash:'#bridge_token=fixture-session'},URLSearchParams,crypto,parent,window:{},addEventListener:(name,fn)=>handlers[name]=fn,setTimeout:fn=>{timers.set(++timerID,fn);return timerID;},clearTimeout:id=>timers.delete(id)};
  vm.runInNewContext(fs.readFileSync(require.resolve('../ui/assets/bridge-v1.js'),'utf8'),context);
  let resolved=false;const result=context.window.BasisPointsBridge.call('config.load').then(r=>{resolved=true;return r;});
  const request=sent[0];assert.equal(request.type,'config.load');assert.equal(request.bridge_token,'fixture-session');
  const response={source:'sub2api-plugin-host',bridge_token:'fixture-session',request_id:request.request_id,ok:true,config:{route_enabled:false}};
  handlers.message({source:{},data:response});handlers.message({source:parent,data:{...response,bridge_token:'wrong'}});handlers.message({source:parent,data:{...response,request_id:'wrong'}});
  await Promise.resolve();assert.equal(resolved,false);
  handlers.message({source:parent,data:response});assert.equal((await result).config.route_enabled,false);assert.equal(timers.size,0);
  const pending=context.window.BasisPointsBridge.call('plugin.status');handlers.pagehide();await assert.rejects(pending,/已关闭/);assert.equal(timers.size,0);
});
