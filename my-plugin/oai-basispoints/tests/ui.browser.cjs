const test=require('node:test');
const assert=require('node:assert/strict');
const http=require('node:http');
const fs=require('node:fs');
const path=require('node:path');
const {chromium}=require('playwright');
const root=path.resolve(__dirname,'../ui');

test('sandboxed UI saves before probing, keeps dirty form on refresh, renders results as text',async()=>{
  const server=http.createServer((req,res)=>{
    const relative=new URL(req.url,'http://localhost').pathname;
    const file=path.resolve(root,'.'+relative);
    if(!file.startsWith(root+path.sep)){res.writeHead(404).end();return;}
    if(relative==='/host.html'){
      res.setHeader('Content-Type','text/html');res.end(`<!doctype html><title>Sub2API fixture host</title><iframe sandbox="allow-scripts" src="/index.html#bridge_token=test-session" style="border:0;width:100%;height:1600px"></iframe>`);return;
    }
    try{res.setHeader('Content-Type',file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':'text/html');res.end(fs.readFileSync(file));}catch{res.writeHead(404).end();}
  });
  await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
  let browser;
  try{
    browser=await chromium.launch({headless:true});const page=await browser.newPage({viewport:{width:1050,height:1250}});
    const errors=[];page.on('pageerror',e=>errors.push(e.message));
    await page.addInitScript(()=>{
      if(window!==top)return;
      const config={route_enabled:false,account_ids:[],models:['gpt-5.6-sol'],default_effort:'medium',allow_ultra:false,timeout_seconds:600,replay_ttl_seconds:86400};
      const state={instance:'fixture-instance-123456',host_ready:true,accounts:[{id:7,name:'<img src=x onerror="alert(1)"> account',schedulable:true}],probe:null};
      window.actions=[];window.stored=config;
      addEventListener('message',e=>{
        const m=e.data;if(!m||m.source!=='sub2api-plugin-ui'||m.bridge_token!=='test-session')return;
        window.actions.push(m.type);let response={source:'sub2api-plugin-host',request_id:m.request_id,bridge_token:m.bridge_token,ok:true};
        if(m.type==='config.load')response.config=window.stored;
        else if(m.type==='config.save'){window.stored=m.config;response.config=m.config;}
        else if(m.type==='plugin.status')response.result={healthy:true,status_json:JSON.stringify(state)};
        else if(m.type==='config.test'){
          if(window.stored.command.action==='probe')state.probe={id:window.stored.command.id,account_id:7,model:window.stored.command.model,effort:'low',state:'succeeded',returned_model:'actual-fixture-model',message:'通道成功；不代表质量验收',http_status:200,latency_ms:50};
          response.result={success:true,status_json:JSON.stringify({command_id:window.stored.command.id,data:{}})};
        }else return;
        e.source.postMessage(response,'*');
      });
    });
    await page.goto(`http://127.0.0.1:${server.address().port}/host.html`);
    const ui=page.frameLocator('iframe');await ui.locator('#save').waitFor();
    await ui.locator('#probe').waitFor({state:'visible'});await page.waitForFunction(()=>window.actions.includes('plugin.status'));
    await ui.locator('#models').fill('gpt-5.6-sol\ngpt-6-astra');
    await ui.locator('#refresh').click();await ui.locator('#notice').filter({hasText:'账号目录已更新'}).waitFor();
    assert.deepEqual(await page.evaluate(()=>window.stored.models),['gpt-5.6-sol']);
    assert.equal(await ui.locator('#models').inputValue(),'gpt-5.6-sol\ngpt-6-astra');
    assert.equal(await ui.locator('#accounts img').count(),0);
    await ui.locator('#probe-model').selectOption('gpt-6-astra');await ui.locator('#probe').click();
    await ui.locator('#probe-result').filter({hasText:'actual-fixture-model'}).waitFor();
    const actions=await page.evaluate(()=>window.actions);const i=actions.lastIndexOf('config.test');assert.equal(actions[i-1],'config.save');
    assert.equal(await page.evaluate(()=>window.stored.command.model),'gpt-6-astra');assert.equal(await page.evaluate(()=>window.stored.route_enabled),false);
    assert.deepEqual(errors,[]);
    fs.mkdirSync(path.resolve(__dirname,'../build'),{recursive:true});
    await page.screenshot({path:path.resolve(__dirname,'../build/ui-desktop.png'),fullPage:true});
    await page.setViewportSize({width:390,height:844});await page.screenshot({path:path.resolve(__dirname,'../build/ui-mobile.png'),fullPage:true});
    assert.equal(await ui.locator('body').evaluate(el=>el.scrollWidth<=el.clientWidth+1),true,'mobile UI overflows');
  }finally{await browser?.close();await new Promise(resolve=>server.close(resolve));}
});
