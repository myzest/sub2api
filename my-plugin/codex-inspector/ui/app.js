(() => {
  'use strict';
  var DEFAULTS = {
    "enabled": true,
    "timezone": {
      "mode": "egress_ip",
      "custom_tz": "Asia/Singapore",
      "default_tz": "Asia/Singapore",
      "egress_cache_hours": 24,
      "overrides": {}
    },
    "detect": {
      "task_id": "",
      "created_at": "",
      "account_ids": [],
      "models": ["gpt-6-astra", "gpt-5.6-sol"],
      "repeats": 3,
      "concurrency": 4
    }
  };

  const $ = id => document.getElementById(id);
  const clone = value => JSON.parse(JSON.stringify(value));
  const token = new URLSearchParams(location.hash.slice(1)).get('bridge_token');
  const pending = new Map();
  let closed = false, timer = null, polling = false, epoch = 0;
  let draft = clone(DEFAULTS), baseline = '', loaded = false, busy = false;
  let status = null, online = false, expectedTaskID = '', confirmation = null;
  let catalogKey = '', taskKey = '', eventKey = '', pollAgain = false;

  function randomHex(length) {
    return Array.from(crypto.getRandomValues(new Uint8Array(length)), b => b.toString(16).padStart(2, '0')).join('');
  }
  function fire(type, payload, id) {
    if (closed || !token || parent === window) return;
    parent.postMessage(Object.assign({}, payload, {source: 'sub2api-plugin-ui', bridge_token: token, type: type, request_id: id || randomHex(12)}), '*');
  }
  function call(type, payload) {
    return new Promise((resolve, reject) => {
      if (closed || !token || parent === window) { reject(new Error('请从 Sub2API 插件管理页面打开本插件')); return; }
      const id = randomHex(16);
      const timeout = setTimeout(() => { pending.delete(id); reject(new Error('宿主响应超时；请先刷新任务状态，勿立即重复启动检测')); }, type === 'plugin.status' ? 15000 : 120000);
      pending.set(id, {resolve: resolve, reject: reject, timeout: timeout});
      fire(type, payload || {}, id);
    });
  }
  addEventListener('message', event => {
    const m = event.data;
    if (event.source !== parent || !m || m.source !== 'sub2api-plugin-host' || m.bridge_token !== token) return;
    const entry = pending.get(m.request_id);
    if (!entry) return;
    pending.delete(m.request_id); clearTimeout(entry.timeout);
    if (m.ok === true) entry.resolve(m);
    else entry.reject(new Error(m.error || (m.result && m.result.message) || '宿主拒绝本次操作'));
  });
  addEventListener('pagehide', () => {
    closed = true; epoch++; clearTimeout(timer);
    for (const entry of pending.values()) { clearTimeout(entry.timeout); entry.reject(new Error('插件窗口已关闭')); }
    pending.clear();
  });

  function node(tag, text, className) {
    const element = document.createElement(tag);
    if (text !== undefined) element.textContent = String(text);
    if (className) element.className = className;
    return element;
  }
  function notify(text, kind) { $('notice').textContent = text; $('notice').className = 'notice' + (kind ? ' ' + kind : ''); }
  function parseStatus(result) {
    if (!result || result.healthy !== true) throw new Error((result && result.message) || '插件未运行；请先在插件管理页启用');
    const parsed = typeof result.status_json === 'string' ? JSON.parse(result.status_json) : result.status_json;
    if (!parsed || typeof parsed !== 'object' || !/^[a-f0-9]{16}$/.test(parsed.instance || '')) throw new Error('插件运行状态格式无效，请刷新或重新打开窗口');
    return parsed;
  }
  function normalize(raw) {
    const out = clone(DEFAULTS);
    if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return out;
    if (typeof raw.enabled === 'boolean') out.enabled = raw.enabled;
    for (const group of ['timezone', 'detect']) {
      if (!raw[group] || typeof raw[group] !== 'object') continue;
      for (const key of Object.keys(out[group])) if (raw[group][key] !== undefined && raw[group][key] !== null) out[group][key] = clone(raw[group][key]);
    }
    return out;
  }
  function accountList() {
    return (Array.isArray(status && status.accounts) ? status.accounts : []).filter(a => a && Number.isSafeInteger(a.id) && a.id > 0).map(a => ({id: a.id, name: String(a.name || ('账号 ' + a.id)), schedulable: a.schedulable === true}));
  }
  function stringList(key) { return (Array.isArray(status && status[key]) ? status[key] : []).filter(v => typeof v === 'string' && v.length > 0); }
  function setSelect(select, values, selected, emptyLabel) {
    select.replaceChildren();
    if (emptyLabel) { const option = node('option', emptyLabel); option.value = ''; select.append(option); }
    const all = values.slice(); if (selected && !all.includes(selected)) all.push(selected);
    for (const value of all) { const option = node('option', value + (values.includes(value) ? '' : '（待宿主确认）')); option.value = value; select.append(option); }
    select.value = selected || '';
  }
  function drawCatalog(force) {
    const accounts = accountList(), models = stringList('models'), timezones = stringList('timezones');
    const key = JSON.stringify([accounts, models, timezones]);
    if (!force && key === catalogKey) return;
    catalogKey = key;
    setSelect($('custom-tz'), timezones, draft.timezone.custom_tz);
    setSelect($('default-tz'), timezones, draft.timezone.default_tz);
    const accountIDs = new Set(accounts.map(a => a.id));
    const viewAccounts = accounts.concat(draft.detect.account_ids.filter(id => !accountIDs.has(id)).map(id => ({id: id, name: '未找到的账号 ' + id, schedulable: false})));
    $('accounts').replaceChildren();
    for (const account of viewAccounts) {
      const label = node('label', undefined, account.schedulable ? '' : 'disabled');
      const input = node('input'); input.type = 'checkbox'; input.value = String(account.id); input.checked = draft.detect.account_ids.includes(account.id); input.dataset.unavailable = account.schedulable ? '' : 'true';
      input.addEventListener('change', () => { draft.detect.account_ids = Array.from($('accounts').querySelectorAll('input:checked'), x => Number(x.value)).sort((a, b) => a - b); changed(); });
      label.append(input, node('span', account.name + ' (#' + account.id + ')' + (account.schedulable ? '' : ' · 不可调度'))); $('accounts').append(label);
    }
    if (!viewAccounts.length) $('accounts').append(node('p', '没有可用的 OpenAI OAuth 账号', 'subtle'));
    $('models').replaceChildren();
    for (const model of Array.from(new Set(models.concat(draft.detect.models)))) {
      const label = node('label'); const input = node('input'); input.type = 'checkbox'; input.value = model; input.checked = draft.detect.models.includes(model); input.dataset.unavailable = models.includes(model) ? '' : 'true';
      input.addEventListener('change', () => { draft.detect.models = Array.from($('models').querySelectorAll('input:checked'), x => x.value); changed(); });
      label.append(input, node('span', model + (models.includes(model) ? '' : ' · 不可用'))); $('models').append(label);
    }
    if (!models.length) $('models').append(node('p', '等待宿主提供可选模型，不自动猜测其他模型', 'subtle'));
    const overrideAccounts = accounts.concat(Object.keys(draft.timezone.overrides).filter(id => !accountIDs.has(Number(id))).map(id => ({id: Number(id), name: '未找到的账号 ' + id})));
    $('overrides').replaceChildren(); $('overrides').className = overrideAccounts.length ? 'list' : 'list empty';
    for (const account of overrideAccounts) {
      const row = node('div', undefined, 'override-row'); const label = node('label', account.name + ' (#' + account.id + ')');
      const select = node('select'); select.id = 'override-' + account.id; label.htmlFor = select.id;
      setSelect(select, timezones, draft.timezone.overrides[String(account.id)], '跟随策略');
      select.addEventListener('change', () => { if (select.value) draft.timezone.overrides[String(account.id)] = select.value; else delete draft.timezone.overrides[String(account.id)]; changed(); });
      row.append(label, select); $('overrides').append(row);
    }
    if (!overrideAccounts.length) $('overrides').append(node('p', '账号列表为空；运行状态恢复后可设置覆盖。'));
    updateControls();
  }
  function fillForm() {
    $('enabled').checked = draft.enabled;
    for (const input of document.querySelectorAll('input[name="mode"]')) input.checked = input.value === draft.timezone.mode;
    $('cache-hours').value = draft.timezone.egress_cache_hours;
    $('repeats').value = draft.detect.repeats; $('concurrency').value = draft.detect.concurrency;
    drawCatalog(true); updateControls();
  }
  function dirty() { return loaded && JSON.stringify(draft) !== baseline; }
  function changed() { updateControls(); }
  function invalidReason() {
    const c = draft, accounts = accountList(), models = stringList('models');
    if (!c.detect.account_ids.length || !c.detect.models.length) return '请选择账号与模型';
    if (c.detect.account_ids.some(id => !accounts.some(a => a.id === id && a.schedulable))) return '选择中包含已移除或不可调度账号';
    if (c.detect.models.some(m => !models.includes(m))) return '选择中包含宿主不支持的模型';
    if (c.detect.account_ids.length * c.detect.models.length > 200) return '账号与模型组合数不能超过 200';
    if (!Number.isInteger(c.detect.repeats) || c.detect.repeats < 1 || c.detect.repeats > 10) return '重复次数必须为 1–10';
    if (!Number.isInteger(c.detect.concurrency) || c.detect.concurrency < 1 || c.detect.concurrency > 16) return '并发账号数必须为 1–16';
    return '';
  }
  function activeTask() {
    const task = status && status.detect, dispatch = status && status.dispatch;
    return (!!task && ['running', 'queued', 'pending', 'starting'].includes(task.state)) || (!!dispatch && ['running', 'queued'].includes(dispatch.state));
  }
  function updateControls() {
    $('save').disabled = !loaded || busy || !dirty();
    $('save').textContent = busy ? '处理中…' : '保存配置';
    $('dirty').textContent = !loaded ? '配置未加载' : busy ? '正在提交配置' : dirty() ? '有未保存修改' : '配置已保存';
    const reason = invalidReason();
    $('start').disabled = !loaded || busy || !online || !status || status.host_services !== 'ready' || activeTask() || !!expectedTaskID || !!reason;
    $('start').textContent = busy ? '正在提交…' : activeTask() || expectedTaskID ? '检测进行中' : '开始检测';
    $('start').title = reason;
    $('refresh').disabled = polling || closed;
    for (const input of document.querySelectorAll('main input, main select')) input.disabled = !loaded || busy || (input.dataset.unavailable === 'true' && !input.checked);
    const pairs = draft.detect.account_ids.length * draft.detect.models.length;
    $('request-count').textContent = pairs ? draft.detect.account_ids.length + ' 个账号 × ' + draft.detect.models.length + ' 个模型 × ' + draft.detect.repeats + ' 轮 = ' + pairs * draft.detect.repeats + ' 次请求' : '请选择账号与模型';
    const ready = online && status && status.host_services === 'ready';
    $('connection').textContent = !online ? '未连接' : !ready ? '宿主能力降级' : status.enabled === false ? '改写已停用' : '运行正常';
    $('connection').className = 'badge ' + (ready && status.enabled !== false ? 'good' : 'bad');
  }
  function timeText(value) { const d = new Date(value); return Number.isFinite(d.getTime()) ? d.toLocaleString('zh-CN', {hour12: false}) : '—'; }
  function cell(row, value, className) { row.append(node('td', value, className)); }
  function renderTask() {
    const task = status && status.detect;
    const dispatch = status && status.dispatch;
    if (dispatch && dispatch.task_id === (expectedTaskID || draft.detect.task_id) && ['not_started', 'interrupted'].includes(dispatch.state) && (!task || (task.task_id || task.id) !== dispatch.task_id)) {
      expectedTaskID = '';
      notify('本次任务未能启动或已中断。请检查诊断与其他实例状态，再决定是否重新提交。', 'error');
      $('task-state').textContent = dispatch.state === 'not_started' ? '未能启动' : '已中断';
      $('task-info').textContent = dispatch.task_id + ' · 未获得执行条件；下表可能是上一次任务结果。';
      return;
    }
    // An older read-only snapshot must not replace the task just accepted.
    if (expectedTaskID && (!task || (task.task_id || task.id) !== expectedTaskID)) {
      $('results').replaceChildren(); $('no-results').hidden = false; $('progress').value = 0;
      taskKey = '';
      $('task-state').textContent = '等待任务状态'; $('task-info').textContent = '已提交任务 ' + expectedTaskID + '，等待宿主发布状态；勿重复启动。'; return;
    }
    if (expectedTaskID && task && !['running', 'queued', 'pending', 'starting'].includes(task.state)) expectedTaskID = '';
    const key = JSON.stringify(task); if (key === taskKey) return; taskKey = key;
    $('results').replaceChildren();
    if (!task) { $('task-state').textContent = '暂无任务'; $('task-info').textContent = '检测结果仅保存在宿主插件 KV。'; $('progress').value = 0; $('no-results').hidden = false; return; }
    const states = {running: '进行中', queued: '排队中', pending: '等待执行', starting: '启动中', completed: '已完成', interrupted: '已中断', failed: '失败', canceled: '已取消'};
    $('task-state').textContent = states[task.state] || task.state || (task.done ? '已完成' : '未知状态');
    const results = Array.isArray(task.results) ? task.results : [];
    const total = Number(task.total) || (Array.isArray(task.account_ids) && Array.isArray(task.models) ? task.account_ids.length * task.models.length : 0);
    const completed = Number.isFinite(task.completed) ? task.completed : results.length;
    $('task-info').textContent = (task.task_id || task.id || '未知任务') + ' · ' + completed + ' / ' + total + ' 组合 · 创建于 ' + timeText(task.created_at) + (task.state === 'interrupted' ? ' · 不会自动重试，重新检测会再次消耗额度。' : '');
    $('progress').max = Math.max(total, 1); $('progress').value = Math.max(0, Math.min(completed, total));
    const accounts = accountList();
    for (const result of results) {
      const row = node('tr'); const account = accounts.find(a => a.id === result.account_id);
      const valid = Number(result.valid_runs) > 0;
      cell(row, (account ? account.name : '账号 ' + result.account_id) + '\n' + (result.model || '—'));
      cell(row, valid ? (result.verdict || '无判定') : '无有效结果');
      cell(row, valid ? (result.match ? '匹配' : '不匹配') : '—', valid ? result.match ? 'match' : 'mismatch' : '');
      cell(row, valid && typeof result.probability === 'number' ? (result.probability * 100).toFixed(1) + '%' : '—');
      cell(row, (Number(result.top_hits) || 0) + ' / ' + (Number(result.valid_runs) || 0));
      cell(row, valid ? String(Number(result.avg_latency_ms) || 0) + ' ms' : '—');
      cell(row, Array.isArray(result.reasons) && result.reasons.length ? result.reasons.join('\n') : result.failures ? String(result.failures) + ' 轮失败' : '—');
      $('results').append(row);
    }
    $('no-results').hidden = results.length > 0;
  }
  function renderEvents() {
    const events = Array.isArray(status && status.events) ? status.events : [];
    const key = JSON.stringify(events); if (key === eventKey) return; eventKey = key;
    $('events').replaceChildren();
    for (const event of events) {
      const row = node('tr'); cell(row, timeText(event.at)); cell(row, event.action || '—'); cell(row, (event.outcome || '—') + ' / ' + (event.count || 1));
      cell(row, [event.account_id ? '账号 #' + event.account_id : '', event.model || '', event.error || ''].filter(Boolean).join(' · ')); $('events').append(row);
    }
    $('no-events').hidden = events.length > 0;
  }
  function applyStatus(next) {
    const restarted = status && status.instance !== next.instance;
    status = next; online = true;
    if (restarted) { expectedTaskID = ''; closeConfirmation(); notify('插件实例已变化。旧任务不会自动重放；请核对最近任务，确认后再发起新检测。', 'error'); }
    if (!expectedTaskID && next.dispatch && ['queued', 'running'].includes(next.dispatch.state)) expectedTaskID = next.dispatch.task_id;
    if (next.host_services !== 'ready') notify('宿主服务当前降级，检测已禁用。未保存的表单保持不变。', 'error');
    if (next.enabled === false && !dirty() && !busy) notify('请求时区改写已停用，业务请求保持转发；手动检测仍可独立使用。');
    drawCatalog(false); renderTask(); renderEvents();
    $('runtime-info').textContent = '版本 ' + (next.version || '未知') + ' · 实例 ' + next.instance + ' · 宿主服务 ' + next.host_services + ' · 时区策略 ' + ((next.timezone && next.timezone.mode) || '未知');
    updateControls();
  }
  async function refresh() {
    if (closed) return;
    if (polling) { pollAgain = true; return; }
    polling = true; const startedEpoch = epoch; updateControls(); clearTimeout(timer);
    try {
      const response = await call('plugin.status');
      if (!closed && startedEpoch === epoch) applyStatus(parseStatus(response.result));
    } catch (error) {
      if (!closed && startedEpoch === epoch) { online = false; closeConfirmation(); notify(error.message, 'error'); }
    } finally {
      polling = false; updateControls();
      if (!closed) { timer = setTimeout(refresh, pollAgain ? 0 : 3000); pollAgain = false; }
    }
  }
  function validateConfig(config) {
    if (!Number.isInteger(config.timezone.egress_cache_hours) || config.timezone.egress_cache_hours < 1 || config.timezone.egress_cache_hours > 720) throw new Error('出口缓存时长必须为 1–720 小时');
    for (const [key, min, max] of [['repeats', 1, 10], ['concurrency', 1, 16]]) if (!Number.isInteger(config.detect[key]) || config.detect[key] < min || config.detect[key] > max) throw new Error(key + ' 超出允许范围');
  }
  async function saveConfiguration(config) {
    validateConfig(config);
    const response = await call('config.save', {config: config});
    if (!response.config || typeof response.config !== 'object') throw new Error('宿主未返回已保存配置，请重新打开窗口核对');
    const saved = normalize(response.config);
    if (config.detect.task_id && saved.detect.task_id !== config.detect.task_id) throw new Error('检测任务 ID 与保存响应不一致；可能有其他配置窗口冲突，请刷新核对');
    draft = saved; baseline = JSON.stringify(saved); fillForm();
    return saved;
  }
  async function saveOnly() {
    if (busy || !loaded) return;
    busy = true; epoch++; updateControls();
    try { await saveConfiguration(clone(draft)); notify('配置已保存。普通保存不会发起任何检测请求。', 'success'); }
    catch (error) { notify(error.message, 'error'); }
    finally { busy = false; updateControls(); refresh(); }
  }
  function closeConfirmation() {
    confirmation = null; $('confirm-overlay').hidden = true; $('app').inert = false;
  }
  function confirmDetection() {
    if ($('start').disabled || !status) return;
    confirmation = {config: clone(draft), instance: status.instance};
    $('confirm-details').textContent = $('request-count').textContent + '。这些请求会发送至真实上游并消耗所选账号额度。此结果仅供统计参考。';
    $('confirm-overlay').hidden = false; $('app').inert = true; $('confirm-cancel').focus();
  }
  async function startDetection() {
    if (busy || !confirmation) return;
    const selected = confirmation;
    closeConfirmation();
    if (!online || !status || status.instance !== selected.instance || status.host_services !== 'ready' || activeTask()) { notify('运行状态已变化，请刷新后重新确认检测。', 'error'); return; }
    busy = true; epoch++; updateControls();
    const config = selected.config;
    const taskID = 'd' + selected.instance + '-' + Date.now() + '-' + randomHex(6);
    config.detect.task_id = taskID; config.detect.created_at = new Date().toISOString();
    let submitted = false;
    try {
      await saveConfiguration(config);
      // The host tests saved config, not the UI payload. Check for a cross-window save before the side effect.
      const current = await call('config.load');
      if (!current.config || normalize(current.config).detect.task_id !== taskID) throw new Error('配置已被其他窗口修改，未启动检测。请刷新并核对配置。');
      submitted = true; expectedTaskID = taskID;
      const response = await call('config.test');
      const result = response.result;
      if (!result || result.success !== true) { expectedTaskID = ''; throw new Error((result && result.message) || '宿主拒绝检测任务'); }
      const accepted = typeof result.status_json === 'string' ? JSON.parse(result.status_json) : result.status_json;
      if (!accepted || accepted.task_id !== taskID || accepted.accepted !== true) { expectedTaskID = ''; throw new Error('检测确认与当前任务不一致，可能存在其他窗口保存冲突；请刷新查看实际任务，勿立即重试。'); }
      notify('检测任务已接受。关闭窗口不影响执行；请等待结果。', 'success');
      renderTask();
    } catch (error) {
      if (submitted) online = false;
      notify(error.message + (submitted ? ' 请先核对最近任务状态。' : ''), 'error');
    } finally { busy = false; updateControls(); refresh(); }
  }

  $('enabled').addEventListener('change', () => { draft.enabled = $('enabled').checked; changed(); });
  for (const input of document.querySelectorAll('input[name="mode"]')) input.addEventListener('change', () => { draft.timezone.mode = input.value; changed(); });
  for (const [id, group, key] of [['custom-tz','timezone','custom_tz'],['default-tz','timezone','default_tz'],['cache-hours','timezone','egress_cache_hours'],['repeats','detect','repeats'],['concurrency','detect','concurrency']]) {
    $(id).addEventListener('input', () => { draft[group][key] = $(id).type === 'number' ? Number($(id).value) : $(id).value; changed(); });
  }
  for (const name of ['timezone', 'detect']) $(name + '-tab').addEventListener('click', () => {
    for (const key of ['timezone', 'detect']) { $(key + '-tab').setAttribute('aria-selected', String(name === key)); $(key + '-panel').hidden = name !== key; }
  });
  $('save').addEventListener('click', saveOnly); $('refresh').addEventListener('click', refresh); $('start').addEventListener('click', confirmDetection);
  $('confirm-cancel').addEventListener('click', () => { closeConfirmation(); $('start').focus(); }); $('confirm-start').addEventListener('click', startDetection);
  addEventListener('keydown', event => {
    if ($('confirm-overlay').hidden) return;
    if (event.key === 'Escape') { closeConfirmation(); $('start').focus(); }
    if (event.key === 'Tab') { event.preventDefault(); (document.activeElement === $('confirm-cancel') ? $('confirm-start') : $('confirm-cancel')).focus(); }
  });
  let lastHeight = 0;
  if (typeof ResizeObserver === 'function') new ResizeObserver(() => {
    const height = Math.ceil($('app').getBoundingClientRect().height);
    if (height !== lastHeight) { lastHeight = height; fire('ui.resize', {height: height}); }
  }).observe($('app'));
  fire('sub2api.plugin.ready');
  fillForm();
  (async () => {
    try {
      const response = await call('config.load');
      draft = normalize(response.config); baseline = JSON.stringify(draft); loaded = true; fillForm();
      notify('配置已加载。检测需要单独确认并发起，不会自动运行。');
    } catch (error) { notify(error.message, 'error'); }
    await refresh();
  })();
})();
