const { test } = require('node:test');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const http = require('node:http');
const fs = require('node:fs/promises');
const path = require('node:path');
const { randomUUID, createHash } = require('node:crypto');
const { gzipSync } = require('node:zlib');

const root = path.resolve(__dirname, '..');
const uiRoot = process.env.INSPECTOR_UI_DIR || path.join(root, 'ui');
const hostCSP = "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'; navigate-to 'none'";
const bank = {
  revision: 'fixture-revision', sha256: 'fixture-bank-hash', built_at: '2026-10-09T06:24:27Z',
  models: [
    { id: 'fixture-model', display_name: 'Fixture Model', family: 'gpt', family_name: 'GPT' },
    { id: 'other-model', display_name: 'Other Model', family: 'claude', family_name: 'Claude' },
  ],
};
const fixturePage = `<!doctype html><html><head><meta charset="utf-8"></head><body style="margin:0">
<button id="close">关闭弹窗</button><button id="open">重新打开</button><div id="mount"></div><script>
function mount() {
  const frame = document.createElement('iframe');
  frame.id = 'plugin'; frame.sandbox = 'allow-scripts';
  frame.src = '/plugin/index.html#bridge_token=fixture-token';
  frame.style = 'width:100%;height:1400px;border:0';
  document.getElementById('mount').replaceChildren(frame);
}
document.getElementById('close').onclick = () => document.getElementById('mount').replaceChildren();
document.getElementById('open').onclick = mount;
addEventListener('message', async event => {
  const message = event.data, frame = document.getElementById('plugin');
  if (!frame || event.source !== frame.contentWindow || event.origin !== 'null' || message.source !== 'sub2api-plugin-ui' || message.bridge_token !== 'fixture-token') return;
  if (message.type === 'sub2api.plugin.ready') return;
  const response = await fetch('/rpc', { method: 'POST', body: JSON.stringify(message) });
  const reply = await response.json();
  event.source.postMessage({ ...reply, source: 'sub2api-plugin-host', bridge_token: 'fixture-token', request_id: message.request_id }, '*');
});
mount();
</script></body></html>`;

function analysis(round, model, overrides = {}) {
  const winner = round === 1 ? bank.models[0] : bank.models[1];
  return {
    round, state: 'complete', covered: bank.models.some(candidate => candidate.id === model), warnings: [],
    result: {
      prediction: winner.id, prediction_name: winner.display_name, probability: 0.9, used_outputs: 3,
      family_prediction_name: winner.family_name, family_probability: 0.9,
      family_probabilities: bank.models.map(candidate => ({ family: candidate.family, display_name: candidate.family_name, probability: candidate === winner ? 0.9 : 0.1 })),
      results: [winner, ...bank.models.filter(candidate => candidate !== winner)].map((candidate, index) => ({
        model: candidate.id, display_name: candidate.display_name, probability: index ? 0.1 : 0.9,
        profile_similarity: 0.7, score: index ? -1 : 1, family: candidate.family, family_name: candidate.family_name,
        conditional_probability: 1,
      })),
      diagnostics: [0, 1, 2].map(index => ({ index, parsed_numbers: 310, minimum_numbers: 171, accepted: true })),
    },
    ...overrides,
  };
}

async function fixture({ models, delay = 80, hold = false, answerFor, analysisFor, readResult } = {}) {
  const state = {
    instance: randomUUID(), ready: true, bank,
    accounts: [
      { id: 7, name: 'Fixture GPT 账号', schedulable: true },
      { id: 8, name: '暂停账号', schedulable: false },
      { id: 9, name: 'Fixture B', schedulable: true },
    ],
    stored: [], active: [], bytes: 0, results: 0,
  };
  const records = new Map(), outputs = new Map(), calls = [], timers = new Set();
  let config = {}, healthy = true, modelsFail = false, frozenStatus = null, completions = [];

  function update() {
    state.stored = [...records.values()].map(record => {
      const batches = [record.latest, record.pending].filter(Boolean), batch = batches.at(-1);
      const blobs = [...outputs.entries()].filter(([key]) => batches.some(batch => key.startsWith(batch.id + ':')));
      return {
        account_id: record.account_id, name: record.name, bytes: blobs.reduce((sum, [, blob]) => sum + blob.length, 1000),
        results: batches.reduce((sum, batch) => sum + batch.items.filter(item => item.parts).length, 0),
        batch_id: batch.id, state: batch.state, created_at: batch.created_at,
      };
    });
    state.bytes = state.stored.reduce((sum, record) => sum + record.bytes, 0);
    state.results = state.stored.reduce((sum, record) => sum + record.results, 0);
  }
  function dropOutput(batch) {
    if (batch) for (const key of outputs.keys()) if (key.startsWith(batch.id + ':')) outputs.delete(key);
  }
  function finish(record, batch) {
    dropOutput(record.latest);
    record.latest = batch;
    delete record.pending;
    state.active = state.active.filter(active => active.id !== batch.id);
    update();
  }
  function later(account, fn) {
    if (hold) { completions.push({ account, fn }); return; }
    const timer = setTimeout(() => { timers.delete(timer); fn(); }, delay);
    timers.add(timer);
  }
  async function dispatch(command) {
    calls.push(structuredClone(command));
    switch (command.action) {
      case 'accounts': return state.accounts;
      case 'models':
        if (modelsFail) throw new Error('fixture catalog unavailable');
        return models ? models(command) : [{ id: 'fixture-model', name: 'Fixture Model', efforts: ['none', 'low', 'high'] }];
      case 'start': {
        if (state.active.some(batch => batch.account_id === command.account_id)) throw new Error('此账号已有测试批次正在运行');
        const account = state.accounts.find(account => account.id === command.account_id);
        const batch = { ...command, id: command.id, account_name: account.name, state: 'running', created_at: new Date().toISOString(), bank, items: [], analyses: [] };
        for (let round = 1; round <= command.rounds; round++) for (let probe = 1; probe <= 3; probe++) {
          batch.items.push({ round, probe, state: 'queued', session_id: randomUUID(), prompt: { text: `第 ${round} 轮挑战 ${probe}：生成 310 个整数。`, expected_count: 310 } });
        }
        const record = { account_id: command.account_id, name: account.name, pending: batch, latest: records.get(command.account_id)?.latest };
        records.set(command.account_id, record);
        state.active.push(batch);
        const running = () => records.get(command.account_id)?.pending === batch;
        function nextRound(round) {
          if (!running()) return;
          if (round > command.rounds) { batch.state = 'completed'; finish(record, batch); return; }
          for (const item of batch.items) if (item.round === round) { item.state = 'running'; item.started_at = new Date().toISOString(); }
          update();
          later(command.account_id, () => {
            if (!running()) return;
            batch.items.forEach((item, index) => {
              if (item.round !== round) return;
              const answer = {
                prompt: item.prompt, text: `原始回答：账号 ${command.account_id} · 第 ${round} 轮 · 挑战 ${item.probe} <script>parent.pwned=true</script>`,
                session_id: item.session_id, response_id: `response-${batch.id}-${index}`, usage: { input_tokens: 20, output_tokens: 50 },
                quality: { parsed_numbers: 310, minimum_numbers: 171, accepted: true, warnings: [] },
              };
              if (answerFor) Object.assign(answer, answerFor(answer, item));
              const blob = gzipSync(JSON.stringify(answer));
              outputs.set(batch.id + ':' + index, blob);
              Object.assign(item, { parts: 1, sha256: createHash('sha256').update(blob).digest('hex'), state: answer.error ? 'failed' : 'completed', error: answer.error, attempt: answer.attempts?.length || 1, duration_ms: delay });
            });
            batch.analyses.push(analysisFor ? analysisFor(round, batch) : analysis(round, batch.model));
            update();
            nextRound(round + 1);
          });
        }
        nextRound(1);
        return { batch_id: batch.id };
      }
      case 'read_batch': return records.get(command.account_id) || { account_id: command.account_id };
      case 'read_result': {
        if (readResult) await readResult(command);
        const blob = outputs.get(command.batch_id + ':' + command.item);
        if (!blob) throw new Error('fixture result missing');
        return { encoding: 'gzip-base64', data: blob.toString('base64') };
      }
      case 'clear_all': case 'clear_account': {
        for (const [id, record] of records) if (command.action === 'clear_all' || id === command.account_id) {
          dropOutput(record.latest); dropOutput(record.pending); records.delete(id);
        }
        state.active = state.active.filter(batch => records.has(batch.account_id));
        update();
        return { clearing: true };
      }
      case 'stop': {
        const record = records.get(command.account_id), batch = record?.pending;
        if (batch) {
          if (batch.id !== command.batch_id) throw new Error('运行批次已变化');
          batch.state = 'interrupted';
          for (const item of batch.items) if (['queued', 'running'].includes(item.state)) item.state = 'interrupted';
          finish(record, batch);
        }
        return { stopping: true };
      }
      default: throw new Error('unknown fixture command');
    }
  }
  const server = http.createServer(async (request, response) => {
    try {
      if (request.url === '/rpc') {
        let body = '';
        for await (const chunk of request) body += chunk;
        const message = JSON.parse(body);
        let reply;
        if (message.type === 'plugin.status') reply = { ok: true, result: { healthy, status_json: healthy ? JSON.stringify(frozenStatus || state) : '' } };
        else if (message.type === 'config.save') { config = message.config; reply = { ok: true, config }; }
        else if (message.type === 'config.test') {
          try {
            const command = config.command;
            reply = { ok: true, result: { success: true, status_json: JSON.stringify({ command_id: command.id, data: await dispatch(command) }) } };
          } catch (error) { reply = { ok: true, result: { success: false, message: error.message } }; }
        } else reply = { ok: false, error: 'unsupported fixture bridge method' };
        response.setHeader('Content-Type', 'application/json');
        response.end(JSON.stringify(reply));
        return;
      }
      if (request.url.startsWith('/plugin/')) {
        const relative = request.url.slice('/plugin/'.length);
        if (relative.includes('..')) throw new Error('invalid path');
        const content = await fs.readFile(path.join(uiRoot, relative));
        response.setHeader('Content-Security-Policy', hostCSP);
        response.setHeader('Cross-Origin-Resource-Policy', 'cross-origin');
        response.setHeader('Content-Type', relative.endsWith('.js') ? 'text/javascript' : relative.endsWith('.css') ? 'text/css' : 'text/html');
        response.end(content);
        return;
      }
      response.setHeader('Content-Type', 'text/html');
      response.end(fixturePage);
    } catch (error) { response.statusCode = 500; response.end(error.message); }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  return {
    url: `http://127.0.0.1:${server.address().port}`, state, calls,
    freezeStatus() { frozenStatus = structuredClone(state); }, resumeStatus() { frozenStatus = null; },
    advance(id) {
      const due = completions.filter(completion => completion.account === id);
      completions = completions.filter(completion => completion.account !== id);
      for (const completion of due) completion.fn();
    },
    setModelsFail(value) { modelsFail = value; }, setHealthy(value) { healthy = value; },
    async close() {
      for (const timer of timers) clearTimeout(timer);
      server.closeAllConnections();
      await new Promise(resolve => server.close(resolve));
    },
  };
}

async function waitUntil(fn, timeout = 15000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    if (await fn()) return;
    await new Promise(resolve => setTimeout(resolve, 30));
  }
  throw new Error('condition timed out');
}
async function withUI(options, callback) {
  const host = await fixture(options);
  const executablePath = process.env.CHROME_BIN || (process.platform === 'darwin' ? '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome' : undefined);
  const browser = await chromium.launch({ headless: true, executablePath, args: ['--disable-gpu'] });
  const page = await browser.newPage({ viewport: { width: 1120, height: 1100 } });
  const pageErrors = [];
  page.on('pageerror', error => pageErrors.push(error.message));
  try {
    await page.goto(host.url);
    const ui = page.frameLocator('#plugin');
    await ui.locator('#account option[value="7"]').waitFor({ state: 'attached' });
    await callback({ host, browser, page, ui });
    assert.deepEqual(pageErrors, []);
  } finally { await browser.close(); await host.close(); }
}
async function select(ui, account = 7, model = 'fixture-model') {
  await ui.locator('#account').selectOption(String(account));
  await ui.locator(`#model option[value="${model}"]`).waitFor({ state: 'attached' });
  await ui.locator('#model').selectOption(model);
  await ui.locator('#effort').selectOption('high');
}
async function start(ui, host, account = 7, rounds = 1) {
  await select(ui, account);
  await ui.locator('#rounds').fill(String(rounds));
  const count = host.calls.filter(command => command.action === 'start').length;
  await ui.locator('#start').click();
  await waitUntil(() => host.calls.filter(command => command.action === 'start').length > count);
}
async function screenshot(page, name) {
  await fs.mkdir(path.join(root, 'test-results'), { recursive: true });
  const frame = page.locator('#plugin'), body = page.frameLocator('#plugin').locator('body');
  const viewport = page.viewportSize(), previousHeight = await frame.evaluate(frame => frame.style.height);
  const height = await body.evaluate(body => Math.ceil(Math.max(body.scrollHeight, body.getBoundingClientRect().height)));
  // Capture the sandbox after it paints, without nested scrolling or a blank
  // out-of-process iframe surface in Chrome's full-page screenshot compositor.
  try {
    await frame.evaluate((frame, height) => { frame.style.height = `${height}px`; }, height);
    await page.setViewportSize({ width: viewport.width, height: Math.max(viewport.height, height + 30) });
    await body.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await frame.screenshot({ path: path.join(root, 'test-results', name + '.png'), animations: 'disabled' });
  } finally {
    await frame.evaluate((frame, height) => { frame.style.height = height; }, previousHeight);
    await page.setViewportSize(viewport);
  }
}

test('默认参数、三挑战、关闭弹窗继续运行、每轮归因与原文安全展示', { timeout: 45000 }, async () => {
  await withUI({}, async ({ host, page, ui }) => {
    const external = [];
    page.on('request', request => { if (!request.url().startsWith(host.url)) external.push(request.url()); });
    assert.equal(await ui.locator('#model').inputValue(), '');
    assert.equal(await ui.locator('#effort').inputValue(), '');
    assert.equal(await ui.locator('#rounds').inputValue(), '1');
    assert.equal(await ui.locator('#timeout').inputValue(), '30');
    assert.equal(await ui.locator('#prompts').count(), 0);
    assert(await ui.locator('#start').isDisabled());
    await screenshot(page, 'new-test');
    await select(ui);
    await ui.locator('#effort').selectOption('none');
    await ui.locator('#model').selectOption('');
    assert.equal(await ui.locator('#effort').inputValue(), '');
    await ui.locator('#model').selectOption('fixture-model');
    await ui.locator('#effort').selectOption('high');
    await ui.locator('#rounds').fill('2');
    assert.match(await ui.locator('#request-count').textContent(), /2 轮 × 3 条挑战 = 6 次请求/);
    await ui.locator('#start').click();
    await waitUntil(() => host.calls.some(command => command.action === 'start'));
    const command = host.calls.find(command => command.action === 'start');
    assert.equal(command.timeout_minutes, 30);
    assert.equal(command.prompts, undefined);
    await page.locator('#close').click();
    await waitUntil(() => host.state.results === 6 && host.state.active.length === 0);
    await page.locator('#open').click();
    ui = page.frameLocator('#plugin');
    await ui.locator('#account option[value="7"]').waitFor({ state: 'attached' });
    assert.equal(await ui.locator('#model').inputValue(), '');
    await ui.locator('#results-tab').click();
    await waitUntil(async () => (await ui.locator('#answer-text').textContent()).startsWith('原始回答'));
    assert.equal(await ui.locator('#items button').count(), 3);
    assert.equal(await ui.locator('.candidate-table tbody tr').count(), 2);
    assert.match(await ui.locator('.winner').textContent(), /Fixture Model/);
    assert.match(await ui.locator('#analysis-panel').textContent(), /库内归因概率/);
    assert.match(await ui.locator('.family-probabilities').textContent(), /GPT 90\.0% · Claude 10\.0%/);
    assert.match(await ui.locator('.bank-provenance').textContent(), /fixture-bank-hash/);
    assert.equal(await ui.locator('#answer-text script').count(), 0);
    assert.equal(await page.evaluate(() => window.pwned), undefined);
    const info = JSON.parse(await ui.locator('#answer-usage').textContent());
    await ui.locator('#round').selectOption('2');
    await waitUntil(async () => (await ui.locator('#answer-text').textContent()).includes('第 2 轮'));
    assert.match(await ui.locator('.winner').textContent(), /Other Model/);
    const next = JSON.parse(await ui.locator('#answer-usage').textContent());
    assert.notEqual(info.session_id, next.session_id);
    assert.notEqual(info.response_id, next.response_id);
    assert.deepEqual(external, []);
    await screenshot(page, 'round-results');
    await page.setViewportSize({ width: 390, height: 844 });
    assert(await ui.locator('body').evaluate(body => body.scrollWidth <= body.clientWidth));
    await screenshot(page, 'mobile-results');
  });
});

test('运行中追加账号、停止与清理隔离、全部清理不会恢复迟到回答', { timeout: 45000 }, async () => {
  await withUI({ hold: true }, async ({ host, page, ui }) => {
    await start(ui, host, 7, 2);
    await start(ui, host, 9, 2);
    await waitUntil(async () => await ui.locator('.task-card').count() === 2);
    assert.match(await ui.locator('.task-card[data-account-id="7"] .task-detail').textContent(), /同时运行 3 条/);
    await screenshot(page, 'parallel-accounts');
    const firstBatch = host.state.active.find(batch => batch.account_id === 7).id;
    await ui.locator('.task-card[data-account-id="7"] [data-stop-batch]').click();
    await waitUntil(() => host.state.active.length === 1);
    assert.equal(host.state.active[0].account_id, 9);
    assert.equal(host.calls.find(command => command.action === 'stop').batch_id, firstBatch);
    await page.locator('#close').click();
    await page.locator('#open').click();
    ui = page.frameLocator('#plugin');
    await waitUntil(async () => await ui.locator('.task-card').count() === 1);
    await start(ui, host, 7);
    await ui.locator('#results-tab').click();
    await ui.locator('#result-account').selectOption('7');
    await ui.locator('#clear-account').click();
    await ui.locator('#confirm-ok').click();
    await waitUntil(() => !host.state.stored.some(record => record.account_id === 7));
    assert.equal(host.state.active[0].account_id, 9);
    await ui.locator('#clear-all').click();
    await ui.locator('#confirm-ok').click();
    await waitUntil(async () => await ui.locator('#storage-total').textContent() === '0 B');
    host.advance(7); host.advance(9);
    assert.equal(host.state.bytes, 0);
    assert.equal(host.state.active.length, 0);
    assert(await ui.locator('#empty').isVisible());
  });
});

test('目录错误无回退，快速切换账号丢弃过期目录，none 是有效档位', { timeout: 30000 }, async () => {
  let release, requests = 0;
  const gate = new Promise(resolve => { release = resolve; });
  try {
    await withUI({ models: async command => {
      if (++requests === 1) { await gate; return [{ id: 'stale-model', efforts: ['low'] }]; }
      return [{ id: command.account_id === 7 ? 'fresh-model' : 'other-model', efforts: ['none', 'high'] }];
    } }, async ({ host, ui }) => {
      host.setModelsFail(true);
      await ui.locator('#account').selectOption('7');
      await waitUntil(async () => (await ui.locator('#model-note').textContent()).includes('获取失败'));
      assert(await ui.locator('#start').isDisabled());
      host.setModelsFail(false);
      await ui.locator('#retry-models').click();
      await waitUntil(() => requests === 1);
      await ui.locator('#account').selectOption('9');
      await ui.locator('#account').selectOption('7');
      release();
      await ui.locator('#model option[value="fresh-model"]').waitFor({ state: 'attached' });
      assert.deepEqual(await ui.locator('#model option').evaluateAll(options => options.map(option => option.value)), ['', 'fresh-model']);
      assert.equal(await ui.locator('#model').inputValue(), '');
      assert.equal(await ui.locator('#effort').inputValue(), '');
      await ui.locator('#model').selectOption('fresh-model');
      await ui.locator('#effort').selectOption('none');
      assert(!(await ui.locator('#start').isDisabled()));
      assert.match(await ui.locator('#model-note').textContent(), /未覆盖/);
    });
  } finally { release(); }
});

test('未覆盖、部分样本、无有效样本分别显示，错误不伪装成归因成功', { timeout: 30000 }, async () => {
  await withUI({
    models: () => [{ id: 'unknown-model', efforts: ['high'] }],
    analysisFor: (round, batch) => {
      const value = analysis(round, batch.model);
      value.warnings = ['model_not_covered', 'response_incomplete', 'insufficient_numbers'];
      value.state = round === 1 ? 'partial' : 'unavailable';
      value.result.used_outputs = round === 1 ? 2 : 0;
      value.result.diagnostics[0].accepted = false;
      if (round === 2) { value.result.results = []; value.error = '没有可用回答'; }
      return value;
    },
  }, async ({ host, ui }) => {
    await select(ui, 7, 'unknown-model');
    await ui.locator('#rounds').fill('2');
    await ui.locator('#start').click();
    await waitUntil(() => host.state.results === 6);
    await waitUntil(async () => await ui.locator('#result-count').textContent() === '6');
    await ui.locator('#results-tab').click();
    await ui.locator('.analysis-warning').waitFor();
    assert.match(await ui.locator('.analysis-warning').textContent(), /未覆盖请求模型 unknown-model/);
    assert.match(await ui.locator('.analysis-warning').textContent(), /2\/3/);
    assert.match(await ui.locator('.analysis-warning').textContent(), /未完整结束/);
    assert.match(await ui.locator('.diagnostics').textContent(), /挑战 1：310 个数字 · 忽略/);
    await ui.locator('#round').selectOption('2');
    assert.equal(await ui.locator('.candidate-table').count(), 0);
    assert.match(await ui.locator('.analysis-empty').textContent(), /没有可用于归因/);
    assert(!((await ui.locator('#analysis-panel').textContent()).includes('undefined')));
  });
});

test('新批次在轮询之间完成仍更新原文，模型与档位提交后清空', { timeout: 35000 }, async () => {
  let answerNumber = 0;
  await withUI({ hold: true, answerFor: () => ({ text: `本次回答 ${++answerNumber}` }) }, async ({ host, ui }) => {
    await start(ui, host);
    host.advance(7);
    await waitUntil(async () => await ui.locator('#result-count').textContent() === '3');
    await ui.locator('#results-tab').click();
    await waitUntil(async () => await ui.locator('#answer-text').textContent() === '本次回答 1');
    const old = JSON.parse(await ui.locator('#answer-usage').textContent());
    host.freezeStatus();
    await ui.locator('#new-tab').click();
    await start(ui, host);
    host.advance(7);
    host.resumeStatus();
    await waitUntil(async () => await ui.locator('#model').inputValue() === '');
    assert.equal(await ui.locator('#effort').inputValue(), '');
    await ui.locator('#results-tab').click();
    await waitUntil(async () => await ui.locator('#answer-text').textContent() === '本次回答 4');
    const next = JSON.parse(await ui.locator('#answer-usage').textContent());
    assert.notEqual(old.session_id, next.session_id);
    assert.notEqual(old.response_id, next.response_id);
  });
});

test('切换轮次时迟到的原文不会覆盖当前轮次', { timeout: 30000 }, async () => {
  let release, blocked = false;
  const gate = new Promise(resolve => { release = resolve; });
  try {
    await withUI({ readResult: async command => {
      if (command.item === 0 && !blocked) { blocked = true; await gate; }
    } }, async ({ host, ui }) => {
      await start(ui, host, 7, 2);
      await waitUntil(() => host.state.results === 6);
      await waitUntil(async () => await ui.locator('#result-count').textContent() === '6');
      await ui.locator('#results-tab').click();
      await waitUntil(() => blocked);
      await ui.locator('#round').selectOption('2');
      release();
      await waitUntil(async () => (await ui.locator('#answer-text').textContent()).includes('第 2 轮'));
      assert.equal(await ui.locator('#round').inputValue(), '2');
      assert.match(await ui.locator('.winner').textContent(), /Other Model/);
      assert.match(await ui.locator('#answer-title').textContent(), /第 2 轮/);
    });
  } finally { release(); }
});

test('旧轮次读取失败不污染新轮次，当前读取失败仍显示', { timeout: 30000 }, async () => {
  let release, blocked = false;
  const gate = new Promise(resolve => { release = resolve; });
  try {
    await withUI({ readResult: async command => {
      if (command.item === 0 && !blocked) {
        blocked = true;
        await gate;
        throw new Error('OLD_ROUND_READ_ERROR');
      }
      if (command.item === 4) throw new Error('CURRENT_RESULT_READ_ERROR');
    } }, async ({ host, ui }) => {
      await start(ui, host, 7, 2);
      await waitUntil(() => host.state.results === 6);
      await waitUntil(async () => await ui.locator('#result-count').textContent() === '6');
      await ui.locator('#results-tab').click();
      await waitUntil(() => blocked);
      await ui.locator('#round').selectOption('2');
      release();
      await waitUntil(async () => (await ui.locator('#answer-text').textContent()).includes('第 2 轮'));
      assert(await ui.locator('#error').isHidden(), '旧轮次的读取错误不应污染新轮次');
      await ui.locator('#items [data-item="4"]').click();
      await waitUntil(async () => (await ui.locator('#error').textContent()).includes('CURRENT_RESULT_READ_ERROR'));
      assert(await ui.locator('#error').isVisible(), '当前选中回答的真实读取错误必须可见');
    });
  } finally { release(); }
});

test('批次级存储错误与已保存原文同时可见', { timeout: 30000 }, async () => {
  await withUI({ hold: true }, async ({ host, ui }) => {
    await start(ui, host);
    const batch = host.state.active[0];
    host.advance(7);
    // A round-index write can fail after all answers are saved; a successful
    // final commit preserves its error on Batch, without a global store error.
    batch.error = '保存本轮归因失败：fixture storage error';
    batch.state = 'failed';
    host.state.stored[0].state = 'failed';
    await waitUntil(async () => await ui.locator('#result-count').textContent() === '3');
    await ui.locator('#results-tab').click();
    await waitUntil(async () => (await ui.locator('#answer-text').textContent()).startsWith('原始回答'));
    assert.match(await ui.locator('#batch-info').textContent(), /失败/);
    assert(await ui.locator('#batch-error').isVisible());
    assert.equal(await ui.locator('#batch-error').textContent(), batch.error);
    assert(await ui.locator('#answer-error').isHidden(), '批次错误不应改写完整回答的成功状态');
  });
});

test('重新选择同条原文时旧失败不覆盖重读成功', { timeout: 30000 }, async () => {
  let release, reads = 0;
  const gate = new Promise(resolve => { release = resolve; });
  try {
    await withUI({ readResult: async command => {
      if (command.item === 0 && ++reads === 1) {
        await gate;
        throw new Error('OLD_ATTEMPT_READ_ERROR');
      }
    } }, async ({ host, ui }) => {
      try {
        await start(ui, host);
        await waitUntil(async () => await ui.locator('#result-count').textContent() === '3');
        await ui.locator('#results-tab').click();
        await waitUntil(() => reads === 1);
        await ui.locator('#items [data-item="0"]').click();
        release();
        await waitUntil(() => reads >= 2);
        await waitUntil(async () => (await ui.locator('#answer-text').textContent()).startsWith('原始回答'));
        assert(await ui.locator('#error').isHidden(), '同条结果的旧请求错误不应污染重新读取');
      } finally { release(); }
    });
  } finally { release(); }
});

test('插件停止与重启清除目录选择，重新获取后才允许提交', { timeout: 30000 }, async () => {
  await withUI({}, async ({ host, ui }) => {
    await select(ui);
    host.setHealthy(false);
    await waitUntil(async () => (await ui.locator('#notice').textContent()).includes('请先在插件管理页启用'));
    assert.equal(await ui.locator('#model').inputValue(), '');
    assert.equal(await ui.locator('#effort').inputValue(), '');
    assert(await ui.locator('#start').isDisabled());
    host.state.instance = randomUUID();
    host.setHealthy(true);
    await waitUntil(async () => !(await ui.locator('#refresh-accounts').isDisabled()));
    assert.equal(await ui.locator('#model').inputValue(), '');
    assert(await ui.locator('#start').isDisabled());
  });
});
