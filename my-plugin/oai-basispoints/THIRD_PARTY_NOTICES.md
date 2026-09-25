# Sources and licenses

This plugin is distributed under LGPL-3.0, matching the Sub2API code used below. The complete editable source and build configuration are included in the separate source archive.

## Sub2API and local GPT Inspector scaffolding

The `internal/pluginapi` protocol, generated bindings and runtime come from this workspace's `backend/pkg/pluginapi/v1`. The publisher packager, static UI bridge and HTTP transport pool were adapted from this workspace's `my-plugin/gpt-inspector`. Their copyright and LGPL-3.0 licensing are retained in `LICENSE` and `internal/pluginapi/NOTICE.md`. This project has its own plugin ID, signing key and runtime implementation.

## Protocol references

- [JaxsonWang/cpa-plugin-oai-basispoints](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints), reviewed at `f4a2563647f807f5bc08e725fa933bcba28d2d11` (v0.1.8), MIT, Copyright (c) 2026 JaxsonWang. Its attachment wire behavior, multipart/cache structure, tool catalog instructions, selection, output replay and compaction handling informed this Go adapter. The license is included at `licenses/cpa-plugin-oai-basispoints.LICENSE`. The earlier v0.1.3 snapshot (`05b2d97efa1bd117da6bd4d362d6e88f8e483680`) was used only as a wire-behavior reference when no license was present in that snapshot.
- [Kaixxrua/excel-codex-bridge](https://github.com/Kaixxrua/excel-codex-bridge), reviewed at `b2d6f2529b6ffa9f1a630f17b7037ef6dcc0480a`, previously `dca684d6afd711620fc471e7c00fe10abbc64c8f`. The Excel request format, tool catalog and `run_officejs` relay behavior informed an independent Go implementation. Its `excel_upstream.py` and `excel_stream.py` acknowledge Nonary/ghcp_proxy. Both Unlicense texts are included under `licenses/excel-codex-bridge.*`. Its image hosting implementation was reviewed but is not used; this plugin uses same-origin BPS attachments.

Version 0.1.8 of this adapter also reviewed CPA commit `708082da2f851569984de395d25405e61c2bbc34` and implements external historical tool-call conversion informed by its `fallbackTransportCall` and Excel's `_fallback_transport_call`. The adapter requires complete call/result pairs and retains strict KV recovery for its own handles. Image filename extensions are explicitly mapped to formats listed in an observed BPS validation error instead of relying on the host MIME database.

The new implementation deliberately does not execute OfficeJS, repair malformed model JSON, share a global call-ID cache, silently release unknown native tools, or lower unknown reasoning levels.

Version 0.1.10 follows CPA commit `708082da2f851569984de395d25405e61c2bbc34` for bounded native-envelope unwrapping and complete-history fallback when no native record is found. It adopts only whole single JSON fences from Excel's `_decode_transport_code`; it does not scan arbitrary text or repair invalid backslashes. Native KV scope checks remain intact; fallback uses only the supplied complete history and never loads another scope's record or executes a historical command.

## Build dependencies

Direct Go dependencies include HashiCorp go-plugin (MPL-2.0), go-hclog (MIT), gRPC-Go (Apache-2.0), protobuf-Go (BSD-3-Clause) and santhosh-tekuri/jsonschema/v6 (Apache-2.0). The module graph and exact versions are recorded in `go.mod` / `go.sum`; available upstream LICENSE, NOTICE and PATENTS files are copied into `licenses/` and included in the signed package.

Playwright (Apache-2.0) is a development-only UI test dependency, recorded in `package-lock.json`; it is not part of the runtime package.

This is a local Sub2API transport adapter, not an official OpenAI integration. Repository descriptions of model quality are hypotheses to evaluate against runtime results, not guarantees provided by this package.
