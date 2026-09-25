(() => {
  'use strict';
  const defaults = {route_enabled:false,account_ids:[],models:['gpt-5.6-sol','gpt-5.6-terra','gpt-5.6-luna','gpt-6-astra'],default_effort:'medium',allow_ultra:false,timeout_seconds:600,replay_ttl_seconds:86400,image_transport:'attachment'};
  function parseJSON(value) { return typeof value === 'string' ? JSON.parse(value) : value; }
  function config(value) { const c={...defaults,...(parseJSON(value)||{})}; delete c.command; return c; }
  function models(value) { return [...new Set(value.split(/[\s,]+/).filter(Boolean))]; }
  function stageText(stage) {
    const labels={request:'接收客户端请求',identity:'获取账号身份',prepare:'准备请求',upload:'上传图片',connect:'连接 BPS',http:'接收 HTTP 响应',response:'解析响应',image_check:'核对图片答案',tool_check:'校验工具调用',tool_result_check:'核对工具回放结果',completed:'完成'};
    return labels[stage]?`${labels[stage]}（${stage}）`:stage;
  }
  function valueText(value) { return typeof value==='object'?JSON.stringify(value):String(value); }
  function probeText(p) {
    if(!p) return '尚未探测';
    const kind={text:'文本',image:'图片',tools:'工具'}[p.kind]||'文本';
    const success={text:'通道可用',image:'图片探测通过',tools:'工具回放通过'}[p.kind]||'通道可用';
    const state = {running:'探测中',succeeded:success,failed:'探测失败'}[p.state] || p.state;
    const lines=[`${kind} · ${state} · 账号 #${p.account_id} · ${p.model} / ${p.effort}`,p.message];
    if(p.stage)lines.push(`阶段：${stageText(p.stage)}${p.round?` · 第 ${p.round} 轮`:''}`);
    lines.push(`Responses HTTP：${p.http_status||'尚未收到响应'}`);
    if(p.content_type)lines.push(`Responses Content-Type：${p.content_type}`);
    if(p.request_id)lines.push(`Responses Request ID：${p.request_id}`);
    if(p.request_bytes)lines.push(`Responses 请求体：${p.request_bytes} bytes`);
    if(p.returned_model)lines.push(`上游返回模型：${p.returned_model}`);
    if(p.response_id)lines.push(`Response ID：${p.response_id}`);
    if(p.latency_ms)lines.push(`耗时：${(p.latency_ms/1000).toFixed(1)} 秒`);
    if(p.usage)lines.push(`Usage：${JSON.stringify(p.usage)}`);
    if(p.upstream_error)lines.push(`上游诊断（已脱敏）：${p.upstream_error}`);
    if(p.kind==='image'){
      lines.push(`图片传输：${p.image_transport||'未知'} · detail：${p.image_detail||'未知'}`);
      if(p.attachment){
        const a=p.attachment;
        lines.push(`附件：上传 ${a.uploaded||0} 张 · 复用 ${a.reused||0} 张`);
        lines.push(`附件 HTTP：${a.http_status||(a.reused?'缓存复用，无上传请求':'尚未收到响应')}`);
        if(a.content_type)lines.push(`附件 Content-Type：${a.content_type}`);
        if(a.request_id)lines.push(`附件 Request ID：${a.request_id}`);
        if(a.diagnostic)lines.push(`附件诊断（已脱敏）：${a.diagnostic}`);
      }
      if(p.image_expected)lines.push(`图片正确答案：${p.image_expected}`);
      if(p.image_reply)lines.push(`图片实际回复：${p.image_reply}`);
      else if(p.state==='succeeded'||p.state==='failed')lines.push('图片实际回复：未收到文本');
    }
    if(p.kind==='tools'){
      if(p.tool_name)lines.push(`模拟工具：${p.tool_name}`);
      lines.push(`已转换工具调用：${p.tool_calls||0}`);
      if(p.tool_expected)lines.push(`模拟结果中的校验值：${p.tool_expected}`);
      if(p.tool_reply)lines.push(`回放后实际回复：${p.tool_reply}`);
      for(const r of p.rounds||[]){
        const details=[`第 ${r.round} 轮：HTTP ${r.http_status||'未收到响应'}`];
        if(r.latency_ms)details.push(`${(r.latency_ms/1000).toFixed(1)} 秒`);
        lines.push(details.join(' · '));
        if(r.request_id)lines.push(`  Request ID：${r.request_id}`);
        if(r.response_id)lines.push(`  Response ID：${r.response_id}`);
        if(r.returned_model)lines.push(`  上游返回模型：${r.returned_model}`);
        if(r.usage)lines.push(`  Usage：${JSON.stringify(r.usage)}`);
      }
      lines.push('此探测使用模拟的客户端结果；本地文件读取和实际执行需在 Codex 客户端验证。');
    }
    return lines.filter(Boolean).join('\n');
  }
  function imagePreview(value) {
    if(typeof value!=='string'||value.length>65536||!value.startsWith('data:image/png;base64,iVBORw0KGgo'))return '';
    return /^data:image\/png;base64,(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(value)?value:'';
  }
  function requestText(r) {
    if(!r)return '尚无已结束的 BPS 路由请求。在 Codex 中发送请求后，点击“刷新诊断”。';
    const lines=[`账号 #${r.account_id} · ${r.model||'模型未解析'}`];
    if(r.id)lines.push(`诊断 ID：${r.id}`);
    for(const [key,label] of [['started_at','开始时间'],['finished_at','结束时间']]){
      if(Number.isFinite(r[key])&&r[key]>0)lines.push(`${label}：${new Date(r[key]*1000).toLocaleString('zh-CN',{hour12:false})}`);
    }
    if(r.stage)lines.push(`阶段：${stageText(r.stage)}`);
    lines.push(`Responses HTTP：${r.http_status||'未收到响应'}`);
    if(r.attachment_http_status)lines.push(`附件 HTTP：${r.attachment_http_status}`);
    if(r.terminal)lines.push(`结束状态：${valueText(r.terminal)}`);
    const types=Object.entries(r.tool_types||{}).map(([name,count])=>`${name} × ${count}`);
    lines.push(`客户端工具类型：${types.length?types.join('，'):'无'}`);
    lines.push(`可调用工具：${r.callable_tools||0}`);
    if(r.tool_names?.length)lines.push(`工具名称（最多 32 个）：${r.tool_names.slice(0,32).join('，')}`);
    lines.push(`tool_choice：${r.tool_choice===undefined||r.tool_choice===''?'未显式指定':valueText(r.tool_choice)}`);
    lines.push(`parallel_tool_calls：${r.parallel_tool_calls===undefined?'未显式指定':valueText(r.parallel_tool_calls)}`);
    lines.push(`输入图片：${r.input_images||0} · 输出工具调用：${r.output_tool_calls||0}`);
    if(r.error)lines.push(`错误：${r.error}`);
    if(r.error&&['request','prepare'].includes(r.stage))lines.push('请求或工具目录转换尚未完成，请先查看错误；当前数量不能证明客户端没有提供工具。');
    else if(!r.callable_tools)lines.push(r.tool_choice==='none'?'tool_choice 为 none，本次请求禁止模型调用工具。':'本次请求没有可调用的 function/custom 工具，请结合客户端工具类型与 tool_choice 判断。');
    else if(r.output_tool_calls)lines.push('已生成客户端工具调用；实际执行与权限由 Codex 客户端决定。');
    else lines.push('本次请求有可调用工具，但未输出工具调用；请结合阶段与错误判断。');
    return lines.join('\n');
  }
  const api={defaults,parseJSON,config,models,probeText,imagePreview,requestText};
  if(typeof module!=='undefined'&&module.exports)module.exports=api; else window.BasisPointsModel=api;
})();
