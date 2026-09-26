// Regenerate offline parity fixtures from the pinned, original ModelTrace JS.
// Run from any directory: node tools/gen-golden/main.mjs [ModelTrace checkout]
import { readFile, writeFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';

const root = fileURLToPath(new URL('../../', import.meta.url));
const upstream = resolve(process.argv[2] || resolve(root, '.cache/ModelTrace'));
const provenance = JSON.parse(await readFile(resolve(root, 'internal/modeltrace/provenance.json'), 'utf8'));
const digest = value => createHash('sha256').update(value).digest('hex');
const blob = path => {
  const bytes = execFileSync('git', ['-C', upstream, 'show', provenance.revision + ':' + path], { maxBuffer: 64 * 1024 * 1024 });
  if (digest(bytes) !== provenance.files[path]) throw new Error('Upstream hash mismatch: ' + path);
  return bytes;
};
const bankBytes = await readFile(resolve(root, 'internal/modeltrace/unified_bank.json'));
if (digest(bankBytes) !== provenance.files['data/unified_bank.json']) throw new Error('Embedded bank hash mismatch');
const bank = JSON.parse(bankBytes);
const source = blob('static/fingerprint-core.js');
const { analyzeGlobalOutputs } = await import('data:text/javascript;base64,' + source.toString('base64'));
const rows = blob('data/gpt_reference.jsonl').toString('utf8').trim().split(/\r?\n/).map(line => JSON.parse(line));
const golden = [];
for (const model of bank.models.filter(model => model.id.startsWith('gpt-'))) {
  const samples = rows.filter(row => row.model_id === model.id).slice(0, 3);
  if (samples.length !== 3) throw new Error('Missing original samples: ' + model.id);
  for (const count of [1, 3]) {
    const inputs = samples.slice(0, count).map(row => ({ expected_count: row.requested_count, text: row.text }));
    const result = analyzeGlobalOutputs(inputs, bank);
    golden.push({ inputs, prediction: result.prediction, probability: result.probability, used_outputs: result.used_outputs,
      scores: Object.fromEntries(result.results.map(item => [item.model, item.score])) });
  }
}
if (golden.length !== 16) throw new Error('Expected 16 pinned GPT parity cases');
await writeFile(resolve(root, 'internal/modeltrace/testdata/golden.json'), JSON.stringify(golden, null, 2) + '\n');
process.stdout.write('Generated ' + golden.length + ' cases from ModelTrace ' + provenance.revision + '\n');
