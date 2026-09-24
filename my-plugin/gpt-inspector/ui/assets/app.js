(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const bridge = window.InspectorBridge;
  const preview = window.InspectorPreview;
  const stateNames = { queued:'等待', running:'运行中', stopping:'正在停止', completed:'已完成', failed:'失败', interrupted:'中断' };
  let snapshot = null, models = [], record = null, selectedBatch = '', selectedItem = -1, answer = null;
  let operation = Promise.resolve(), operations = 0, closed = false, timer = null, commandError = '', accountSignature = '', resultSignature = '';
  let loadingResult = '', requestGeneration = 0, modelGeneration = 0;
  const cache = new Map();
  const bytes = n => n < 1024 ? `${n} B` : n < 1024**2 ? `${(n/1024).toFixed(1)} KiB` : `${(n/1024**2).toFixed(2)} MiB`;
  const date = text => text ? new Date(text).toLocaleString('zh-CN', {hour12:false}) : '';
  const text = (id, value) => { $(id).textContent = value ?? ''; };
  function option(value, label) { const o = document.createElement('option'); o.value = value; o.textContent = label; return o; }
  function error(message = '') { commandError = message; renderMessages(); }
  function notify(message) { text('notice', message); $('notice').hidden = !message; }
  function renderMessages() {
    const message = commandError || snapshot?.error || '';
    text('error', message); $('error').hidden = !message;
  }
  function chosenPrompts() { return [...document.querySelectorAll('#prompts input:checked')].map(input => input.value); }
  function enableControls() {
    const available = !!snapshot?.ready && !snapshot.busy && operations === 0;
    const rounds = Number($('rounds').value), count = chosenPrompts().length;
    $('start').disabled = !available || !!snapshot?.active || !$('account').value || !$('model').value || !$('effort').value || count === 0 || !Number.isInteger(rounds) || rounds < 1 || rounds > 20;
    $('refresh-accounts').disabled = !available; $('retry-models').disabled = !available;
    $('stop').disabled = !available || !snapshot?.active || snapshot.active.state === 'stopping';
    $('clear-all').disabled = !snapshot || !!snapshot.busy || operations > 0;
    $('clear-account').disabled = !available || !$('result-account').value;
    $('refresh-results').disabled = !available || !$('result-account').value;
    text('request-count', count && Number.isInteger(rounds) && rounds > 0 ? `${count} 道题 × ${rounds} 轮 = ${count*rounds} 次请求` : '请选择题目');
  }
  function command(action, args = {}) {
    operations++; enableControls();
    const run = operation.then(async () => {
      if (!snapshot?.instance) throw new Error('插件未运行，请先在管理页启用插件');
      const c = { id:bridge.uuid(), instance:snapshot.instance, issued_at:Math.floor(Date.now()/1000), action, ...args };
      await bridge.call('config.save', {config:{command:c}});
      const reply = await bridge.call('config.test');
      const payload = JSON.parse(reply.result.status_json || '{}');
      if (payload.command_id !== c.id) throw new Error('其他弹窗修改了插件操作，请刷新后重试，避免同时从多个窗口操作');
      return payload.data;
    });
    operation = run.catch(() => {});
    return run.finally(() => { operations--; enableControls(); });
  }
  function action(fn) { return async event => { try { error(); await fn(event); } catch (e) { if (!closed) error(e.message); } }; }
  function renderPrompts() {
    if ($('prompts').children.length || !snapshot?.prompts) return;
    for (const p of snapshot.prompts) {
      const card = document.createElement('div'); card.className = 'prompt-card';
      const label = document.createElement('label'); const input = document.createElement('input'); input.type = 'checkbox'; input.value = p.id;
      input.addEventListener('change', enableControls);
      const title = document.createElement('span'); title.textContent = p.title;
      const kind = document.createElement('span'); kind.className = 'kind'; kind.textContent = p.kind === 'html' ? '动画' : '文字';
      label.append(input, title, kind);
      const details = document.createElement('details'); const summary = document.createElement('summary'); summary.textContent = '查看提示词';
      const content = document.createElement('pre'); content.textContent = p.text;
      details.append(summary, content); card.append(label, details); $('prompts').append(card);
    }
  }
  function renderAccounts() {
    const accounts = snapshot?.accounts || [];
    const signature = JSON.stringify(accounts);
    if (signature !== accountSignature) {
      accountSignature = signature; const selected = $('account').value;
      $('account').replaceChildren(option('', '请选择账号'));
      for (const a of accounts) { const o = option(String(a.id), `${a.name} · #${a.id}${a.schedulable ? '' : '（暂停调度）'}`); o.disabled = !a.schedulable; $('account').append(o); }
      $('account').value = accounts.some(a => String(a.id) === selected && a.schedulable) ? selected : '';
      if ($('account').value !== selected) resetModels();
    }
    const stored = snapshot?.stored || [];
    const resultKey = JSON.stringify(stored.map(a => [a.account_id,a.name,a.bytes,a.results]));
    if (resultKey !== resultSignature) {
      resultSignature = resultKey; const selected = $('result-account').value;
      $('result-account').replaceChildren(option('', '请选择结果账号'));
      for (const a of stored) $('result-account').append(option(String(a.account_id), `${a.name} · #${a.account_id} · ${bytes(a.bytes)}`));
      $('result-account').value = selected;
      if (!$('result-account').value && selected) { record = null; clearAnswer(); renderRecord(); }
    }
    text('storage-total', bytes(snapshot?.bytes || 0)); text('storage-detail', `${stored.length} 个账号 · ${snapshot?.results || 0} 条结果`); text('result-count', snapshot?.results || 0);
  }
  function resetModels() {
    modelGeneration++;
    models = []; $('model').replaceChildren(option('', '请选择模型')); $('effort').replaceChildren(option('', '请选择推理强度'));
    $('model').disabled = true; $('effort').disabled = true; $('retry-models').hidden = !$('account').value;
    text('model-note', '选择账号后获取其模型和 Codex 推理档位，不预选模型或档位。'); enableControls();
  }
  async function loadModels() {
    const id = Number($('account').value); resetModels(); if (!id) return;
    const generation = modelGeneration;
    text('model-note', '正在获取该账号的模型目录…');
    try {
      const data = await command('models', {account_id:id});
      if (generation !== modelGeneration) return;
      models = data;
      for (const m of models) $('model').append(option(m.id, m.name && m.name !== m.id ? `${m.id} · ${m.name}` : m.id));
      $('model').disabled = false; text('model-note', '模型与推理强度均需手动选择。档位来自该模型的 Codex 目录。');
    } catch (e) {
      if (generation !== modelGeneration) return;
      text('model-note', `获取失败：${e.message}。可点击“重新获取”。`);
      throw e;
    } finally { enableControls(); }
  }
  function renderActivity() {
    const b = snapshot?.active; $('activity').hidden = !b;
    if (!b) return;
    const done = b.items.filter(i => !['queued','running'].includes(i.state)).length;
    const running = b.items.find(i => i.state === 'running');
    text('activity-title', `${b.account_name} · ${stateNames[b.state] || b.state}`);
    $('progress').max = b.items.length; $('progress').value = done;
    text('activity-detail', `已处理 ${done} / ${b.items.length}${running ? ` · 第 ${running.round} 轮 · ${promptTitle(running.prompt_id)}` : ''} · 可关闭弹窗，后台会继续运行`);
  }
  function resetRuntime() {
    resetModels(); requestGeneration++; cache.clear(); record = null; clearAnswer(); renderRecord();
  }
  async function poll() {
    try {
      const response = await bridge.call('plugin.status');
      if (!response.result?.healthy || !response.result.status_json) { snapshot = null; resetRuntime(); renderActivity(); notify('请先在插件管理页启用 GPT Inspector，再打开弹窗。'); enableControls(); return; }
      const previous = snapshot; snapshot = JSON.parse(response.result.status_json);
      if (previous?.instance && previous.instance !== snapshot.instance) { resetRuntime(); notify('插件已重新启动，未完成的测试已中断。请重新选择测试参数。'); }
      if (snapshot.busy) notify(snapshot.busy);
      else if (!snapshot.ready) notify('账号与结果存储服务尚未就绪。');
      else if (previous?.busy || !previous?.ready) notify('');
      renderMessages(); renderPrompts(); renderAccounts(); renderActivity(); enableControls();
      if (record && snapshot.active?.account_id === record.account_id) { record.pending = snapshot.active; renderRecord(false); }
      if (previous?.active && !snapshot.active) {
        notify('任务已结束，已保存的回答可在“结果与存储”查看。');
        if (Number($('result-account').value) === previous.active.account_id && snapshot.ready && !snapshot.busy) await loadRecord();
      }
      if (previous?.busy && !snapshot.busy && !snapshot.active && Number($('result-account').value) && snapshot.ready) await loadRecord();
    } catch (e) { if (!closed) error(e.message); }
    finally { if (!closed) timer = setTimeout(poll, 2500); }
  }
  function tab(name) {
    const isNew = name === 'new';
    $('new-panel').hidden = !isNew; $('results-panel').hidden = isNew;
    $('new-tab').classList.toggle('active', isNew); $('results-tab').classList.toggle('active', !isNew);
    $('new-tab').setAttribute('aria-pressed', String(isNew)); $('results-tab').setAttribute('aria-pressed', String(!isNew));
  }
  function promptTitle(id) { return snapshot?.prompts?.find(p => p.id === id)?.title || id; }
  function currentBatch() { return [record?.pending,record?.latest].find(b => b?.id === selectedBatch); }
  async function loadRecord() {
    const id = Number($('result-account').value), generation = ++requestGeneration;
    if (!id) { record = null; clearAnswer(); renderRecord(); return; }
    const data = await command('read_batch', {account_id:id});
    if (generation !== requestGeneration || Number($('result-account').value) !== id) return;
    record = data; renderRecord();
  }
  function renderRecord(reset = true) {
    const batches = [record?.pending,record?.latest].filter(Boolean);
    $('result-layout').hidden = batches.length === 0; $('empty').hidden = batches.length > 0;
    if (!batches.length) { selectedBatch = ''; selectedItem = -1; return; }
    if (!batches.some(b => b.id === selectedBatch)) { selectedBatch = batches[0].id; selectedItem = -1; reset = true; }
    $('batch').replaceChildren(...batches.map(b => option(b.id, `${b === record.pending ? '当前测试' : '最新结果'} · ${date(b.created_at)}`)));
    $('batch').value = selectedBatch;
    const b = currentBatch();
    text('batch-info', `${b.model} · ${b.effort} · ${date(b.created_at)} · ${stateNames[b.state] || b.state}`);
    const children = b.items.map((item, index) => {
      const button = document.createElement('button'); button.type = 'button'; button.className = 'item'; button.dataset.state = item.state;
      button.setAttribute('role','listitem'); button.classList.toggle('active', index === selectedItem);
      const title = document.createElement('span'); title.textContent = `第 ${item.round} 轮 · ${promptTitle(item.prompt_id)}`;
      const status = document.createElement('span'); status.className = 'state'; status.textContent = `${stateNames[item.state] || item.state}${item.duration_ms ? ` · ${(item.duration_ms/1000).toFixed(1)}s` : ''}`;
      button.append(title,status); button.addEventListener('click', action(() => showItem(index))); return button;
    });
    $('items').replaceChildren(...children);
    if (reset && selectedItem === -1) { const index = b.items.findIndex(i => i.parts > 0); void action(() => showItem(Math.max(0,index)))(); }
    else if (reset && !answer && b.items[selectedItem]?.parts) { void action(() => showItem(selectedItem))(); }
    else if (selectedItem >= 0 && !answer) {
      const item = b.items[selectedItem];
      if (item) text('answer-text', item.parts ? '回答已保存，点击左侧此条结果查看。' : item.error || `${stateNames[item.state] || item.state}…`);
    }
  }
  function clearAnswer() {
    answer = null; loadingResult = ''; preview.clear($('preview'));
    text('answer-text',''); $('answer-text').hidden = false;
    for (const id of ['answer-error','show-preview','show-source','reasoning-section','prompt-section','usage-section']) $(id).hidden = true;
  }
  async function showItem(index) {
    const b = currentBatch(); if (!b?.items[index]) return;
    selectedItem = index; clearAnswer(); renderRecord(false);
    const item = b.items[index], key = `${b.id}:${index}:${item.sha256 || ''}`;
    text('answer-title', `第 ${item.round} 轮 · ${promptTitle(item.prompt_id)}`);
    text('answer-meta', `${stateNames[item.state] || item.state}${item.duration_ms ? ` · ${(item.duration_ms/1000).toFixed(1)} 秒` : ''}`);
    text('answer-error',item.error); $('answer-error').hidden = !item.error;
    if (!item.parts) { text('answer-text', item.error || `${stateNames[item.state] || item.state}…`); return; }
    loadingResult = key; text('answer-text','正在读取保存的回答…');
    let data = cache.get(key);
    if (!data) {
      const result = await command('read_result', {account_id:b.account_id, batch_id:b.id, item:index});
      if (result.encoding !== 'gzip-base64') throw new Error('未知的结果编码');
      const compressed = Uint8Array.from(atob(result.data), c => c.charCodeAt(0));
      if (crypto.subtle) {
        const hash = Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', compressed)), b => b.toString(16).padStart(2,'0')).join('');
        if (hash !== item.sha256) throw new Error('结果完整性校验失败');
      }
      if (!window.DecompressionStream) throw new Error('当前浏览器不支持结果解压，请使用新版 Chrome、Edge、Firefox 或 Safari');
      const stream = new Blob([compressed]).stream().pipeThrough(new DecompressionStream('gzip'));
      data = JSON.parse(await new Response(stream).text());
      cache.set(key,data); if (cache.size > 8) cache.delete(cache.keys().next().value);
    }
    if (loadingResult !== key || currentBatch()?.id !== b.id || selectedItem !== index) return;
    answer = data;
    text('answer-text',answer.text || '没有文本回答');
    text('answer-error',answer.error || item.error); $('answer-error').hidden = !(answer.error || item.error);
    text('answer-reasoning',answer.reasoning); $('reasoning-section').hidden = !answer.reasoning;
    text('answer-prompt',answer.prompt.text); $('prompt-section').hidden = false;
    text('answer-usage',JSON.stringify({model:b.model, reasoning_effort:b.effort, returned_model:answer.returned_model, response_id:answer.response_id, session_id:item.session_id, usage:answer.usage},null,2)); $('usage-section').hidden = false;
    if (answer.prompt.kind === 'html') {
      $('show-preview').hidden = false; $('show-source').hidden = false;
      if (/<(?:html|svg|!doctype)/i.test(answer.text)) showPreview();
    }
  }
  function showPreview() { if (!answer) return; preview.show($('preview'),answer.text); $('answer-text').hidden = true; }
  function confirmClear(all) {
    text('confirm-title', all ? '清空全部测试结果？' : '清理此账号结果？');
    text('confirm-body', all ? '将停止当前测试并清空本插件所有账号的回答、动画和结果索引。账号与正常转发保持可用。' : '将停止此账号的当前测试，并删除其已保存的全部测试结果。');
    return new Promise(resolve => {
      const dialog = $('confirm-dialog');
      const done = value => { dialog.close(); $('confirm-ok').onclick = null; $('confirm-cancel').onclick = null; dialog.oncancel = null; resolve(value); };
      $('confirm-ok').onclick = () => done(true); $('confirm-cancel').onclick = () => done(false);
      dialog.oncancel = event => { event.preventDefault(); done(false); }; dialog.showModal();
    });
  }
  async function clear(all) {
    const id = Number($('result-account').value); if (!all && !id) return;
    if (!await confirmClear(all)) return;
    await command(all ? 'clear_all' : 'clear_account', all ? {} : {account_id:id});
    requestGeneration++; cache.clear(); record = null; selectedBatch = ''; selectedItem = -1; clearAnswer(); renderRecord();
    notify('清理已提交，正在删除保存的测试数据。');
  }
  $('account').addEventListener('change',action(loadModels)); $('retry-models').addEventListener('click',action(loadModels));
  $('model').addEventListener('change',() => {
    $('effort').replaceChildren(option('','请选择推理强度'));
    const m = models.find(m => m.id === $('model').value);
    for (const effort of m?.efforts || []) $('effort').append(option(effort,effort));
    $('effort').disabled = !m?.efforts?.length;
    if (m && !m.efforts.length) text('model-note','该模型目录未提供可选推理档位，无法开始测试。请重新获取或选择其他模型。');
    enableControls();
  });
  $('effort').addEventListener('change',enableControls); $('rounds').addEventListener('input',enableControls);
  $('refresh-accounts').addEventListener('click',action(async () => { const accounts = await command('accounts'); snapshot.accounts = accounts; renderAccounts(); notify('账号列表已更新。'); }));
  // The host sandbox has no allow-forms; native submission is blocked before
  // the submit event. Use the explicit Bridge action even for keyboard users.
  $('test-form').addEventListener('submit', event => event.preventDefault());
  $('start').addEventListener('click',action(async event => {
    event.preventDefault(); if ($('start').disabled) return;
    const id = Number($('account').value);
    await command('start',{account_id:id,model:$('model').value,effort:$('effort').value,prompts:chosenPrompts(),rounds:Number($('rounds').value)});
    // A fresh form always requires a deliberate model and effort selection.
    $('model').value = ''; $('effort').replaceChildren(option('','请选择推理强度')); $('effort').disabled = true;
    notify('测试已在后台开始。可以关闭弹窗，稍后查看结果。'); enableControls();
  }));
  $('stop').addEventListener('click',action(async () => { await command('stop'); notify('已请求停止，已完成的回答会保留。'); }));
  $('new-tab').addEventListener('click',() => { tab('new'); preview.clear($('preview')); $('answer-text').hidden = false; });
  $('results-tab').addEventListener('click',action(async () => {
    tab('results');
    if (!$('result-account').value && snapshot?.stored?.length) {
      const id = snapshot.active?.account_id || Number($('account').value) || snapshot.stored[0].account_id;
      $('result-account').value = String(id);
      if (!$('result-account').value) $('result-account').value = String(snapshot.stored[0].account_id);
      await loadRecord();
    }
    enableControls();
  }));
  $('result-account').addEventListener('change',action(async () => { record = null; selectedBatch = ''; selectedItem = -1; clearAnswer(); renderRecord(); enableControls(); await loadRecord(); }));
  $('refresh-results').addEventListener('click',action(loadRecord));
  $('batch').addEventListener('change',() => { selectedBatch = $('batch').value; selectedItem = -1; clearAnswer(); renderRecord(); });
  $('show-preview').addEventListener('click',showPreview);
  $('show-source').addEventListener('click',() => { preview.clear($('preview')); $('answer-text').hidden = false; });
  $('clear-all').addEventListener('click',action(() => clear(true))); $('clear-account').addEventListener('click',action(() => clear(false)));
  addEventListener('pagehide',() => { closed = true; clearTimeout(timer); preview.clear($('preview')); cache.clear(); });
  notify('正在连接插件…'); bridge.ready(); void poll();
})();
