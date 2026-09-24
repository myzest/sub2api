(() => {
  'use strict';
  const token = new URLSearchParams(location.hash.slice(1)).get('bridge_token');
  const pending = new Map();
  function uuid() {
    const b = crypto.getRandomValues(new Uint8Array(16)); b[6] = (b[6] & 15) | 64; b[8] = (b[8] & 63) | 128;
    const hex = Array.from(b, x => x.toString(16).padStart(2, '0')).join('');
    return `${hex.slice(0,8)}-${hex.slice(8,12)}-${hex.slice(12,16)}-${hex.slice(16,20)}-${hex.slice(20)}`;
  }
  function post(type, payload, requestID) {
    parent.postMessage({ ...payload, source: 'sub2api-plugin-ui', bridge_token: token, type, request_id: requestID }, '*');
  }
  function call(type, payload = {}) {
    return new Promise((resolve, reject) => {
      if (!token || parent === window) { reject(new Error('请从 Sub2API 插件管理页面打开本插件')); return; }
      const id = uuid();
      const timeout = setTimeout(() => { pending.delete(id); reject(new Error('宿主响应超时，请重新打开弹窗检查任务状态')); }, type === 'plugin.status' ? 15000 : 120000);
      pending.set(id, { resolve, reject, timeout }); post(type, payload, id);
    });
  }
  addEventListener('message', event => {
    const message = event.data;
    if (event.source !== parent || !message || message.source !== 'sub2api-plugin-host' || message.bridge_token !== token) return;
    const entry = pending.get(message.request_id); if (!entry) return;
    pending.delete(message.request_id); clearTimeout(entry.timeout);
    if (message.ok) entry.resolve(message);
    else entry.reject(new Error(message.error || message.result?.message || '宿主拒绝了本次操作'));
  });
  addEventListener('pagehide', () => {
    for (const entry of pending.values()) { clearTimeout(entry.timeout); entry.reject(new Error('插件弹窗已关闭')); }
    pending.clear();
  });
  window.InspectorBridge = { call, uuid, ready: () => post('sub2api.plugin.ready', {}, uuid()) };
})();
