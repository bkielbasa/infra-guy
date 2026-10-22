(function () {
  'use strict';

  function escapeHTML(str) {
    return str
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;');
  }

  const regoTokenRe = new RegExp(
    [
      '(#[^\\n]*)',                                                                                     // 1: comment
      '("(?:\\\\.|[^"\\\\])*"|`[^`]*`)',                                                                // 2: string
      '\\b(package|import|default|if|contains|in|some|every|not|as|with|else)\\b',                      // 3: keyword
      '\\b(allow|deny|rego|v1|input|data)\\b',                                                          // 4: builtin identifier
      '\\b(true|false|null)\\b',                                                                        // 5: boolean / null
      '\\b(count|sprintf|startswith|endswith|regex|match|split|trim|lower|upper|to_number|walk|print)\\b(?=\\s*[.(])', // 6: function
      '(-?\\b\\d+(?:\\.\\d+)?\\b)',                                                                     // 7: number
      '(:=|==|!=|<=|>=|[=<>+\\-*/%|&])',                                                                // 8: operator
      '([{}()\\[\\].,;:])'                                                                              // 9: punctuation
    ].join('|'),
    'g'
  );

  function highlightRego(src) {
    let out = '';
    let lastIdx = 0;
    regoTokenRe.lastIndex = 0;
    let m;
    while ((m = regoTokenRe.exec(src)) !== null) {
      if (m.index > lastIdx) {
        out += escapeHTML(src.slice(lastIdx, m.index));
      }
      const tok = escapeHTML(m[0]);
      if (m[1] !== undefined) {
        out += '<span class="tok-comment">' + tok + '</span>';
      } else if (m[2] !== undefined) {
        out += '<span class="tok-string">' + tok + '</span>';
      } else if (m[3] !== undefined) {
        out += '<span class="tok-keyword">' + tok + '</span>';
      } else if (m[4] !== undefined) {
        out += '<span class="tok-builtin">' + tok + '</span>';
      } else if (m[5] !== undefined) {
        out += '<span class="tok-bool">' + tok + '</span>';
      } else if (m[6] !== undefined) {
        out += '<span class="tok-func">' + tok + '</span>';
      } else if (m[7] !== undefined) {
        out += '<span class="tok-number">' + tok + '</span>';
      } else if (m[8] !== undefined) {
        out += '<span class="tok-op">' + tok + '</span>';
      } else if (m[9] !== undefined) {
        out += '<span class="tok-punct">' + tok + '</span>';
      } else {
        out += tok;
      }
      lastIdx = regoTokenRe.lastIndex;
    }
    if (lastIdx < src.length) {
      out += escapeHTML(src.slice(lastIdx));
    }
    return out;
  }

  const jsonTokenRe = new RegExp(
    [
      '("(?:\\\\.|[^"\\\\])*")(\\s*:)',                     // 1: key, 2: colon
      '("(?:\\\\.|[^"\\\\])*")',                            // 3: string value
      '\\b(true|false|null)\\b',                            // 4: boolean / null
      '(-?\\b\\d+(?:\\.\\d+)?(?:[eE][+\\-]?\\d+)?\\b)',     // 5: number
      '([{}()\\[\\].,:])'                                   // 6: punctuation
    ].join('|'),
    'g'
  );

  function highlightJSON(src) {
    let out = '';
    let lastIdx = 0;
    jsonTokenRe.lastIndex = 0;
    let m;
    while ((m = jsonTokenRe.exec(src)) !== null) {
      if (m.index > lastIdx) {
        out += escapeHTML(src.slice(lastIdx, m.index));
      }
      if (m[1] !== undefined) {
        out += '<span class="tok-key">' + escapeHTML(m[1]) + '</span><span class="tok-punct">' + escapeHTML(m[2]) + '</span>';
      } else if (m[3] !== undefined) {
        out += '<span class="tok-string">' + escapeHTML(m[3]) + '</span>';
      } else if (m[4] !== undefined) {
        out += '<span class="tok-bool">' + escapeHTML(m[4]) + '</span>';
      } else if (m[5] !== undefined) {
        out += '<span class="tok-number">' + escapeHTML(m[5]) + '</span>';
      } else if (m[6] !== undefined) {
        out += '<span class="tok-punct">' + escapeHTML(m[6]) + '</span>';
      } else {
        out += escapeHTML(m[0]);
      }
      lastIdx = jsonTokenRe.lastIndex;
    }
    if (lastIdx < src.length) {
      out += escapeHTML(src.slice(lastIdx));
    }
    return out;
  }

  function highlightText(src, lang) {
    if (lang === 'json') {
      return highlightJSON(src);
    }
    return highlightRego(src);
  }

  function mountEditor(textarea) {
    if (textarea.dataset.editorMounted === '1') {
      return;
    }
    textarea.dataset.editorMounted = '1';
    const lang = textarea.getAttribute('data-code-editor') || 'rego';

    textarea.setAttribute('spellcheck', 'false');
    textarea.setAttribute('autocomplete', 'off');
    textarea.setAttribute('autocapitalize', 'off');

    const wrapper = document.createElement('div');
    wrapper.className = 'code-editor';
    wrapper.setAttribute('data-lang', lang);

    const gutter = document.createElement('div');
    gutter.className = 'code-editor-gutter';
    gutter.setAttribute('aria-hidden', 'true');

    const body = document.createElement('div');
    body.className = 'code-editor-body';

    const pre = document.createElement('pre');
    pre.className = 'code-editor-highlight';
    pre.setAttribute('aria-hidden', 'true');

    const code = document.createElement('code');
    code.className = 'language-' + lang;
    pre.appendChild(code);

    textarea.parentNode.insertBefore(wrapper, textarea);
    body.appendChild(pre);
    body.appendChild(textarea);
    wrapper.appendChild(gutter);
    wrapper.appendChild(body);

    let lastLineCount = 0;
    function syncContent() {
      const val = textarea.value || '';
      const highlighted = highlightText(val, lang);
      code.innerHTML = val.endsWith('\n') ? highlighted + '\n ' : highlighted;

      const lineCount = Math.max(1, val.split('\n').length);
      if (lineCount !== lastLineCount) {
        lastLineCount = lineCount;
        let nums = '';
        for (let i = 1; i <= lineCount; i++) {
          nums += '<span>' + i + '</span>';
        }
        gutter.innerHTML = nums;
      }
      syncScroll();
    }

    function syncScroll() {
      pre.scrollTop = textarea.scrollTop;
      pre.scrollLeft = textarea.scrollLeft;
      gutter.scrollTop = textarea.scrollTop;
    }

    textarea.addEventListener('input', syncContent);
    textarea.addEventListener('scroll', syncScroll);
    textarea.addEventListener('keydown', function (e) {
      if (e.key === 'Tab') {
        e.preventDefault();
        const start = textarea.selectionStart;
        const end = textarea.selectionEnd;
        const val = textarea.value;
        if (e.shiftKey) {
          const lineStart = val.lastIndexOf('\n', start - 1) + 1;
          if (val.slice(lineStart, lineStart + 2) === '  ') {
            textarea.value = val.slice(0, lineStart) + val.slice(lineStart + 2);
            textarea.selectionStart = Math.max(lineStart, start - 2);
            textarea.selectionEnd = Math.max(lineStart, end - 2);
            syncContent();
          } else if (val.slice(lineStart, lineStart + 1) === '\t') {
            textarea.value = val.slice(0, lineStart) + val.slice(lineStart + 1);
            textarea.selectionStart = Math.max(lineStart, start - 1);
            textarea.selectionEnd = Math.max(lineStart, end - 1);
            syncContent();
          }
        } else {
          textarea.value = val.slice(0, start) + '  ' + val.slice(end);
          textarea.selectionStart = textarea.selectionEnd = start + 2;
          syncContent();
        }
      } else if (e.key === 'Enter') {
        const start = textarea.selectionStart;
        const end = textarea.selectionEnd;
        const val = textarea.value;
        const lineStart = val.lastIndexOf('\n', start - 1) + 1;
        const currentLine = val.slice(lineStart, start);
        const indentMatch = currentLine.match(/^[ \t]+/);
        let indent = indentMatch ? indentMatch[0] : '';
        const trimmed = currentLine.trimEnd();
        if (trimmed.endsWith('{') || trimmed.endsWith('[')) {
          indent += '  ';
        }
        if (indent.length > 0) {
          e.preventDefault();
          const insert = '\n' + indent;
          textarea.value = val.slice(0, start) + insert + val.slice(end);
          textarea.selectionStart = textarea.selectionEnd = start + insert.length;
          syncContent();
        }
      }
    });

    syncContent();
  }

  function mountStaticHighlight(codeEl) {
    if (codeEl.dataset.highlightMounted === '1') {
      return;
    }
    codeEl.dataset.highlightMounted = '1';
    const lang = codeEl.getAttribute('data-code-highlight') || 'json';
    const raw = codeEl.textContent || '';
    codeEl.innerHTML = highlightText(raw, lang);
  }

  function initAll(root) {
    const scope = root || document;
    scope.querySelectorAll('textarea[data-code-editor]').forEach(mountEditor);
    scope.querySelectorAll('code[data-code-highlight]').forEach(mountStaticHighlight);
  }

  const preservedDetails = new Map();
  const stepPrevStatus = new Map();
  const savedLogScroll = new Map();

  function getDetailsKey(el) {
    if (!el || !el.dataset) return null;
    if (el.dataset.step) return 'step:' + el.dataset.step;
    if (el.dataset.preserveKey) return 'key:' + el.dataset.preserveKey;
    return null;
  }

  document.addEventListener('click', function (e) {
    const summary = e.target && e.target.closest ? e.target.closest('summary') : null;
    if (!summary) return;
    const details = summary.parentElement;
    if (!details || details.tagName !== 'DETAILS') return;
    const key = getDetailsKey(details);
    if (!key) return;
    preservedDetails.set(key, !details.open);
  });

  document.addEventListener('htmx:beforeSwap', function (e) {
    const target = e.target;
    if (!target || !target.querySelectorAll) return;
    target.querySelectorAll('details[data-step]').forEach(function (d) {
      stepPrevStatus.set(d.dataset.step, d.dataset.status || '');
    });
    target.querySelectorAll('pre.log[data-log-step]').forEach(function (pre) {
      const step = pre.dataset.logStep;
      const atBottom = (pre.scrollHeight - pre.scrollTop - pre.clientHeight) <= 32;
      savedLogScroll.set(step, { scrollTop: pre.scrollTop, atBottom: atBottom });
    });
  });

  function restoreRunPanelState(root) {
    const scope = root || document;
    if (!scope.querySelectorAll) return;
    scope.querySelectorAll('details[data-step], details[data-preserve-key]').forEach(function (d) {
      const key = getDetailsKey(d);
      if (!key) return;
      if (d.dataset.step) {
        const prev = stepPrevStatus.get(d.dataset.step);
        const cur = d.dataset.status || '';
        if (cur === 'running' && prev && prev !== 'running') {
          preservedDetails.delete(key);
        }
      }
      if (preservedDetails.has(key)) {
        d.open = preservedDetails.get(key);
      }
    });
    scope.querySelectorAll('pre.log[data-log-step]').forEach(function (pre) {
      const step = pre.dataset.logStep;
      const details = pre.closest('details[data-step]');
      const isRunning = details && details.dataset.status === 'running';
      const saved = savedLogScroll.get(step);
      if (isRunning && (!saved || saved.atBottom)) {
        pre.scrollTop = pre.scrollHeight;
      } else if (saved) {
        pre.scrollTop = saved.scrollTop;
      }
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function () {
      initAll(document);
    });
  } else {
    initAll(document);
  }

  document.addEventListener('htmx:afterSwap', function (e) {
    restoreRunPanelState(e.target);
    initAll(e.target);
  });
})();
