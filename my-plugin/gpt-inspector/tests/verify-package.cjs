const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { execFileSync } = require('node:child_process');

const root = path.resolve(__dirname,'..');
const version = JSON.parse(fs.readFileSync(path.join(root,'package.json'),'utf8')).version;
const artifact = path.resolve(process.argv[2] || path.join(root,`dist/gpt-inspector-${version}.s2plugin`));
const read = name => execFileSync('unzip',['-p',artifact,name],{maxBuffer:64*1024*1024});
const names = execFileSync('unzip',['-Z1',artifact],{encoding:'utf8'}).trim().split('\n');
assert.equal(new Set(names).size,names.length,'duplicate archive paths');
assert(names.every(n=>!n.startsWith('/')&&!n.split('/').includes('..')&&!n.includes('\\')),'unsafe archive path');
assert(names.every(n=>!n.includes('.signing')&&!/\.(pem|key)$/i.test(n)),'private material included');
const raw = read('manifest.json'), manifest=JSON.parse(raw), signature=JSON.parse(read('signature.json'));
const config = fs.readFileSync(path.join(root,'dist/trusted-publisher.yaml'),'utf8');
const line = config.split('\n').find(line=>line.trim().startsWith(signature.key_id+':'));
assert(line,'publisher configuration missing');
const encoded = line.split(':').slice(1).join(':').trim().replace(/^"|"$/g,'');
const key = crypto.createPublicKey({format:'der',type:'spki',key:Buffer.concat([Buffer.from('302a300506032b6570032100','hex'),Buffer.from(encoded,'base64')])});
assert.equal(signature.algorithm,'ed25519');
assert(crypto.verify(null,raw,key,Buffer.from(signature.signature,'base64')),'signature does not match installation public key');
assert.equal(manifest.id,'local.sub2api.gpt-inspector');
assert.equal(manifest.version,version);
assert.equal(manifest.ui.entrypoint,'ui/index.html');
assert.deepEqual(names.filter(n=>!['manifest.json','signature.json'].includes(n)).sort(),Object.keys(manifest.files).sort());
for(const [name,expected] of Object.entries(manifest.files)) {
  const bytes=read(name);assert.equal(crypto.createHash('sha256').update(bytes).digest('hex'),expected,`hash mismatch: ${name}`);
  if(name.startsWith('ui/'))assert(bytes.equals(fs.readFileSync(path.join(root,name))),`stale packaged UI: ${name}`);
  if(name.startsWith('runtimes/'))assert(bytes.equals(fs.readFileSync(path.join(root,'build',name))),`stale packaged runtime: ${name}`);
}
for(const target of ['linux-amd64','linux-arm64','darwin-arm64'])assert(manifest.files[manifest.runtimes[target].path],`missing runtime: ${target}`);
const content=fs.readFileSync(artifact),sha256=crypto.createHash('sha256').update(content).digest('hex');
fs.writeFileSync(artifact+'.sha256',sha256+'  '+path.basename(artifact)+'\n');
console.log(JSON.stringify({artifact,bytes:content.length,sha256,files:names.length,platforms:Object.keys(manifest.runtimes),signature_verified:true,packaged_files_match_build:true,private_material_included:false},null,2));
