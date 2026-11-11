'use strict';
// ---------------------------------------------------------------------------
// Recent: the pages opened in this browser (localStorage), newest first. Every
// page records itself; the /recent page lists them by day with a section
// filter and a search.
// ---------------------------------------------------------------------------

const Recent = {
  key: 'portal.recent',
  max: 100,
  sections: { inbox: 'Needs you', board: 'Board', changes: 'Changes', gates: 'Changes', executions: 'Executions', backlog: 'Backlog', merge: 'Merge queue', cycles: 'Cycles', visual: 'Visual QA', events: 'Events', quality: 'Assessments', agents: 'Agents', system: 'System' },
  section(path) { return this.sections[path.split('?')[0].split('/')[1]]; },
  // The list is of pages the portal has. An address recorded before a page left the portal is in no
  // section of it any more, and is dropped as the list is read.
  read() {
    try {
      return (JSON.parse(localStorage.getItem(this.key)) || []).map((r) => ({ ...r, section: this.section(r.path) })).filter((r) => r.section);
    } catch { return []; }
  },
  write(list) {
    try { localStorage.setItem(this.key, JSON.stringify(list.slice(0, this.max).map(({ path, title, at }) => ({ path, title, at })))); } catch { /* storage unavailable */ }
  },
  add(path, title) {
    if (path === '/' || path.startsWith('/recent') || !title || title === 'Error' || title === 'Not valid' || !this.section(path)) return;
    const list = this.read().filter((r) => r.path !== path);
    // An address that is not found is not a page: it is taken off the list instead of put on it.
    if (title !== 'Not found') list.unshift({ path, title, at: new Date().toISOString() });
    this.write(list);
  },
  clear() { try { localStorage.removeItem(this.key); } catch { /* storage unavailable */ } },
};

function recentDay(iso) {
  const d = new Date(iso);
  const days = Math.round((new Date().setHours(0, 0, 0, 0) - new Date(d).setHours(0, 0, 0, 0)) / 86400000);
  if (days === 0) return 'Today';
  if (days === 1) return 'Yesterday';
  return d.toLocaleDateString(undefined, { weekday: 'long', month: 'short', day: 'numeric' });
}

// decodePath shows a recorded address decoded, or as recorded when it is not valid percent-encoding
// (/events?q=100%): decodeURIComponent throws on it.
function decodePath(p) {
  try { return decodeURIComponent(p); } catch { return p; }
}

function renderRecent() {
  const listEl = $('[data-recent-list]');
  if (!listEl) return;
  const params = new URLSearchParams(window.location.search);
  const section = params.get('section') || 'All';
  const input = $('[data-recent-q]');
  const all = Recent.read();

  function paint() {
    const q = (input.value || '').trim().toLowerCase();
    const base = all.filter((r) => !q || `${r.title} ${decodePath(r.path)}`.toLowerCase().includes(q));
    const counts = base.reduce((a, r) => ({ ...a, [r.section]: (a[r.section] || 0) + 1 }), {});
    const tabs = [['All', base.length], ...Object.keys(counts).sort().map((s) => [s, counts[s]])];
    $('[data-recent-sections]').innerHTML = `<div role="tablist" class="tabs tabs-box tabs-sm bg-base-100" aria-label="Section">${tabs.map(([s, n]) => {
      const href = s === 'All' ? '/recent' : `/recent?section=${encodeURIComponent(s)}`;
      const active = s === section;
      // A tab that is not the chosen one is drawn the way the server draws its own: daisyUI pales it
      // below what is read.
      return active
        ? `<a role="tab" href="${href}" class="tab tab-active gap-2 text-base-content touch:min-h-11 touch:min-w-11" aria-selected="true">${esc(s)}<span class="badge badge-sm badge-ghost tabular-nums">${n}</span></a>`
        : `<a role="tab" href="${href}" class="tab gap-2 text-base-content/80 hover:text-base-content touch:min-h-11 touch:min-w-11">${esc(s)}<span class="badge badge-sm badge-ghost tabular-nums">${n}</span></a>`;
    }).join('')}</div>`;
    const rows = section === 'All' ? base : base.filter((r) => r.section === section);
    if (!rows.length) {
      listEl.innerHTML = $(all.length ? '#recent-nomatch' : '#recent-empty').innerHTML;
      return;
    }
    let day = '';
    let html = '';
    rows.forEach((r) => {
      const d = recentDay(r.at);
      if (d !== day) {
        html += `${day ? '</ul>' : ''}<h2 class="border-y border-base-content/10 bg-base-200/95 px-4 py-1.5 font-mono text-xs uppercase tracking-[0.16em] text-base-content/70">${esc(d)}</h2><ul class="list">`;
        day = d;
      }
      const shown = decodePath(r.path);
      const time = new Date(r.at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
      html += `<li class="list-row items-center py-2.5"><span class="w-12 font-mono text-xs tabular-nums text-base-content/60">${esc(time)}</span>`
        + `<div class="flex min-w-0 flex-col"><a href="${esc(r.path)}" class="link link-hover block truncate font-medium touch:py-3">${esc(r.title)}</a><span class="truncate font-mono text-xs text-base-content/60">${esc(shown)}</span></div>`
        + `<span class="badge badge-sm badge-ghost">${esc(r.section)}</span></li>`;
    });
    listEl.innerHTML = `${html}</ul>`;
  }
  input.addEventListener('input', paint);
  $('[data-recent-clear]')?.addEventListener('click', async () => {
    if (!(await confirmDialog({ text: 'Forget every page listed here? This only clears this browser\'s list.', title: 'Clear history?', ok: 'Clear the list', danger: true }))) return;
    Recent.clear();
    reload('History cleared', 'info');
  });
  paint();
}

window.addEventListener('DOMContentLoaded', () => {
  Recent.add(window.location.pathname + window.location.search, document.body.dataset.title);
  renderRecent();
});
