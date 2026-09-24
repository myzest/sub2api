(() => {
  'use strict';
  const defaults = {route_enabled:false,account_ids:[],models:['gpt-5.6-sol','gpt-5.6-terra','gpt-5.6-luna','gpt-6-astra'],default_effort:'medium',allow_ultra:false,timeout_seconds:600,replay_ttl_seconds:86400};
  function parseJSON(value) { return typeof value === 'string' ? JSON.parse(value) : value; }
  function config(value) { const c={...defaults,...(parseJSON(value)||{})}; delete c.command; return c; }
  function models(value) { return [...new Set(value.split(/[\s,]+/).filter(Boolean))]; }
  function probeText(p) {
    if(!p) return '尚未探测';
    const state = {running:'探测中',succeeded:'通道可用',failed:'探测失败'}[p.state] || p.state;
    const lines=[`${state} · 账号 #${p.account_id} · ${p.model} / ${p.effort}`,p.message];
    if(p.http_status)lines.push(`HTTP ${p.http_status}`);
    if(p.returned_model)lines.push(`上游返回模型：${p.returned_model}`);
    if(p.response_id)lines.push(`Response ID：${p.response_id}`);
    if(p.latency_ms)lines.push(`耗时：${(p.latency_ms/1000).toFixed(1)} 秒`);
    if(p.usage)lines.push(`Usage：${JSON.stringify(p.usage)}`);
    return lines.filter(Boolean).join('\n');
  }
  const api={defaults,parseJSON,config,models,probeText};
  if(typeof module!=='undefined'&&module.exports)module.exports=api; else window.BasisPointsModel=api;
})();
