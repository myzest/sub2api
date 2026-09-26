# Third-party notices

Codex Inspector is a local Sub2API host transport plugin. It is not a Codex marketplace plugin and is not an official OpenAI product.

## Sub2API SDK and packaging reference

The byte-pinned protocol files in internal/pluginapi/v1 are copied from this repository's backend/pkg/pluginapi/v1. UPSTREAM.json records the exact source revision and SHA-256 hashes. Do not reformat, translate, or silently regenerate those files.

The Go plugin skeleton, UI Bridge v1 message pattern, and Ed25519 archive packaging were adapted from the Sub2API host documentation and the existing local gpt-inspector plugin. LICENSE retains the repository's GNU Lesser General Public License v3 text. Distribution must retain the corresponding notices and comply with the applicable upstream license terms.

## ModelTrace

The fingerprint bank and scoring/challenge port are derived from ModelTrace by xqy2006:
https://github.com/xqy2006/ModelTrace

The pinned revision, individual upstream file hashes, and import notes are recorded in internal/modeltrace/provenance.json. Its MIT license is preserved verbatim in internal/modeltrace/LICENSE.ModelTrace. The packager includes both files under licenses/ and the fingerprint bank is embedded in the plugin binary.

ModelTrace statistical matching is not a measurement of IQ, reasoning ability, identity authenticity, or an established fact of service degradation. The limitations and provenance must remain visible when distributing derived results.

## Dependencies

Go runtime/library versions are locked by go.mod and go.sum. The browser test tool Playwright is a development dependency pinned in package-lock.json and is not shipped in the plugin archive. No signing private key, account token, proxy password, or administrator credential belongs in the package.
