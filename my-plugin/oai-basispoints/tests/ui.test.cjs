const test=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const crypto=require('node:crypto').webcrypto;
const model=require('../ui/assets/model.js');

test('agent diagnostics distinguish explicit plaintext from missing encryption metadata',()=>{
  const parent=model.requestText({output_tool_calls:1,output_tools:[{type:'function_call',name:'collaboration.spawn_agent',argument_encryption:'plaintext'}]});
  assert.match(parent,/明确明文（encrypted_function_args: \[\]）/);
  assert.match(parent,/不等于客户端已执行/);
  const old=model.requestText({output_tool_calls:1,output_tools:[{type:'function_call',name:'collaboration.spawn_agent'}]});
  assert.doesNotMatch(old,/明确明文/);
  const child=model.requestText({error_source:'plugin_agent_message',client_http_status:400,responses_started:false,agent_input:{messages:1,encrypted_parts:1,encrypted_paths:['input[3].content[0]']},error:'bps_agent_encrypted_content'});
  assert.match(child,/错误来源：插件子任务消息校验/);
  assert.match(child,/agent_message 1 项 · encrypted_content 1 段/);
  assert.match(child,/input\[3\].content\[0\]/);assert.match(child,/Responses 发送阶段：尚未开始/);
  assert.match(child,/不代表 BPS 拒绝/);assert.match(child,/升级后可保留会话重试/);assert.doesNotMatch(child,/已解密|已修复密文/);
  const native=model.requestText({output_tools:[{type:'function_call',name:'direct',argument_encryption:'declared'}]});
  assert.match(native,/保留原生加密字段声明（未验证密文）/);
});

test('upstream stream failures display raw evidence without claiming completion',()=>{
  const raw=JSON.stringify({code:'capacity_fixture',type:'server_error',message:"fixture 'raw input' https://fixture.invalid/private?q=1"});
  const text=model.requestText({account_id:58,http_status:200,client_http_status:200,error_source:'upstream_stream',terminal:'error',upstream_error_event:'error',upstream_error_state:'captured_raw',upstream_error:raw,error:'bps_upstream_event',response_attempts:1,protocol_retries:0,output_tool_calls:0});
  assert.match(text,/错误来源：BPS 流内错误事件/);
  assert.match(text,/上游错误事件：error/);assert.match(text,/结束状态：error/);
  assert.match(text,/已采集错误字段原文（未脱敏）/);assert(text.includes(raw));
  assert.match(text,/HTTP 200 不代表完成/);assert.match(text,/分享记录前请自行检查/);
  assert.doesNotMatch(text,/错误来源：响应转换|已生成客户端工具调用/);
  const old=model.requestText({upstream_error_state:'captured'});
  assert.match(old,/旧记录/);assert.doesNotMatch(old,/已采集错误字段原文/);
});

test('marked relay diagnostics distinguish raw code from metadata JSON',()=>{
  const text=model.requestText({relay:[{state:'decoded',unwrapped:0,transport:'custom',fields:[{field:'code',type:'string',bytes:2350}]},{state:'rejected',unwrapped:0,transport:'function_code',error_kind:'duplicate_code',fields:[]}],terminal:'response.failed',error:'bps_tool_envelope_invalid',error_source:'tool_relay'});
  assert.match(text,/CUSTOM 原文/);assert.match(text,/FUNCTION_CODE 原文/);
  assert.match(text,/解析记录 2/);assert.match(text,/response.failed/);
  assert.doesNotMatch(text,/重试成功|已执行工具/);
});

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
test('diagnostic separates local rejection, relay failure and recovered history',()=>{
  const local=model.requestText({account_id:7,model:'gpt-6-sol',stage:'prepare',error_source:'plugin_local_config',responses_started:false,client_http_status:400,error:'bps_model_not_allowed'});
  assert.match(local,/插件本地模型配置/);assert.match(local,/Responses 发送阶段：尚未开始/);
  const failed=model.requestText({account_id:7,http_status:200,client_http_status:200,error_source:'tool_relay',error:'decode failed',relay:[{state:'rejected',unwrapped:1,error_kind:'invalid_escape',fields:[{layer:1,field:'code',type:'string',bytes:90,error_kind:'invalid_escape',error_offset:20}]}]});
  assert.match(failed,/HTTP 200 不代表完成/);assert.match(failed,/invalid_escape/);assert.match(failed,/字节偏移 20/);
  const recovered=model.requestText({account_id:7,replay:{scope_fingerprint:'aabbcc',native_hits:0,missing:1,rebuilt:1}});
  assert.match(recovered,/缺失原生记录 1/);assert.match(recovered,/未重新执行旧工具/);
});
test('compatibility diagnostics retain earlier refusal and distinguish omitted pictures from success',()=>{
  const value=model.requestText({account_id:7,http_status:200,terminal:'response.completed',response_attempts:3,protocol_retries:0,image_fallbacks:['refresh_cached_attachments','omit_rejected_images'],response_retries:[{attempt:1,http_status:400,reason:'refresh_cached_attachments',request_id:'refused-first',upstream_error:'Invalid input format'}],omitted_images:1,completion_recovered:true,keepalives:2,skipped_tools:1,relay:[{state:'decoded',unwrapped:0,fields:[{layer:0,field:'code',type:'string',bytes:80,extracted:true,backslashes_repaired:1}]}]});
  assert.match(value,/HTTP 400/);assert.match(value,/refused-first/);assert.match(value,/不能算作识图成功/);
  assert.match(value,/未伪造 usage/);assert.match(value,/反斜杠兼容 1 处/);assert.match(value,/已提取完整 JSON 对象/);
  const image=model.requestText({account_id:7,image_operation:'generations',http_status:200,client_http_status:200,error:'no b64_json',generated_images:0});
  assert.match(image,/HTTP 200 不代表出图/);assert.doesNotMatch(image,/流内失败/);
});

test('client tool trace distinguishes queued acknowledgements from verified previews',()=>{
  const text=model.requestText({account_id:7,callable_tools:1,output_tool_calls:1,output_tools:[{type:'custom_tool_call',name:'functions.exec'}],client_tool_history_count:2,client_tool_history:[{input_index:2,type:'function_call',name:'mcp__codex_app.open_in_codex'},{input_index:3,type:'function_call_output',name:'mcp__codex_app.open_in_codex',output_format:'object',signals:['output.structuredContent.status=queued']}]});
  assert.match(text,/转换后.*不等于客户端已执行/);assert.match(text,/custom_tool_call · functions.exec/);
  assert.match(text,/input\[3\].*结果/);assert.match(text,/status=queued/);
  assert.match(text,/不能证明已渲染预览/);assert.match(text,/不推断嵌套执行器内部调用/);
  const unknown=model.requestText({account_id:7,client_tool_history_count:1,client_tool_history:[{input_index:1,type:'custom_tool_call_output',output_format:'string'}]});
  assert.match(unknown,/不能据此判断成功、失败或策略拦截/);
  assert.doesNotMatch(model.requestText({account_id:7}),/客户端工具回传链/);
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


test('probe results identify their own source, ID and time instead of client route errors',()=>{
  const text=model.probeText({id:'probe-current',kind:'text',state:'succeeded',started_at:1790404000,finished_at:1790404002,account_id:58,model:'gpt-6-astra',effort:'xhigh',http_status:200,message:'fixture completed'});
  assert.match(text,/请求来源：插件主动文本探测/);assert.match(text,/探测 ID：probe-current/);
  assert.match(text,/开始时间：/);assert.match(text,/结束时间：/);assert.match(text,/不读取 Codex 会话/);
  const route=model.requestText({id:'route-other',origin:'route',error_source:'plugin_agent_message',error:'bps_agent_encrypted_content'});
  assert.match(route,/不是“保存并探测文本”的结果/);assert.doesNotMatch(text,/bps_agent_encrypted_content|route-other/);
});

test('opaque agent content preservation never claims decryption or old history',()=>{
  const text=model.requestText({origin:'route',responses_started:false,agent_input:{messages:25,encrypted_parts:16,encrypted_paths:['input[63].content[1]'],handling:'preserved'}});
  assert.match(text,/原始加密字段及消息顺序已保留/);assert.match(text,/未解密/);assert.match(text,/尚未开始/);
  assert.doesNotMatch(text,/不要反复重试旧子任务|已解密|必须新建/);
  const unknown=model.requestText({agent_input:{messages:1,encrypted_parts:1}});assert.match(unknown,/未提供处理结果/);
});

test('image route probe history points back to its own probe instead of a client request',()=>{
  const text=model.requestText({id:'image-route-id',origin:'image_route_probe'});
  assert.match(text,/记录归属：插件主动图片路由探测/);
  assert.match(text,/路由诊断 ID 核对/);
  assert.doesNotMatch(text,/记录归属：客户端请求/);
  const probe=model.probeText({id:'probe-image-id',kind:'image_route',state:'succeeded',route_diagnostic_id:'image-route-id'});
  assert.match(probe,/探测 ID：probe-image-id/);
  assert.match(probe,/路由诊断 ID：image-route-id/);
});
