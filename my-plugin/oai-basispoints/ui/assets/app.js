(() => {
  'use strict';
  const bridge=window.BasisPointsBridge, model=window.BasisPointsModel, $=id=>document.getElementById(id);
  let saved=model.config({}), snapshot=null, busy=false, timer=null, closed=false, observedInstance='';
  function notice(text,error=false){$('notice').textContent=text;$('notice').classList.toggle('error',error);}
  function buttons(){for(const id of ['save','refresh','probe'])$(id).disabled=busy||!snapshot?.host_ready||(id==='probe'&&snapshot?.probe?.state==='running');}
  function refreshModels(){const old=$('probe-model').value;$('probe-model').replaceChildren(...model.models($('models').value).map(name=>new Option(name,name)));if([...$('probe-model').options].some(o=>o.value===old))$('probe-model').value=old;}
  function populate(c){$('route-enabled').checked=c.route_enabled;$('models').value=c.models.join('\n');$('effort').value=c.default_effort==='max'?'xhigh':c.default_effort;$('ultra').checked=c.allow_ultra;$('timeout').value=c.timeout_seconds;$('ttl').value=c.replay_ttl_seconds/3600;refreshModels();renderAccounts(c.account_ids);}
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
  function form(){return {...saved,route_enabled:$('route-enabled').checked,account_ids:selectedAccounts(),models:model.models($('models').value),default_effort:$('effort').value,allow_ultra:$('ultra').checked,timeout_seconds:Number($('timeout').value),replay_ttl_seconds:Math.round(Number($('ttl').value)*3600)};}
  async function save(command,keepForm=false){const config=keepForm?{...saved}:form();if(command)config.command=command;const response=await bridge.call('config.save',{config});saved=model.config(response.config||config);return saved;}
  async function test(cmd){const r=await bridge.call('config.test');const payload=model.parseJSON(r.result.status_json||'{}');if(payload.command_id!==cmd.id)throw new Error('另一个插件窗口更改了操作，请刷新后重试。');if(!r.result.success)throw new Error(r.result.message);return payload;}
  function command(action){return {id:bridge.uuid(),instance:snapshot.instance,issued_at:Math.floor(Date.now()/1000),action};}
  async function status(){
    const response=await bridge.call('plugin.status');if(!response.result?.healthy||!response.result.status_json)throw new Error('请先在插件管理页启用插件，再打开设置。');snapshot=model.parseJSON(response.result.status_json);
    $('runtime').textContent=snapshot.host_ready?'宿主已连接':'等待 HostService v2';
    $('probe-result').textContent=model.probeText(snapshot.probe);$('probe-result').dataset.state=snapshot.probe?.state||'';
    if(observedInstance&&observedInstance!==snapshot.instance)notice('插件已重启；请检查保存的设置。',true);observedInstance=snapshot.instance;buttons();
    clearTimeout(timer);if(!closed&&(!snapshot.host_ready||snapshot.probe?.state==='running'))timer=setTimeout(poll,2000);
    return snapshot;
  }
  async function poll(){try{await status();}catch(error){notice(error.message,true);}}
  async function run(fn){if(busy)return;busy=true;buttons();try{await fn();}catch(error){notice(error.message,true);}finally{busy=false;buttons();}}
  $('save').addEventListener('click',()=>run(async()=>{await save();notice('设置已保存。');await status();}));
  $('refresh').addEventListener('click',()=>run(async()=>{const selected=selectedAccounts();const cmd=command('accounts');await save(cmd,true);await test(cmd);await status();renderAccounts(selected);notice('账号目录已更新。');}));
  $('probe').addEventListener('click',()=>run(async()=>{
    const cmd={...command('probe'),account_id:Number($('probe-account').value),model:$('probe-model').value,effort:$('probe-effort').value};
    if(!cmd.account_id||!cmd.model)throw new Error('请选择可用账号和模型。');
    await save(cmd);await test(cmd);
    notice('探测已启动，结果会自动更新；最长等待 120 秒。');await status();
  }));
  $('models').addEventListener('input',refreshModels);
  addEventListener('pagehide',()=>{closed=true;clearTimeout(timer);});
  async function init(){bridge.ready();const r=await bridge.call('config.load');saved=model.config(r.config);populate(saved);await status();renderAccounts(saved.account_ids);notice(snapshot.last_error||'先探测一个账号，再启用 BPS 路由。',Boolean(snapshot.last_error));}
  run(init);
})();
