'use strict';
// ---------------------------------------------------------------------------
// File viewers ([data-viewer]): the server renders the file in a <pre>; this
// adds find with match navigation, line wrap, copy, and rendered Markdown.
// ---------------------------------------------------------------------------

// Markdown to safe HTML: everything is escaped first. The viewer's container styles the tags.
function renderMarkdown(src) {
  const inline = (t) => esc(t)
    .replace(/`([^`]+)`/g, '<code>$1</code>')
    .replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>')
    .replace(/(^|[^*\w])\*([^*\s][^*]*)\*/g, '$1<i>$2</i>')
    .replace(/\[([^\]]+)\]\((https?:\/\/[^)\s]+|\/[^)\s]*)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
  const lines = String(src).replace(/\r/g, '').split('\n');
  const out = [];
  const isBlockStart = (l) => /^(```|#{1,4}\s|>|\s*([-*+]|\d+\.)\s|---+\s*$)/.test(l);
  for (let i = 0; i < lines.length;) {
    const l = lines[i];
    let m;
    if (/^```/.test(l)) {
      const buf = [];
      for (i++; i < lines.length && !/^```/.test(lines[i]); i++) buf.push(lines[i]);
      i++;
      out.push(`<pre><code>${esc(buf.join('\n'))}</code></pre>`);
    } else if ((m = /^(#{1,4})\s+(.*)$/.exec(l))) {
      out.push(`<h${m[1].length}>${inline(m[2])}</h${m[1].length}>`); i++;
    } else if (/^---+\s*$/.test(l)) {
      out.push('<hr>'); i++;
    } else if (l.includes('|') && /^\s*\|?\s*:?-{2,}/.test(lines[i + 1] || '')) {
      const cells = (row) => row.trim().replace(/^\||\|$/g, '').split('|').map((c) => c.trim());
      const head = cells(l);
      const rows = [];
      for (i += 2; i < lines.length && lines[i].includes('|'); i++) rows.push(cells(lines[i]));
      out.push(`<div class="overflow-x-auto"><table><thead><tr>${head.map((c) => `<th>${inline(c)}</th>`).join('')}</tr></thead><tbody>${rows.map((r) => `<tr>${r.map((c) => `<td>${inline(c)}</td>`).join('')}</tr>`).join('')}</tbody></table></div>`);
    } else if (/^>/.test(l)) {
      const buf = [];
      for (; i < lines.length && /^>/.test(lines[i]); i++) buf.push(lines[i].replace(/^>\s?/, ''));
      out.push(`<blockquote>${inline(buf.join(' '))}</blockquote>`);
    } else if ((m = /^(\s*)([-*+]|\d+\.)\s+(.*)$/.exec(l))) {
      const ordered = /\d/.test(m[2]);
      const items = [];
      for (; i < lines.length; i++) {
        const li = /^(\s*)([-*+]|\d+\.)\s+(.*)$/.exec(lines[i]);
        if (!li) break;
        const task = /^\[([ xX])\]\s+(.*)$/.exec(li[3]);
        const depth = Math.min(4, Math.floor(li[1].length / 2));
        const pad = ['', 'ml-4', 'ml-8', 'ml-12', 'ml-16'][depth];
        items.push(task
          ? `<li class="${pad} flex items-start gap-2 [list-style-type:none]"><input type="checkbox" class="checkbox checkbox-xs mt-1" disabled${task[1] === ' ' ? '' : ' checked'} aria-label="${task[1] === ' ' ? 'to do' : 'done'}"><span>${inline(task[2])}</span></li>`
          : `<li class="${pad}">${inline(li[3])}</li>`);
      }
      out.push(`<${ordered ? 'ol' : 'ul'}>${items.join('')}</${ordered ? 'ol' : 'ul'}>`);
    } else if (!l.trim()) {
      i++;
    } else {
      const buf = [];
      for (; i < lines.length && lines[i].trim() && !isBlockStart(lines[i]); i++) buf.push(lines[i]);
      if (!buf.length) { buf.push(lines[i]); i++; }
      out.push(`<p>${inline(buf.join(' '))}</p>`);
    }
  }
  return out.join('\n');
}

const MARK = 'rounded-sm bg-warning text-warning-content';
const MARK_CUR = 'rounded-sm bg-primary text-primary-content';

function mountViewer(root) {
  const q = (n) => $(`[data-v=${n}]`, root);
  const body = q('body');
  if (!body) return;
  const md = q('md');
  const isMd = /\.md$/i.test(root.dataset.name || '');
  const st = { src: body.textContent, mode: isMd ? (root.dataset.md || 'rendered') : 'source', q: '', cur: -1 };
  let wrap = false;
  try { wrap = localStorage.getItem('portal.wrap') === '1'; } catch { /* storage unavailable */ }
  q('wrap').checked = wrap;

  function applyWrap() {
    body.classList.toggle('whitespace-pre', !wrap);
    body.classList.toggle('whitespace-pre-wrap', wrap);
    body.classList.toggle('break-words', wrap);
  }
  function highlight() {
    const text = st.src;
    if (!st.q) { body.textContent = text; q('count').textContent = ''; return 0; }
    const re = new RegExp(st.q.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), 'gi');
    let html = '';
    let last = 0;
    let n = 0;
    let m;
    while (n < 2000 && (m = re.exec(text))) {
      if (!m[0]) { re.lastIndex++; continue; }
      html += `${esc(text.slice(last, m.index))}<mark class="${MARK}">${esc(m[0])}</mark>`;
      last = m.index + m[0].length;
      n++;
    }
    body.innerHTML = html + esc(text.slice(last));
    q('count').textContent = `${n}${n >= 2000 ? '+' : ''} match${n === 1 ? '' : 'es'}`;
    return n;
  }
  function jump(i) {
    const marks = $$('mark', body);
    if (!marks.length) return;
    st.cur = (i + marks.length) % marks.length;
    marks.forEach((mk, k) => { mk.className = k === st.cur ? MARK_CUR : MARK; });
    marks[st.cur].scrollIntoView({ block: 'center' });
  }
  function paint() {
    const rendered = isMd && st.mode === 'rendered';
    body.hidden = rendered;
    if (md) { md.hidden = !rendered; md.classList.toggle('hidden', !rendered); }
    ['findbox', 'count', 'prev', 'next'].forEach((n) => { const el = q(n); if (el) el.hidden = rendered; });
    if (isMd) {
      const mb = q('mode');
      mb.hidden = false;
      $('span', mb).textContent = rendered ? 'Source' : 'Rendered';
    }
    if (rendered) md.innerHTML = renderMarkdown(st.src);
    else if (highlight()) jump(0);
    applyWrap();
  }
  function follow() {
    if ('follow' in root.dataset) body.scrollTop = body.scrollHeight;
  }

  q('find')?.addEventListener('input', (e) => { st.q = e.target.value; clearTimeout(st.t); st.t = setTimeout(paint, 200); });
  q('find')?.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); jump(st.cur + (e.shiftKey ? -1 : 1)); } });
  q('prev')?.addEventListener('click', () => jump(st.cur - 1));
  q('next')?.addEventListener('click', () => jump(st.cur + 1));
  q('wrap')?.addEventListener('change', (e) => {
    wrap = e.target.checked;
    try { localStorage.setItem('portal.wrap', wrap ? '1' : '0'); } catch { /* storage unavailable */ }
    applyWrap();
  });
  q('mode')?.addEventListener('click', () => { st.mode = st.mode === 'rendered' ? 'source' : 'rendered'; paint(); });
  q('copy')?.addEventListener('click', async () => {
    try { await navigator.clipboard.writeText(st.src); toast('Copied to the clipboard', 'info'); } catch { toast('The browser blocked the clipboard', 'error'); }
  });
  // A live refresh replaced the text of the <pre> (20_live.js keeps the scroll): re-apply the search.
  body.addEventListener('viewer:update', () => {
    st.src = body.textContent;
    paint();
  });
  // A viewer with data-stream follows the output of an execution while it runs. The server sends
  // each chunk as it is written; a connection that drops continues after the last chunk it got,
  // because the browser sends the id of that chunk when it connects again.
  function followStream() {
    const url = new URL(root.dataset.stream, window.location.href);
    if (root.dataset.streamAfter) url.searchParams.set('after', root.dataset.streamAfter);
    const source = new EventSource(url);
    source.addEventListener('log', (ev) => {
      const chunk = JSON.parse(ev.data);
      const atBottom = body.scrollHeight - body.scrollTop - body.clientHeight < 60;
      st.src += chunk.content;
      if (st.q) highlight(); else body.append(chunk.content);
      q('empty')?.setAttribute('hidden', '');
      if (atBottom) body.scrollTop = body.scrollHeight;
    });
    source.addEventListener('done', () => {
      source.close();
      const live = q('live');
      if (live) live.hidden = true;
      // The execution ended: the rest of the page (its status, its attempts, what it can do now) is
      // read once more.
      refreshRegions().catch((e) => markStale(e.message));
    });
  }
  paint();
  follow();
  if (root.dataset.stream) followStream();
}

window.addEventListener('DOMContentLoaded', () => $$('[data-viewer]').forEach(mountViewer));
