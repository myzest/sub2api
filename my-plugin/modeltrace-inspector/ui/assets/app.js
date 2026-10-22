(() => {
  'use strict';

  const $ = id => document.getElementById(id);
  const bridge = window.InspectorBridge;
  const stateNames = { queued: '等待', running: '运行中', stopping: '正在停止', completed: '已完成', failed: '失败', interrupted: '中断' };
  const cache = new Map();
  let snapshot = null, models = [], record = null;
  let selectedBatch = '', selectedRound = 1, selectedItem = -1;
  let operation = Promise.resolve(), operations = 0, closed = false, timer = null;
  let commandError = '', accountSignature = '', resultSignature = '';
  let loadingResult = '', displayedResult = '', requestGeneration = 0, modelGeneration = 0, answerGeneration = 0;

  const text = (id, value) => { $(id).textContent = value ?? ''; };
  const bytes = n => n < 1024 ? `${n} B` : n < 1024 ** 2 ? `${(n / 1024).toFixed(1)} KiB` : `${(n / 1024 ** 2).toFixed(2)} MiB`;
  const date = value => value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '';
  const timeoutLabel = minutes => minutes === 0 ? '单条不限时' : `单条超时 ${minutes} 分钟`;
  const activeForAccount = id => snapshot?.active?.find(batch => batch.account_id === id);
  const currentBatch = () => [record?.pending, record?.latest].find(batch => batch?.id === selectedBatch);
  const itemLabel = item => `第 ${item.round} 轮 · 挑战 ${item.probe}`;
  const resultKey = (batch, index) => `${batch.id}:${index}:${batch.items[index]?.sha256 || ''}`;

  function option(value, label) {
    const element = document.createElement('option');
    element.value = value;
    element.textContent = label;
    return element;
  }

  function notify(message) {
    text('notice', message);
    $('notice').hidden = !message;
  }

  function error(message = '') {
    commandError = message;
    renderMessages();
  }

  function renderMessages() {
    const message = commandError || snapshot?.error || '';
    text('error', message);
    $('error').hidden = !message;
  }

  function enableControls() {
    const available = !!snapshot?.ready && !snapshot.busy && operations === 0;
    const rounds = Number($('rounds').value), timeout = Number($('timeout').value);
    const accountBusy = !!activeForAccount(Number($('account').value));
    $('start').disabled = !available || accountBusy || !$('account').value || !$('model').value || !$('effort').value
      || !Number.isInteger(rounds) || rounds < 1 || rounds > 20 || !Number.isInteger(timeout) || timeout < 0 || timeout > 120;
    text('start', accountBusy ? '该账号测试中' : '开始测试');
    $('refresh-accounts').disabled = !available;
    $('retry-models').disabled = !available;
    $('clear-all').disabled = !snapshot || !!snapshot.busy || operations > 0;
    $('clear-account').disabled = !available || !$('result-account').value;
    $('refresh-results').disabled = !available || !$('result-account').value;
    for (const button of document.querySelectorAll('[data-stop-batch]')) {
      const batch = snapshot?.active?.find(batch => batch.id === button.dataset.stopBatch);
      button.disabled = !available || !batch || batch.state === 'stopping';
    }
    text('request-count', Number.isInteger(rounds) && rounds > 0 && rounds <= 20
      ? `${rounds} 轮 × 3 条挑战 = ${rounds * 3} 次请求` : '请输入 1–20 轮');
  }

  function command(action, args = {}) {
    operations++;
    enableControls();
    const run = operation.then(async () => {
      if (!snapshot?.instance) throw new Error('插件未运行，请先在管理页启用插件');
      const instance = snapshot.instance;
      const command = { id: bridge.uuid(), instance, issued_at: Math.floor(Date.now() / 1000), action, ...args };
      await bridge.call('config.save', { config: { command } });
      const reply = await bridge.call('config.test');
      if (!reply.result?.success) throw new Error(reply.result?.message || '插件操作失败');
      const payload = JSON.parse(reply.result.status_json || '{}');
      if (payload.command_id !== command.id) throw new Error('其他弹窗修改了插件操作，请刷新后重试，避免同时从多个窗口操作');
      if (snapshot?.instance !== instance) throw new Error('插件运行状态已变化，请刷新后重试');
      return payload.data;
    });
    operation = run.catch(() => {});
    return run.finally(() => { operations--; enableControls(); });
  }

  function action(fn) {
    return async event => {
      try { error(); await fn(event); }
      catch (exception) { if (!closed) error(exception.message); }
    };
  }

  function renderAccounts() {
    const accounts = snapshot?.accounts || [];
    const signature = JSON.stringify(accounts);
    if (signature !== accountSignature) {
      accountSignature = signature;
      const selected = $('account').value;
      $('account').replaceChildren(option('', '请选择账号'));
      for (const account of accounts) {
        const entry = option(String(account.id), `${account.name} · #${account.id}${account.schedulable ? '' : '（暂停调度）'}`);
        entry.disabled = !account.schedulable;
        $('account').append(entry);
      }
      $('account').value = accounts.some(account => String(account.id) === selected && account.schedulable) ? selected : '';
      if ($('account').value !== selected) resetModels();
    }
    const stored = snapshot?.stored || [];
    const signatureResults = JSON.stringify(stored.map(account => [account.account_id, account.name, account.bytes, account.results]));
    if (signatureResults !== resultSignature) {
      resultSignature = signatureResults;
      const selected = $('result-account').value;
      $('result-account').replaceChildren(option('', '请选择结果账号'));
      for (const account of stored) $('result-account').append(option(String(account.account_id), `${account.name} · #${account.account_id} · ${bytes(account.bytes)}`));
      $('result-account').value = selected;
      if (!$('result-account').value && selected) resetRecord();
    }
    text('storage-total', bytes(snapshot?.bytes || 0));
    text('storage-detail', `${stored.length} 个账号 · ${snapshot?.results || 0} 条回答`);
    text('result-count', snapshot?.results || 0);
    const bank = snapshot?.bank;
    text('bank-summary', bank ? `${bank.models?.length || 0} 个候选模型 · 构建时间 ${date(bank.built_at)} · revision ${bank.revision?.slice(0, 12) || '未提供'}` : '指纹库信息暂不可用');
  }

  function resetModels() {
    modelGeneration++;
    models = [];
    $('model').replaceChildren(option('', '请选择模型'));
    $('effort').replaceChildren(option('', '请选择推理强度'));
    $('model').disabled = true;
    $('effort').disabled = true;
    $('retry-models').hidden = !$('account').value;
    text('model-note', '选择账号后获取其模型和 Codex 推理档位，不预选模型或档位。');
    enableControls();
  }

  async function loadModels() {
    const accountID = Number($('account').value);
    resetModels();
    if (!accountID) return;
    const generation = modelGeneration;
    text('model-note', '正在获取该账号的模型目录…');
    try {
      const data = await command('models', { account_id: accountID });
      if (generation !== modelGeneration) return;
      models = data;
      for (const model of models) $('model').append(option(model.id, model.name && model.name !== model.id ? `${model.id} · ${model.name}` : model.id));
      $('model').disabled = false;
      text('model-note', '模型与推理强度均需手动选择。档位来自该模型的 Codex 目录。');
    } catch (exception) {
      if (generation !== modelGeneration) return;
      text('model-note', `获取失败：${exception.message}。可点击“重新获取”。`);
      throw exception;
    } finally { enableControls(); }
  }

  function renderActivity() {
    const batches = snapshot?.active || [];
    $('activity').hidden = batches.length === 0;
    text('activity-title', `${batches.length} 个账号正在测试`);
    $('active-tasks').replaceChildren(...batches.map(batch => {
      const card = document.createElement('div');
      card.className = 'task-card';
      card.dataset.accountId = batch.account_id;
      const heading = document.createElement('div');
      heading.className = 'row';
      const title = document.createElement('strong');
      title.textContent = `${batch.account_name} · #${batch.account_id} · ${stateNames[batch.state] || batch.state}`;
      const stop = document.createElement('button');
      stop.type = 'button';
      stop.className = 'danger';
      stop.dataset.stopBatch = batch.id;
      stop.textContent = '停止此账号';
      stop.addEventListener('click', action(async () => {
        await command('stop', { account_id: batch.account_id, batch_id: batch.id });
        notify(`已请求停止 ${batch.account_name}，已保存的回答会保留。`);
      }));
      heading.append(title, stop);
      const progress = document.createElement('progress');
      progress.max = batch.items.length;
      progress.value = batch.items.filter(item => !['queued', 'running'].includes(item.state)).length;
      const running = batch.items.filter(item => item.state === 'running');
      const details = running.map(item => {
        const seconds = Math.max(0, Math.floor((Date.now() - new Date(item.started_at).getTime()) / 1000));
        return `${itemLabel(item)}（已用 ${Math.floor(seconds / 60)}分${seconds % 60}秒${item.attempt > 1 ? `，第 ${item.attempt} 次尝试` : ''}）`;
      }).join('；');
      const description = document.createElement('p');
      description.className = 'muted task-detail';
      description.textContent = `${batch.model} · ${batch.effort} · ${timeoutLabel(batch.timeout_minutes)} · 已处理 ${progress.value}/${batch.items.length} · 同时运行 ${running.length} 条${details ? ` · ${details}` : ''}`;
      card.append(heading, progress, description);
      return card;
    }));
  }

  function resetRecord() {
    requestGeneration++;
    record = null;
    selectedBatch = '';
    selectedRound = 1;
    selectedItem = -1;
    clearAnswer();
    renderRecord();
  }

  function resetRuntime() {
    resetModels();
    cache.clear();
    resetRecord();
  }

  async function poll() {
    try {
      const response = await bridge.call('plugin.status');
      if (!response.result?.healthy || !response.result.status_json) {
        snapshot = null;
        resetRuntime();
        renderActivity();
        notify('请先在插件管理页启用 ModelTrace Inspector，再打开弹窗。');
        enableControls();
        return;
      }
      const previous = snapshot;
      snapshot = JSON.parse(response.result.status_json);
      if (previous?.instance && previous.instance !== snapshot.instance) {
        resetRuntime();
        notify('插件已重新启动，未完成的测试已中断。请重新选择测试参数。');
      }
      if (snapshot.busy) notify(snapshot.busy);
      else if (!snapshot.ready) notify('账号与结果存储服务尚未就绪。');
      else if (previous?.busy || !previous?.ready) notify('');
      renderMessages();
      renderAccounts();
      renderActivity();
      enableControls();
      const current = record && activeForAccount(record.account_id);
      if (current) { record.pending = current; renderRecord(); }
      const finished = (previous?.active || []).filter(batch => !snapshot.active.some(next => next.id === batch.id));
      if (finished.length) notify(`${finished.map(batch => batch.account_name).join('、')} 的任务已结束，可在“结果与存储”查看。`);
      const accountID = Number($('result-account').value);
      const stored = snapshot.stored.find(account => account.account_id === accountID);
      const loaded = record?.pending || record?.latest;
      // Short batches can finish between polls; compare persisted IDs as well.
      const changed = stored && (record?.account_id !== accountID || stored.batch_id !== loaded?.id || stored.state !== loaded?.state);
      if (accountID && snapshot.ready && !snapshot.busy && (changed || previous?.busy)) await loadRecord();
    } catch (exception) { if (!closed) error(exception.message); }
    finally { if (!closed) timer = setTimeout(poll, 2500); }
  }

  function tab(name) {
    const isNew = name === 'new';
    $('new-panel').hidden = !isNew;
    $('results-panel').hidden = isNew;
    for (const [id, active] of [['new-tab', isNew], ['results-tab', !isNew]]) {
      $(id).classList.toggle('active', active);
      $(id).setAttribute('aria-pressed', String(active));
    }
  }

  async function loadRecord() {
    const accountID = Number($('result-account').value), generation = ++requestGeneration;
    if (!accountID) { resetRecord(); return; }
    const data = await command('read_batch', { account_id: accountID });
    if (generation !== requestGeneration || Number($('result-account').value) !== accountID) return;
    const old = record?.pending || record?.latest;
    const next = data.pending || data.latest;
    if (old?.id !== next?.id) { selectedBatch = ''; selectedItem = -1; clearAnswer(); }
    record = data;
    renderRecord();
  }

  function renderRecord() {
    const batches = [record?.pending, record?.latest].filter(Boolean);
    $('result-layout').hidden = batches.length === 0;
    $('empty').hidden = batches.length > 0;
    if (!batches.length) return;
    if (!batches.some(batch => batch.id === selectedBatch)) {
      selectedBatch = batches[0].id;
      selectedRound = 1;
      selectedItem = -1;
      clearAnswer();
    }
    $('batch').replaceChildren(...batches.map(batch => option(batch.id, `${batch === record.pending ? '当前测试' : '最新结果'} · ${date(batch.created_at)}`)));
    $('batch').value = selectedBatch;
    const batch = currentBatch();
    text('batch-info', `${batch.model} · ${batch.effort} · ${timeoutLabel(batch.timeout_minutes)} · ${date(batch.created_at)} · ${stateNames[batch.state] || batch.state}`);
    text('batch-error', batch.error);
    $('batch-error').hidden = !batch.error;
    $('round').replaceChildren(...Array.from({ length: batch.rounds }, (_, index) => option(String(index + 1), `第 ${index + 1} 轮`)));
    if (selectedRound > batch.rounds) selectedRound = 1;
    $('round').value = String(selectedRound);
    window.ModelTraceView.render($('analysis-panel'), batch, selectedRound, snapshot?.bank);
    const items = batch.items.map((item, index) => ({ ...item, index })).filter(item => item.round === selectedRound);
    if (!items.some(item => item.index === selectedItem)) {
      selectedItem = (items.find(item => item.parts > 0) || items[0])?.index ?? -1;
      clearAnswer();
    }
    $('items').replaceChildren(...items.map(item => {
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'item';
      button.dataset.state = item.state;
      button.dataset.item = item.index;
      button.classList.toggle('active', item.index === selectedItem);
      const title = document.createElement('span');
      title.textContent = itemLabel(item);
      const status = document.createElement('span');
      status.className = 'state';
      status.textContent = `${stateNames[item.state] || item.state}${item.attempt > 1 ? ` · 尝试 ${item.attempt} 次` : ''}${item.duration_ms ? ` · ${(item.duration_ms / 1000).toFixed(1)}s` : ''}`;
      button.append(title, status);
      button.addEventListener('click', action(async () => {
        selectedItem = item.index;
        clearAnswer();
        renderRecord();
      }));
      return button;
    }));
    if (selectedItem >= 0) {
      const key = resultKey(batch, selectedItem);
      if (displayedResult !== key && loadingResult !== key) void action(() => showItem(selectedItem))();
    }
  }

  function clearAnswer() {
    answerGeneration++;
    loadingResult = '';
    displayedResult = '';
    text('answer-text', '');
    for (const id of ['answer-error', 'answer-retry', 'answer-quality', 'reasoning-section', 'prompt-section', 'usage-section']) $(id).hidden = true;
  }

  async function showItem(index) {
    const batch = currentBatch(), item = batch?.items[index];
    if (!item) return;
    clearAnswer();
    const generation = answerGeneration;
    const key = resultKey(batch, index);
    text('answer-title', `${itemLabel(item)} · 原始回答`);
    text('answer-meta', `${stateNames[item.state] || item.state}${item.duration_ms ? ` · ${(item.duration_ms / 1000).toFixed(1)} 秒` : ''}`);
    text('answer-error', item.error);
    $('answer-error').hidden = !item.error;
    if (!item.parts) {
      text('answer-text', item.error || `${stateNames[item.state] || item.state}…`);
      return;
    }
    loadingResult = key;
    text('answer-text', '正在读取保存的回答…');
    try {
      let answer = cache.get(key);
      if (!answer) {
        const result = await command('read_result', { account_id: batch.account_id, batch_id: batch.id, item: index });
        if (result.encoding !== 'gzip-base64') throw new Error('未知的结果编码');
        const compressed = Uint8Array.from(atob(result.data), character => character.charCodeAt(0));
        if (crypto.subtle) {
          const hash = Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', compressed)), byte => byte.toString(16).padStart(2, '0')).join('');
          if (hash !== item.sha256) throw new Error('结果完整性校验失败');
        }
        if (!window.DecompressionStream) throw new Error('当前浏览器不支持结果解压，请使用新版浏览器');
        const stream = new Blob([compressed]).stream().pipeThrough(new DecompressionStream('gzip'));
        answer = JSON.parse(await new Response(stream).text());
        cache.set(key, answer);
        if (cache.size > 8) cache.delete(cache.keys().next().value);
      }
      if (generation !== answerGeneration) return;
      displayedResult = key;
      text('answer-text', answer.text || '没有文本回答');
      text('answer-error', answer.error || item.error);
      $('answer-error').hidden = !(answer.error || item.error);
      const attempts = answer.attempts || [];
      text('answer-retry', `本条共尝试 ${attempts.length} 次（自动重试 ${Math.max(0, attempts.length - 1)} 次）。${answer.error ? '最后一次未完成。' : '已收到完整回答。'}每次耗时与错误可在“请求信息”查看。`);
      $('answer-retry').hidden = attempts.length < 2;
      if (answer.quality) {
        const quality = answer.quality;
        const warnings = (quality.warnings || []).map(window.ModelTraceView.warningText);
        text('answer-quality', `解析到 ${quality.parsed_numbers} 个数字，最低要求 ${quality.minimum_numbers} 个；${quality.accepted ? '满足数字数量要求' : '未计入归因'}。${warnings.length ? ` ${warnings.join('；')}` : ''}`);
        $('answer-quality').hidden = false;
      }
      text('answer-reasoning', answer.reasoning);
      $('reasoning-section').hidden = !answer.reasoning;
      text('answer-prompt', answer.prompt.text);
      $('prompt-section').hidden = false;
      text('answer-usage', JSON.stringify({ model: batch.model, reasoning_effort: batch.effort, timeout_minutes: batch.timeout_minutes, returned_model: answer.returned_model, response_id: answer.response_id, session_id: answer.session_id || item.session_id, error_code: answer.error_code, usage: answer.usage, attempts: answer.attempts }, null, 2));
      $('usage-section').hidden = false;
    } catch (exception) {
      if (generation === answerGeneration) {
        text('answer-text', '回答读取失败，请刷新结果重试。');
        throw exception;
      }
    } finally {
      if (generation === answerGeneration) loadingResult = '';
    }
  }

  function confirmClear(all) {
    text('confirm-title', all ? '清空全部测试结果？' : '清理此账号结果？');
    text('confirm-body', all ? '将停止所有账号的测试，并清空本插件保存的回答和归因结果。' : '将停止此账号的当前测试，并删除其保存的回答和归因结果。其他账号继续运行。');
    return new Promise(resolve => {
      const dialog = $('confirm-dialog');
      const done = value => {
        dialog.close();
        $('confirm-ok').onclick = null;
        $('confirm-cancel').onclick = null;
        dialog.oncancel = null;
        resolve(value);
      };
      $('confirm-ok').onclick = () => done(true);
      $('confirm-cancel').onclick = () => done(false);
      dialog.oncancel = event => { event.preventDefault(); done(false); };
      dialog.showModal();
    });
  }

  async function clear(all) {
    const accountID = Number($('result-account').value);
    if (!all && !accountID) return;
    if (!await confirmClear(all)) return;
    await command(all ? 'clear_all' : 'clear_account', all ? {} : { account_id: accountID });
    cache.clear();
    resetRecord();
    notify('清理已提交，正在删除保存的测试数据。');
  }

  $('account').addEventListener('change', action(loadModels));
  $('retry-models').addEventListener('click', action(loadModels));
  $('model').addEventListener('change', () => {
    $('effort').replaceChildren(option('', '请选择推理强度'));
    const model = models.find(model => model.id === $('model').value);
    for (const effort of model?.efforts || []) $('effort').append(option(effort, effort));
    $('effort').disabled = !model?.efforts?.length;
    if (model) {
      const covered = snapshot?.bank?.models?.some(candidate => candidate.id === model.id);
      text('model-note', !model.efforts.length ? '该模型目录未提供可选推理档位，无法开始测试。请重新获取或选择其他模型。'
        : covered ? '当前指纹库包含此模型；归因概率仅供参考。' : '当前指纹库未覆盖此模型。仍可查看原文和最近似候选，但不能据此判断模型被替换。');
    }
    enableControls();
  });
  $('effort').addEventListener('change', enableControls);
  $('rounds').addEventListener('input', enableControls);
  $('timeout').addEventListener('change', enableControls);
  $('refresh-accounts').addEventListener('click', action(async () => {
    const accounts = await command('accounts');
    snapshot.accounts = accounts;
    renderAccounts();
    notify('账号列表已更新。');
  }));
  // The host iframe disallows native form submission. All actions use its bridge.
  $('test-form').addEventListener('submit', event => event.preventDefault());
  $('start').addEventListener('click', action(async event => {
    event.preventDefault();
    if ($('start').disabled) return;
    await command('start', {
      account_id: Number($('account').value), model: $('model').value, effort: $('effort').value,
      rounds: Number($('rounds').value), timeout_minutes: Number($('timeout').value),
    });
    $('model').value = '';
    $('effort').replaceChildren(option('', '请选择推理强度'));
    $('effort').disabled = true;
    notify('测试已在后台开始。可继续添加其他账号，也可以关闭弹窗，稍后查看结果。');
    enableControls();
  }));
  $('new-tab').addEventListener('click', () => tab('new'));
  $('results-tab').addEventListener('click', action(async () => {
    tab('results');
    if (!$('result-account').value && snapshot?.stored?.length) {
      const accountID = Number($('account').value) || snapshot.active?.[0]?.account_id || snapshot.stored[0].account_id;
      $('result-account').value = String(accountID);
      if (!$('result-account').value) $('result-account').value = String(snapshot.stored[0].account_id);
    }
    if ($('result-account').value && snapshot?.ready && !snapshot.busy) await loadRecord();
    enableControls();
  }));
  $('result-account').addEventListener('change', action(async () => { resetRecord(); enableControls(); await loadRecord(); }));
  $('refresh-results').addEventListener('click', action(loadRecord));
  $('batch').addEventListener('change', () => { selectedBatch = $('batch').value; selectedItem = -1; clearAnswer(); renderRecord(); });
  $('round').addEventListener('change', () => { selectedRound = Number($('round').value); selectedItem = -1; clearAnswer(); renderRecord(); });
  $('clear-all').addEventListener('click', action(() => clear(true)));
  $('clear-account').addEventListener('click', action(() => clear(false)));
  addEventListener('pagehide', () => { closed = true; clearTimeout(timer); cache.clear(); });
  notify('正在连接插件…');
  bridge.ready();
  void poll();
})();
