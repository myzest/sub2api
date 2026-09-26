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
  function errorSourceText(source) {
    return {plugin_local_config:'插件本地模型配置',plugin_local_request:'插件本地请求校验',plugin_history:'工具历史恢复',attachment:'图片附件链路',upstream_http:'BPS HTTP 拒绝',upstream_transport:'BPS 连接或传输',upstream_response:'BPS 响应未完成',tool_relay:'工具信封解析',response_conversion:'响应转换',plugin_transport:'插件传输',client_transport:'客户端传输',request_canceled:'宿主或客户端已取消请求',request_timeout_or_canceled:'请求超时或取消'}[source]||source;
  }
  function relayText(relays) {
    if(!Array.isArray(relays)||!relays.length)return [];
    const lines=['工具信封诊断（最多 16 项；不含参数正文）：'];
    for(const [i,r] of relays.slice(0,16).entries()){
      lines.push(`  解析记录 ${i+1}：${r.state==='decoded'?(r.transport?'传输已解析，后续仍需工具校验':'JSON 已解码，后续仍需工具校验'):'解析失败'} · 解开重复信封 ${r.unwrapped||0} 层${r.error_kind?` · ${r.error_kind}`:''}`);
      if(r.transport)lines.push(`    传输：${{custom:'CUSTOM 原文（code 不作 JSON 解码）',function_code:'FUNCTION_CODE 原文＋元数据 JSON'}[r.transport]||r.transport}`);
      for(const f of (r.fields||[]).slice(0,6)){
        lines.push(`    层 ${f.layer} · ${f.field} · ${f.type} · ${f.bytes||0} bytes${f.extracted?' · 已提取完整 JSON 对象':f.fenced?' · 已去除 JSON 围栏':''}${f.backslashes_repaired?` · 反斜杠兼容 ${f.backslashes_repaired} 处`:''}${f.error_kind?` · ${f.error_kind}`:''}${f.error_offset?` · ${f.backslashes_repaired?'兼容后':'解码内容'}字节偏移 ${f.error_offset}`:''}`);
      }
    }
    return lines;
  }
  function replayText(r) {
    if(!r)return [];
    const lines=[];
    if(r.scope_fingerprint)lines.push(`回放范围指纹：${r.scope_fingerprint}（模型、会话键及首条用户输入的摘要；账号另行隔离）`);
    lines.push(`回放存储：命中 ${r.native_hits||0} · 新存 ${r.native_saved||0} · 未命中 ${r.missing||0}`);
    lines.push(`完整历史转换：缺失原生记录 ${r.rebuilt||0} · 外部调用 ${r.imported||0}`);
    if(r.rebuilt)lines.push('缺失记录已根据客户端提供的完整调用及结果转换历史；未取回原生状态，也未重新执行旧工具。未命中不能单独证明过期。');
    if(r.failure)lines.push(`回放失败分类：${r.failure}`);
    return lines;
  }
  function probeText(p) {
    if(!p) return '尚未探测';
    const kind={text:'文本',image:'图片',image_route:'图片路由',tools:'工具'}[p.kind]||'文本';
    const success={text:'通道可用',image:'图片探测通过',image_route:'图片路由探测通过',tools:'工具回放通过'}[p.kind]||'通道可用';
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
    if(p.kind==='image'||p.kind==='image_route'){
      lines.push(`图片传输：${p.image_transport||'未知'} · detail：${p.image_detail||'未知'} · 格式：${p.image_format||'未记录'}`);
      if(p.attachment)lines.push(...attachmentText(p.attachment));
      if(p.image_expected)lines.push(`图片正确答案：${p.image_expected}`);
      if(p.image_reply)lines.push(`图片实际回复：${p.image_reply}`);
      else if(p.state==='succeeded'||p.state==='failed')lines.push('图片实际回复：未收到文本');
      if(p.kind==='image_route'){
        if(p.route_diagnostic_id)lines.push(`路由诊断 ID：${p.route_diagnostic_id}`);
        lines.push('此探测由插件生成模拟桌面 additional_tools 请求并经过 Forward 路由；不是重放用户拖入的图片请求。完整记录见“图片路由诊断”。');
      }
    }
    if(p.kind==='tools'){
      if(p.tool_name)lines.push(`模拟工具：${p.tool_name}`);
      if(p.tool_source)lines.push(`工具声明位置：${p.tool_source} · 类型：${p.tool_type||'未知'}`);
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
        lines.push(...relayText(r.relay),...replayText(r.replay));
      }
      lines.push('此探测使用模拟的客户端结果；本地文件读取和实际执行需在 Codex 客户端验证。');
    }
    return lines.filter(Boolean).join('\n');
  }
  function imagePreview(value) {
    if(typeof value!=='string'||value.length>65536)return '';
    if(!value.startsWith('data:image/png;base64,iVBORw0KGgo')&&!value.startsWith('data:image/jpeg;base64,/9j/'))return '';
    return /^data:image\/(?:png|jpeg);base64,(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(value)?value:'';
  }
  function diagnosticHistory(snapshot,imageOnly=false) {
    const key=imageOnly?'image_requests':'recent_requests';
    const fallback=snapshot?.last_request;
    const hasHistory=Array.isArray(snapshot?.[key]);
    const source=hasHistory?snapshot[key]:fallback?[fallback]:[];
    const seen=new Set();
    return source.filter(r=>{
      if(!r||typeof r!=='object'||typeof r.id!=='string'||!r.id||seen.has(r.id))return false;
      if(imageOnly&&!hasHistory&&!r.input_images&&!r.images?.length&&!r.image_operation&&r.origin!=='image_route_probe')return false;
      seen.add(r.id);return true;
    }).slice(0,imageOnly?10:20);
  }
  function requestLabel(r) {
    const time=Number(r.finished_at||r.started_at);
    const date=Number.isFinite(time)&&time>0?new Date(time*1000).toLocaleString('zh-CN',{hour12:false}):'时间未知';
    const origin=r.origin==='image_route_probe'?'路由探测':'客户端';
    const kind=r.image_operation==='generations'?'生图':r.image_operation==='edits'?'改图':'Responses';
    return `${date} · ${origin} · ${kind} · #${r.account_id} · ${r.model||'模型未解析'} · 输入图片 ${r.input_images||0} · HTTP ${r.http_status||'—'}${r.error?` · 失败${r.error_source?`（${errorSourceText(r.error_source)}）`:''}`:''}${r.id?` · ID ${r.id.slice(0,8)}`:''}`;
  }
  function diagnosticRuntime(snapshot) {
    const cleared=snapshot?.diagnostics_cleared_at;
    return `当前插件：${snapshot?.version?`v${snapshot.version}`:'版本未提供'} · 实例：${snapshot?.instance||'尚未连接'}${cleared?` · 上次清空：${new Date(cleared*1000).toLocaleString('zh-CN',{hour12:false})}`:''}`;
  }
  function diagnosticStateText(state) {
    const states={captured:'已采集脱敏校验信息',empty:'错误响应体为空',no_fields:'错误 JSON 不包含可采集的校验字段',non_json:'错误响应不是 JSON',invalid_json:'响应 JSON 无法解析',too_large:'响应超过采集上限',read_error:'响应读取失败',missing_file_id:'附件响应缺少有效文件 ID'};
    return states[state]||state;
  }
  function attachmentText(a) {
    const hasAttempts=Number.isInteger(a.attempts)&&a.attempts>=0;
    const status=a.http_status||(a.attempts>0?'未收到上传响应':a.reused?'缓存复用，无上传请求':hasAttempts?'无上传请求':'未收到上传响应');
    const lines=[`附件：上传 ${a.uploaded||0} 张 · 复用 ${a.reused||0} 张${hasAttempts?` · 上传尝试 ${a.attempts} 次`:''}`,
      `附件 HTTP：${status}${a.attempts>0?'（最近一次上传尝试）':''}`];
    if(a.content_type)lines.push(`附件 Content-Type：${a.content_type}`);
    if(a.request_id)lines.push(`附件 Request ID：${a.request_id}`);
    if(a.diagnostic_state)lines.push(`附件错误采集：${diagnosticStateText(a.diagnostic_state)}`);
    if(a.diagnostic)lines.push(`附件诊断（已脱敏）：${a.diagnostic}`);
    if(Array.isArray(a.images)&&a.images.length){
      lines.push('附件文件字段（最多 16 项；插件生成的文件名）：');
      const states={uploaded:'已上传',reused:'缓存复用',failed:'上传失败'};
      for(const item of a.images.slice(0,16)){
        if(!item||typeof item!=='object')continue;
        const http=item.http_status?` · HTTP=${item.http_status}`:hasAttempts&&item.state==='failed'?' · 未收到上传响应':'';
        lines.push(`  位置=${item.path||'未知'} · 文件名=${item.filename||'未知'} · MIME=${item.mime||'未知'} · 原图=${item.bytes||0} bytes · ${states[item.state]||'状态未知'}${http}`);
      }
    }
    return lines;
  }
  function imageSummaryText(images,label) {
    if(!Array.isArray(images)||!images.length)return [];
    const lines=[`${label}（最多 16 项；仅字段摘要）：`];
    for(const image of images.slice(0,16)){
      if(!image||typeof image!=='object')continue;
      const fields=[];
      for(const [key,name] of [['path','位置'],['type','类型'],['source','来源'],['detail','detail'],['mime','MIME']]){
        const value=image[key];
        if(value===null)fields.push(`${name}=null`);
        else if(typeof value==='string'||typeof value==='number')fields.push(`${name}=${String(value).slice(0,200)}`);
      }
      if(Number.isFinite(image.encoded_bytes))fields.push(`编码长度=${image.encoded_bytes} bytes`);
      if(fields.length)lines.push(`  ${fields.join(' · ')}`);
    }
    return lines;
  }
  function clientToolHistoryText(r) {
    if(!Array.isArray(r.client_tool_history))return [];
    const rows=r.client_tool_history.slice(-16), count=r.client_tool_history_count||0;
    const lines=[`客户端工具回传链：最后一条 user 消息之后共 ${count} 项，显示最近 ${rows.length} 项（来自本次请求携带的历史）`];
    for(const row of rows){
      const result=['function_call_output','custom_tool_call_output'].includes(row.type);
      lines.push(`  input[${row.input_index}] · ${result?'结果':'调用'} · ${row.type} · ${row.name||'名称未关联到唯一调用'}`);
      if(result){
        lines.push(`    output 格式：${row.output_format||'未记录'}`);
        const signals=Array.isArray(row.signals)?row.signals.slice(0,8):[];
        lines.push(`    客户端自报字段：${signals.length?signals.join(' · '):'未提取到受支持的机器状态；不能据此判断成功、失败或策略拦截'}`);
      }
    }
    lines.push('queued/accepted 仅表示请求已提交；completed 或 exit_code=0 也不能证明已渲染预览。这里只摘录白名单字段，不保存结果正文，不推断嵌套执行器内部调用。');
    return lines;
  }
  function requestText(r) {
    if(!r)return '尚无已结束的 BPS 路由请求。在 Codex 中发送请求后，点击“刷新诊断”。';
    const imageAPI=Boolean(r.image_operation), api=imageAPI?'Images':'Responses';
    const lines=[`账号 #${r.account_id} · ${r.model||'模型未解析'}`];
    lines.push(`接口：${imageAPI?`/images/${r.image_operation}`:'/responses'}`);
    if(r.version||r.instance)lines.push(`记录插件：${r.version?`v${r.version}`:'版本未提供'} · 实例：${r.instance||'未提供'}`);
    if(r.origin)lines.push(`请求来源：${{route:'客户端路由',image_route_probe:'图片路由探测'}[r.origin]||r.origin}`);
    if(r.id)lines.push(`诊断 ID：${r.id}`);
    for(const [key,label] of [['started_at','开始时间'],['finished_at','结束时间']]){
      if(Number.isFinite(r[key])&&r[key]>0)lines.push(`${label}：${new Date(r[key]*1000).toLocaleString('zh-CN',{hour12:false})}`);
    }
    if(r.stage)lines.push(`阶段：${stageText(r.stage)}`);
    if(r.reasoning_effort)lines.push(`推理强度：${r.reasoning_effort}`);
    if(r.image_transport)lines.push(`图片传输：${r.image_transport}`);
    if(Number.isFinite(r.request_bytes))lines.push(`客户端请求体：${r.request_bytes} bytes`);
    if(Number.isFinite(r.upstream_request_bytes))lines.push(`${api} 请求体：${r.upstream_request_bytes} bytes`);
    lines.push(`${api} HTTP：${r.http_status||'未收到响应'}`);
    if((imageAPI?r.upstream_started:r.responses_started)===false)lines.push(`${api} 发送阶段：尚未开始`);
    if(r.client_http_status)lines.push(`客户端 HTTP：${r.client_http_status}${r.error&&r.client_http_status<400?(imageAPI?'（响应校验失败，HTTP 200 不代表出图）':'（请求仍在流内失败，HTTP 200 不代表完成）'):''}`);
    if(r.error_source)lines.push(`错误来源：${errorSourceText(r.error_source)}`);
    if(r.content_type)lines.push(`${api} Content-Type：${r.content_type}`);
    if(r.request_id)lines.push(`${api} Request ID：${r.request_id}`);
    if(r.attachment)lines.push(...attachmentText(r.attachment));
    else if(r.attachment_http_status)lines.push(`附件 HTTP：${r.attachment_http_status}`);
    if(r.upstream_error_state){
      lines.push(`上游错误采集：${diagnosticStateText(r.upstream_error_state)}`);
    }
    if(r.upstream_error)lines.push(`上游诊断（已脱敏）：${r.upstream_error}`);
    if(r.terminal)lines.push(`结束状态：${valueText(r.terminal)}`);
    if(r.response_attempts)lines.push(`Responses 发送次数：${r.response_attempts} · 非流式协议中断重试：${r.protocol_retries||0}`);
    const fallbackNames={refresh_cached_attachments:'清除被拒绝的旧附件缓存并重传',upload_rejected_inline_kind:'将被拒绝的内嵌图片改为附件',omit_rejected_images:'改用明确缺图提示',protocol_interruption:'非流式协议中断重试'};
    if(r.image_fallbacks?.length)lines.push(`图片容错路径：${r.image_fallbacks.map(x=>fallbackNames[x]||x).join(' → ')}`);
    for(const attempt of (r.response_retries||[]).slice(0,16)){
      lines.push(`  第 ${attempt.attempt} 次 Responses：HTTP ${attempt.http_status||'未收到响应'} · ${fallbackNames[attempt.reason]||attempt.reason}${attempt.request_id?` · Request ID：${attempt.request_id}`:''}`);
      if(attempt.upstream_error)lines.push(`    该次上游诊断（已脱敏）：${attempt.upstream_error}`);
      else if(attempt.upstream_error_state)lines.push(`    该次错误采集：${diagnosticStateText(attempt.upstream_error_state)}`);
    }
    if(r.omitted_images)lines.push(`注意：${r.omitted_images} 张图片未传给模型，已插入缺图提示；本次文本完成不能算作识图成功。`);
    if(r.keepalives)lines.push(`流式保活：${r.keepalives} 次`);
    if(r.completion_recovered)lines.push('终态恢复：依据完整 output_item.done 收尾；未伪造 usage，不代表已收到上游 response.completed。');
    if(r.skipped_tools)lines.push(`未释放的工具调用：${r.skipped_tools}（无法转换或客户端禁止并行时的额外调用；未执行）`);
    lines.push(`输入图片总数：${r.input_images||0}`);
    lines.push(...imageSummaryText(r.images,'客户端图片字段'));
    lines.push(...imageSummaryText(r.upstream_images,'上游图片字段'));
    if(imageAPI){
      lines.push(`输出图片条目：${r.generated_images||0}（按宿主可接收的 b64_json 计数；不记录图片正文）`);
      if(r.stage==='completed'&&!r.generated_images)lines.push('Images 返回 JSON 但没有图片条目，不能判定生图成功。');
      if(r.error)lines.push(`错误：${r.error}`);
      lines.push('这是 Codex 生图执行器使用的独立图片接口；文件保存与对话展示由客户端完成。');
      return lines.join('\n');
    }
    const types=Object.entries(r.tool_types||{}).map(([name,count])=>`${name} × ${count}`);
    lines.push(`客户端工具类型：${types.length?types.join('，'):'无'}`);
    const unbridged=Object.entries(r.tool_types||{}).filter(([name,count])=>count>0&&!['function','custom','namespace'].includes(name));
    if(unbridged.length)lines.push(`未桥接的工具类型：${unbridged.map(([name,count])=>`${name} × ${count}`).join('，')}（不会作为可调用工具发送给 BPS）`);
    if(r.tool_types?.image_generation)lines.push('原生生图声明：已收到 hosted image_generation，此类型尚未映射；本插件通过客户端 image_gen 工具及独立 Images 接口生图。');
    else lines.push('原生生图声明：本次摘要未记录到 image_generation；不能据此排除 function/custom 或客户端动态提供的生图工具。');
    const sources=Object.entries(r.tool_sources||{}).map(([name,count])=>`${name} × ${count}`);
    if(sources.length)lines.push(`工具声明来源（未展开 namespace）：${sources.join('，')}`);
    if(r.additional_tool_items)lines.push(`additional_tools 输入项：${r.additional_tool_items}`);
    const historyTypes=Object.entries(r.history_types||{}).map(([name,count])=>`${name} × ${count}`);
    if(historyTypes.length)lines.push(`历史工具项：${historyTypes.join('，')}`);
    const handleLabels={plugin:'插件句柄',external:'外部历史 ID',missing:'缺少 ID',invalid_plugin:'插件句柄格式异常'};
    const handles=Object.entries(r.history_handles||{}).map(([name,count])=>`${handleLabels[name]||name} × ${count}`);
    if(handles.length)lines.push(`历史 ID 分类（调用及结果合计）：${handles.join('，')}`);
    lines.push(...replayText(r.replay));
    lines.push(...clientToolHistoryText(r));
    lines.push(`可调用工具：${r.callable_tools||0}`);
    if(r.tool_names?.length)lines.push(`工具名称（最多 32 个）：${r.tool_names.slice(0,32).join('，')}`);
    if(r.callable_tools>(r.tool_names?.length||0))lines.push('工具名称列表已截断；未显示的名称不能据此判定为不存在。');
    lines.push(`tool_choice：${r.tool_choice===undefined||r.tool_choice===''?'未显式指定':valueText(r.tool_choice)}`);
    lines.push(`parallel_tool_calls：${r.parallel_tool_calls===undefined?'未显式指定':valueText(r.parallel_tool_calls)}`);
    lines.push(`输出工具调用：${r.output_tool_calls||0}`);
    if(r.output_tools?.length){
      lines.push('输出工具明细（转换后，最多 16 项；不等于客户端已执行）：');
      for(const tool of r.output_tools.slice(0,16))lines.push(`  ${tool.type} · ${tool.name||'名称未记录'}`);
      if(r.output_tool_calls>r.output_tools.length)lines.push('输出工具明细已截断。');
    }
    const nativeTypes=Object.entries(r.native_tool_types||{}).map(([name,count])=>`${name} × ${count}`);
    if(nativeTypes.length)lines.push(`BPS 原生工具类型（转换前）：${nativeTypes.join('，')}`);
    if(r.native_tool_names?.length)lines.push(`BPS 原生工具（转换前，最多 32 个）：${r.native_tool_names.slice(0,32).join('，')}`);
    lines.push(...relayText(r.relay));
    if(r.error)lines.push(`错误：${r.error}`);
    if(r.error&&['request','prepare'].includes(r.stage))lines.push('请求或工具目录转换尚未完成，请先查看错误；当前数量不能证明客户端没有提供工具。');
    else if(!r.callable_tools)lines.push(r.tool_choice==='none'?'tool_choice 为 none，本次请求禁止模型调用工具。':'顶层 tools 与 input.additional_tools 中未解析到可调用的 function/custom 工具，请结合声明来源与 tool_choice 判断。');
    else if(r.output_tool_calls)lines.push('已生成客户端工具调用；实际执行与权限由 Codex 客户端决定。');
    else lines.push('本次请求有可调用工具，但未输出工具调用；请结合阶段与错误判断。');
    return lines.join('\n');
  }
  const api={defaults,parseJSON,config,models,probeText,imagePreview,requestText,diagnosticHistory,requestLabel,diagnosticRuntime};
  if(typeof module!=='undefined'&&module.exports)module.exports=api; else window.BasisPointsModel=api;
})();
