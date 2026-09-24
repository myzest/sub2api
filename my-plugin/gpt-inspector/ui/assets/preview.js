(() => {
  'use strict';
  const policy = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; connect-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'";
  function htmlFromAnswer(text) {
    const trimmed = text.trim();
    // Models sometimes wrap HTML in prose or leave the closing fence missing
    // after an interruption. Only the preview removes that presentation layer.
    const fence = trimmed.match(/```(?:html|svg|xml)?[ \t]*\r?\n([\s\S]*?)(?:\r?\n[ \t]*```|$)/i);
    return fence ? fence[1] : text;
  }
  window.InspectorPreview = {
    show(container, text) {
      const frame = document.createElement('iframe');
      frame.setAttribute('sandbox', 'allow-scripts');
      frame.setAttribute('referrerpolicy', 'no-referrer');
      frame.title = '鹈鹕动画预览';
      // The original reply stays untouched. Only the isolated preview receives
      // a stricter network policy, inherited in addition to the host's CSP.
      frame.srcdoc = '<!doctype html><html><head><meta http-equiv="Content-Security-Policy" content="' + policy + '"></head><body>' + htmlFromAnswer(text) + '</body></html>';
      container.replaceChildren(frame); container.hidden = false;
    },
    clear(container) { container.replaceChildren(); container.hidden = true; }
  };
})();
