// Rebuild implementation-parity fixtures from the pinned public upstream clone.
// Usage: node tools/modeltrace-parity/main.mjs /path/to/ModelTrace
// These are reference samples already used by the bank, not a holdout accuracy test.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const upstream = process.argv[2];
if (!upstream) throw new Error('Provide a local ModelTrace checkout at the revision in provenance.json');
const moduleDir = path.join(root, 'internal/modeltrace');
const provenance = JSON.parse(fs.readFileSync(path.join(moduleDir, 'provenance.json'), 'utf8'));
const read = relative => {
  const data = fs.readFileSync(path.join(upstream, relative));
  assert.equal(crypto.createHash('sha256').update(data).digest('hex'), provenance.files[relative], `Pinned source mismatch: ${relative}`);
  return data.toString('utf8');
};
const moduleFrom = source => import('data:text/javascript;base64,' + Buffer.from(source).toString('base64'));
const scorer = await moduleFrom(read('static/fingerprint-core.js'));
const bank = JSON.parse(read('data/unified_bank.json'));
const rows = ['data/gpt_reference.jsonl', 'data/claude_reference.jsonl'].flatMap(file => read(file).trim().split('\n').map(line => JSON.parse(line)));
const cases = bank.models.flatMap(model => {
  const samples = rows.filter(row => row.source === model.id && row.strict_valid).slice(0, 3);
  assert.equal(samples.length, 3, `Missing references for ${model.id}`);
  return [1, 2, 3].map(count => {
    const answers = samples.slice(0, count).map(row => ({ text: row.text, expected_count: row.requested_count }));
    return { name: `${model.id}/${count}`, answers, expected: scorer.analyzeGlobalOutputs(answers, bank) };
  });
});
// The Go port accepts a random-index function. Replace only the upstream RNG
// with the same deterministic sequence to compare its templates and call order.
let challengeSource = read('static/challenge-browser.js').replace(/function randomIndex\(length\) \{[\s\S]*?\n\}/, 'let cursor = 0; function randomIndex(length) { return cursor++ % length; }');
const challenges = [];
for (const count of [1, 3]) {
  const generator = await moduleFrom(challengeSource + `\n// fresh module ${count}`);
  challenges.push({ count, expected: generator.generateChallenges(count).map(({ expected_count, prompt }) => ({ expected_count, prompt })) });
}
const parserInputs = [
  '说明 99 个：1,2,3\n另一组 4 5',
  '1 2 three 3 4',
  '-1, 2.5, 0, 356, 355',
  '000000000000000000000000000001, 000355, 0000',
  '1 2 999999999999999999999999999999 3',
  '甲 7,8,9 乙 10,11,12',
];
const fixture = {
  revision: provenance.revision,
  scorer_sha256: provenance.files['static/fingerprint-core.js'],
  note: 'Saved-reference implementation parity only; not independent model attribution accuracy.',
  cases,
  challenges,
  parser_cases: parserInputs.map(text => ({ text, expected: scorer.parseNumbers(text) })),
};
const output = path.join(moduleDir, 'testdata/upstream.json');
fs.writeFileSync(output, JSON.stringify(fixture, null, 2) + '\n');
console.log(JSON.stringify({ output, cases: cases.length, modelCount: bank.models.length, note: fixture.note }));
