(() => {
  'use strict';

  function node(tag, text, className) {
    const element = document.createElement(tag);
    if (text != null) element.textContent = text;
    if (className) element.className = className;
    return element;
  }

  function percent(value) {
    return Number.isFinite(value) ? `${(value * 100).toFixed(1)}%` : '未提供';
  }

  const warningsByCode = {
    model_not_covered: '指纹库未覆盖请求模型，候选结果不能用于判断该模型是否被替换。',
    response_incomplete: '存在未完整结束的回答，该回答未计入归因。',
    insufficient_numbers: '存在数字数量不足的样本。',
    constant_sequence: '样本全部为相同数字，未遵循逐项独立选择要求。',
    ascending_sequence: '样本为连续递增序列，未遵循挑战要求。',
    descending_sequence: '样本为连续递减序列，未遵循挑战要求。',
    duplicate_outputs: '本轮存在重复回答，请结合原文判断，重复本身不能证明模型异常。',
  };
  const warningText = code => warningsByCode[code] || code;

  function render(container, batch, round, bank) {
    container.replaceChildren(node('h2', `第 ${round} 轮 · 指纹归因`));
    const analysis = batch.analyses?.find(item => item.round === round);
    const result = analysis?.result;
    const provenance = batch.bank || bank;
    const covered = analysis?.covered ?? provenance?.models?.some(model => model.id === batch.model);
    const warnings = (analysis?.warnings || []).filter(code => code !== 'model_not_covered').map(warningText);
    if (!covered) warnings.unshift(`指纹库未覆盖请求模型 ${batch.model}，候选结果不能用于判断该模型是否被替换。`);
    if (analysis?.error) warnings.push(analysis.error);
    if (analysis?.state === 'partial') warnings.unshift(`本轮仅有 ${result?.used_outputs || 0}/3 份有效样本，归因不完整。`);
    if (warnings.length) container.append(node('p', [...new Set(warnings)].join('\n'), 'notice warning analysis-warning'));

    if (provenance) container.append(node('p', `参考库：${provenance.built_at || '未提供构建时间'} · revision ${provenance.revision?.slice(0, 12) || '未提供'} · SHA-256 ${provenance.sha256 || '未提供'}`, 'muted small bank-provenance'));
    if (result?.diagnostics?.length) {
      const diagnostics = node('div', null, 'diagnostics');
      result.diagnostics.forEach((item, index) => diagnostics.append(node(
        'span', `挑战 ${(item.index ?? index) + 1}：${item.parsed_numbers} 个数字 · ${item.accepted ? '计入' : '忽略'}`,
      )));
      container.append(diagnostics);
    }
    if (!result?.used_outputs) {
      const pending = batch.items.some(item => item.round === round && ['queued', 'running'].includes(item.state));
      container.append(node('p', pending ? '等待本轮挑战完成后计算归因。' : '本轮没有可用于归因的有效样本。', 'muted analysis-empty'));
      return;
    }

    const summary = node('div', null, 'analysis-summary');
    for (const [label, value] of [
      ['第一候选', result.prediction_name || result.prediction],
      ['库内归因概率', percent(result.probability)],
      ['模型家族', `${result.family_prediction_name || '未提供'} · ${percent(result.family_probability)}`],
      ['有效样本', `${result.used_outputs}/3`],
    ]) {
      const metric = node('div');
      metric.append(node('span', label, 'muted small'), node('strong', value));
      summary.append(metric);
    }
    container.append(summary);
    if (result.family_probabilities?.length) {
      const families = result.family_probabilities.map(family => `${family.display_name || family.family} ${percent(family.probability)}`);
      container.append(node('p', `各模型家族：${families.join(' · ')}`, 'muted small family-probabilities'));
    }

    const table = node('table');
    const head = node('tr');
    for (const label of ['候选模型', '家族', '库内归因概率', '分布相似度']) head.append(node('th', label));
    const thead = node('thead');
    thead.append(head);
    const tbody = node('tbody');
    for (const [index, candidate] of (result.results || []).entries()) {
      const row = node('tr', null, index === 0 ? 'winner' : '');
      row.append(
        node('td', candidate.display_name || candidate.model),
        node('td', candidate.family_name || candidate.family),
        node('td', percent(candidate.probability)),
        node('td', percent(candidate.profile_similarity)),
      );
      tbody.append(row);
    }
    table.append(thead, tbody);
    const wrap = node('div', null, 'candidate-table');
    wrap.append(table);
    container.append(wrap);

    container.append(node('p', '概率是当前参考库内候选之间的相对归因结果。模型更新、推理强度、系统提示词和样本质量都可能影响结果。', 'muted small'));
  }

  window.ModelTraceView = { render, warningText };
})();
