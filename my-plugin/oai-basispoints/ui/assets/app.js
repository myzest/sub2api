(() => {
  'use strict';
  const bridge=window.BasisPointsBridge, model=window.BasisPointsModel, $=id=>document.getElementById(id);
  let saved=model.config({}), snapshot=null, busy=false, timer=null, closed=false, observedInstance='';
  let statusTail=Promise.resolve();
  let statusFailed=false;
  const diagnosticSelections={request:{id:'',record:null},image:{id:'',record:null}};
  function notice(text,error=false){$('notice').textContent=text;$('notice').classList.toggle('error',error);}
  function buttons(){
    const running=snapshot?.probe?.state==='running';
    for(const id of ['save','refresh','probe','probe-image','probe-image-route','probe-tools'])$(id).disabled=busy||!snapshot?.host_ready||(id.startsWith('probe')&&running);
    for(const id of ['refresh-diagnostic','refresh-image-diagnostic'])$(id).disabled=busy||!snapshot;
    for(const id of ['clear-diagnostics','clear-image-diagnostics'])$(id).disabled=busy||!snapshot;
  }
  function refreshModels(){const old=$('probe-model').value;$('probe-model').replaceChildren(...model.models($('models').value).map(name=>new Option(name,name)));if([...$('probe-model').options].some(o=>o.value===old))$('probe-model').value=old;}
  function populate(c){$('route-enabled').checked=c.route_enabled;$('models').value=c.models.join('\n');$('effort').value=c.default_effort==='max'?'xhigh':c.default_effort;$('ultra').checked=c.allow_ultra;$('timeout').value=c.timeout_seconds;$('ttl').value=c.replay_ttl_seconds/3600;$('image-transport').value=c.image_transport;refreshModels();renderAccounts(c.account_ids);}
  function selectedAccounts(){return [...document.querySelectorAll('#accounts input:checked')].map(input=>Number(input.value));}
  function renderAccounts(selected){
    const known=new Map((snapshot?.accounts||[]).map(a=>[a.id,a]));for(const id of selected)if(!known.has(id))known.set(id,{id,name:'已配置但当前不可用',schedulable:false});
    const rows=[];const options=[];const old=$('probe-account').value;
    for(const a of known.values()){
      const label=document.createElement('label');label.className='check';const input=document.createElement('input');input.type='checkbox';input.value=a.id;input.checked=selected.includes(a.id);
      const text=document.createElement('span');text.textContent=`#${a.id} ${a.name}`;const state=document.createElement('small');state.textContent=a.schedulable?'可调度':'暂停或不可用';label.append(input,text,state);rows.push(label);
      const option=new Option(`#${a.id} ${a.name}${a.schedulable?'':'（不可用）'}`,String(a.id));option.disabled=!a.schedulable;options.push(option);
    }
    if(!rows.length){const empty=document.createElement('p');empty.className='hint';empty.textContent='没有可用的 OpenAI OAuth 账号。请先在 sub2api 中导入，再刷新。';rows.push(empty);}
    $('accounts').replaceChildren(...rows);$('probe-account').replaceChildren(...options);
    if(options.some(o=>o.value===old&&!o.disabled))$('probe-account').value=old;else if(options.some(o=>!o.disabled))$('probe-account').value=options.find(o=>!o.disabled).value;
  }
  function form(){return {...saved,route_enabled:$('route-enabled').checked,account_ids:selectedAccounts(),models:model.models($('models').value),default_effort:$('effort').value,allow_ultra:$('ultra').checked,timeout_seconds:Number($('timeout').value),replay_ttl_seconds:Math.round(Number($('ttl').value)*3600),image_transport:$('image-transport').value};}
  async function save(command,keepForm=false){const config=keepForm?{...saved}:form();if(command)config.command=command;const response=await bridge.call('config.save',{config});saved=model.config(response.config||config);return saved;}
  async function test(cmd){const r=await bridge.call('config.test');const payload=model.parseJSON(r.result.status_json||'{}');if(payload.command_id!==cmd.id)throw new Error('另一个插件窗口更改了操作，请刷新后重试。');if(!r.result.success)throw new Error(r.result.message);return payload;}
  function command(action){return {id:bridge.uuid(),instance:snapshot.instance,issued_at:Math.floor(Date.now()/1000),action};}
  function renderDiagnostic(kind){
    const imageOnly=kind==='image', selection=diagnosticSelections[kind];
    const records=model.diagnosticHistory(snapshot,imageOnly);
    const select=$(imageOnly?'image-request-history':'request-history');
    if(selection.id){
      const found=records.find(r=>r.id===selection.id);
      if(found)selection.record=found;
    }
    const options=[new Option('最新完成请求（刷新时自动跟随）',''),...records.map(r=>new Option(model.requestLabel(r),r.id))];
    if(selection.id&&selection.record&&!records.some(r=>r.id===selection.id))options.push(new Option(`${model.requestLabel(selection.record)} · 已移出当前保留窗口`,selection.id));
    select.replaceChildren(...options);select.value=selection.id;select.disabled=records.length===0&&!selection.record;
    const record=selection.id?selection.record:records[0];
    const result=$(imageOnly?'image-request-diagnostic':'request-diagnostic');
    result.textContent=record?model.requestText(record):imageOnly?'本实例尚未记录到已结束的图片路由请求。请在 Codex 中发送图片后刷新；进行中的请求会在结束后显示。':model.requestText(null);
    result.dataset.state=record&&(record.error||record.http_status>=400||record.attachment_http_status>=400)?'failed':'';
    $(imageOnly?'image-diagnostic-runtime':'diagnostic-runtime').textContent=model.diagnosticRuntime(snapshot);
  }
  function renderDiagnostics(reset=false){
    for(const kind of ['request','image']){
      if(reset){diagnosticSelections[kind].id='';diagnosticSelections[kind].record=null;}
      renderDiagnostic(kind);
    }
  }
  function status(){
    // Serialize passive polling and post-command refreshes. An older response
    // must never replace a newer probe or cancel its polling timer.
    const next=statusTail.then(loadStatus);
    statusTail=next.catch(()=>{});
    return next;
  }
  async function loadStatus(){
    if(closed)return snapshot;
    clearTimeout(timer);
    let success=false;
    try{
    const response=await bridge.call('plugin.status');if(!response.result?.healthy||!response.result.status_json)throw new Error('请先在插件管理页启用插件，再打开设置。');
    const previous=snapshot;snapshot=model.parseJSON(response.result.status_json);
    if(snapshot.host_ready&&(!previous?.host_ready||previous.instance!==snapshot.instance))renderAccounts(selectedAccounts());
    $('runtime').textContent=snapshot.host_ready?'宿主已连接':'等待 HostService v2';
    $('probe-result').textContent=model.probeText(snapshot.probe);$('probe-result').dataset.state=snapshot.probe?.state||'';
    $('probe-result').setAttribute('aria-busy',String(snapshot.probe?.state==='running'));
    const preview=['image','image_route'].includes(snapshot.probe?.kind)?model.imagePreview(snapshot.probe.image_preview):'';
    $('image-preview').hidden=!preview;
    if(preview){if($('image-preview-img').getAttribute('src')!==preview)$('image-preview-img').src=preview;}else $('image-preview-img').removeAttribute('src');
    renderDiagnostics(Boolean(previous&&(previous.instance!==snapshot.instance||previous.diagnostic_generation!==snapshot.diagnostic_generation)));
    if(statusFailed){notice('状态查询已恢复。');statusFailed=false;}
    if(observedInstance&&observedInstance!==snapshot.instance)notice('插件已重启；请检查保存的设置。',true);observedInstance=snapshot.instance;buttons();
    success=true;
    return snapshot;
    }catch(error){statusFailed=true;throw error;}finally{
      clearTimeout(timer);
      if(!closed&&(!success||!snapshot?.host_ready||snapshot?.probe?.state==='running'))timer=setTimeout(poll,success?2000:5000);
    }
  }
  async function poll(){try{await status();}catch(error){notice(error.message,true);}}
  async function run(fn){if(busy)return;busy=true;buttons();try{await fn();}catch(error){notice(error.message,true);}finally{busy=false;buttons();}}
  $('save').addEventListener('click',()=>run(async()=>{await save();notice('设置已保存。');await status();}));
  $('refresh').addEventListener('click',()=>run(async()=>{
    const selected=selectedAccounts();
    // Refreshing accounts must not write an older window's route settings
    // over changes already saved by another administrator/window.
    const latest=model.config((await bridge.call('config.load')).config);
    const changed=JSON.stringify(latest)!==JSON.stringify(saved);saved=latest;
    const cmd=command('accounts');await save(cmd,true);await test(cmd);await status();renderAccounts(selected);
    notice(changed?'账号目录已更新。检测到其他窗口更改了配置，已保留服务器设置；当前表单仍保留原值，保存前请确认。':'账号目录已更新。');
  }));
  async function probe(action){
    if(snapshot?.probe?.state==='running')throw new Error('已有探测正在运行，请等待结果后重试。');
    const cmd={...command(action),account_id:Number($('probe-account').value),model:$('probe-model').value,effort:$('probe-effort').value};
    if(action==='probe_image'||action==='probe_image_route'){
      cmd.image_detail=$('image-detail').value;
      cmd.image_format=$('image-format').value;
    }
    if(!cmd.account_id||!cmd.model)throw new Error('请选择可用账号和模型。');
    await save(cmd);await test(cmd);
    const kind={probe_image:'图片',probe_image_route:'图片路由',probe_tools:'工具',probe:'文本'}[action];
    notice(`设置已保存，${kind}探测已启动，结果会自动更新；最长等待 120 秒。`);await status();
  }
  for(const [id,action] of [['probe','probe'],['probe-image','probe_image'],['probe-image-route','probe_image_route'],['probe-tools','probe_tools']])$(id).addEventListener('click',()=>run(()=>probe(action)));
  for(const id of ['refresh-diagnostic','refresh-image-diagnostic'])$(id).addEventListener('click',()=>run(async()=>{await status();notice('路由诊断与历史已刷新，未发送上游请求。');}));
  for(const id of ['clear-diagnostics','clear-image-diagnostics'])$(id).addEventListener('click',()=>run(async()=>{
    // UI Bridge v1 executes commands via saved config. Load the current settings
    // first so clearing history never submits this window's unsaved form.
    const latest=model.config((await bridge.call('config.load')).config);
    const changed=JSON.stringify(latest)!==JSON.stringify(saved);saved=latest;
    const cmd=command('clear_diagnostics');await save(cmd,true);const {data}=await test(cmd);
    await statusTail;
    if(snapshot.instance===cmd.instance&&data?.diagnostic_generation>=(snapshot.diagnostic_generation||0)){
      snapshot={...snapshot,...data,last_request:null,recent_requests:[],image_requests:[]};
      renderDiagnostics(true);
    }
    notice('已清空本实例全部路由与图片诊断历史。请在目标客户端重试，完成后刷新诊断。'+(changed?' 检测到其他窗口修改配置，已沿用服务器设置；当前表单尚未保存。':''));
    await status();
  }));
  for(const [id,kind] of [['request-history','request'],['image-request-history','image']])$(id).addEventListener('change',()=>{
    const selection=diagnosticSelections[kind];selection.id=$(id).value;
    if(!selection.id)selection.record=null;
    else selection.record=model.diagnosticHistory(snapshot,kind==='image').find(r=>r.id===selection.id)||selection.record;
    renderDiagnostic(kind);
  });
  $('models').addEventListener('input',refreshModels);
  addEventListener('pagehide',()=>{closed=true;clearTimeout(timer);});
  async function init(){bridge.ready();const r=await bridge.call('config.load');saved=model.config(r.config);populate(saved);await status();notice(snapshot.last_error||'先探测一个账号，再启用 BPS 路由。',Boolean(snapshot.last_error));}
  run(init);
})();
