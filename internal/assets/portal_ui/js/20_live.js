'use strict';
// ---------------------------------------------------------------------------
// Live pages: a page whose <main> has data-live-interval re-requests its own
// URL on that interval and swaps in the regions marked [data-live-region]
// (by id). It is the same server-rendered page, so nothing is rendered twice;
// the refresh pauses while the tab is hidden or the person is typing, and it
// stops once the server renders the page without an interval.
// ---------------------------------------------------------------------------

function liveBusy() {
  const a = document.activeElement;
  return document.hidden
    // A card of the board is being dragged: a refresh would swap the lane under the pointer.
    || document.body.hasAttribute('data-dragging')
    // A control is being pressed: swapped before it is let go, the press would be lost.
    || !!document.querySelector('main :active')
    || !!$('dialog[open]')
    || !!document.querySelector(':popover-open')
    || (a && /^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName) && !a.matches('input.theme-controller'))
    || String(window.getSelection?.() || '').length > 0;
}

// focusKey says how to find again, once a region is drawn anew, the control that had the focus in
// it: by its id, by the action it posts or by where it leads. A summary is found through its
// details (detailsKeys).
function focusKey(el) {
  const quoted = (v) => `"${String(v).replace(/["\\]/g, '\\$&')}"`;
  if (el.id) return `[id=${quoted(el.id)}]`;
  if (el.dataset.post) return `[data-post=${quoted(el.dataset.post)}]${el.dataset.moveTo ? `[data-move-to=${quoted(el.dataset.moveTo)}]` : ''}`;
  if (el.matches('a[href]')) return `a[href=${quoted(el.getAttribute('href'))}]`;
  return '';
}

// drawnOpen remembers how the server drew each details of a live region, open or closed: one that
// is the other way now is one a person opened or closed.
const drawnOpen = new WeakMap();
function markDrawn(root) {
  $$('details', root).forEach((d) => drawnOpen.set(d, d.open));
}

// detailsKeys names each details of a region, so that it is found again once the region is drawn
// anew: by its id when the server gave it one, else by what holds it (the nearest id inside the
// region, and where the first link of its row leads), the words of its summary, and its place among
// the ones that share all of that.
function detailsKeys(region) {
  const counted = new Map();
  return $$('details', region).map((d) => {
    if (d.id) return [d, `#${d.id}`];
    const holder = d.parentElement.closest('[id]');
    const row = d.closest('li, tr, article');
    const where = [
      holder && holder !== region && region.contains(holder) ? holder.id : '',
      (row && region.contains(row) && $('a[href]', row)?.getAttribute('href')) || '',
      $('summary', d)?.textContent.trim() || '',
    ].join('|');
    const nth = counted.get(where) || 0;
    counted.set(where, nth + 1);
    return [d, `${where}|${nth}`];
  });
}

function swapRegions(doc) {
  $$('[data-live-region][id]').forEach((el) => {
    const fresh = doc.getElementById(el.id);
    if (!fresh) return;
    // The error of an action is not something the server renders: a region that holds one is compared
    // without it, and when the region changes the error stays until it is dismissed: on the card it
    // was shown on, when the region is made of cards and that card is still there, or at its end.
    const errors = $$('[data-action-error]', el);
    // A card is known by its kind and its name: a change and a proposal can have the same name.
    const cardKey = (card) => (card ? `${card.dataset.kind || ''}/${card.dataset.card}` : undefined);
    const cards = new Map(errors.map((box) => [box, cardKey(box.closest('[data-card]'))]));
    // What a person opened or closed is not something the server renders either: the region is
    // compared the way the server drew it, and when it changes each of those stays as the person
    // left it, at the line they had read to. The server knows nothing of a click.
    const touched = detailsKeys(el).filter(([d]) => drawnOpen.has(d) && drawnOpen.get(d) !== d.open);
    let shown = el.innerHTML;
    if (errors.length || touched.length) {
      const copy = el.cloneNode(true);
      const drawn = $$('details', el).map((d) => drawnOpen.get(d));
      $$('details', copy).forEach((d, i) => { if (drawn[i] !== undefined) d.open = drawn[i]; });
      $$('[data-action-error]', copy).forEach((box) => box.remove());
      shown = copy.innerHTML;
    }
    if (fresh.innerHTML === shown) return;
    const scrolls = el.matches('pre');
    const atBottom = scrolls && el.scrollHeight - el.scrollTop - el.clientHeight < 60;
    const top = scrolls ? el.scrollTop : 0;
    const kept = new Map(touched.map(([d, key]) => [key, { open: d.open, tops: $$('pre', d).map((pre) => pre.scrollTop) }]));
    // The control with the focus is drawn anew like the rest: the focus goes to the one that took its
    // place, so that a person who uses the keyboard is not sent back to the top of the page.
    const active = el.contains(document.activeElement) ? document.activeElement : null;
    const summaryOf = active?.matches('summary') ? detailsKeys(el).find(([d]) => d === active.parentElement)?.[1] : undefined;
    const focused = active && !summaryOf ? focusKey(active) : '';
    el.innerHTML = fresh.innerHTML;
    markDrawn(el);
    const redrawn = detailsKeys(el);
    redrawn.forEach(([d, key]) => {
      const was = kept.get(key);
      if (!was) return;
      d.open = was.open;
      $$('pre', d).forEach((pre, i) => { if (was.tops[i]) pre.scrollTop = was.tops[i]; });
    });
    if (summaryOf) redrawn.find(([, key]) => key === summaryOf)?.[0].querySelector('summary')?.focus({ preventScroll: true });
    else if (focused) $(focused, el)?.focus({ preventScroll: true });
    errors.forEach((box) => {
      const key = cards.get(box);
      const card = key === undefined ? null : $$('[data-card]', el).find((c) => cardKey(c) === key);
      (card || el).append(box);
    });
    // A text the server draws clamped is fitted again: where no line is cut it has no control.
    fitClamps();
    if (scrolls) {
      el.dispatchEvent(new CustomEvent('viewer:update'));
      el.scrollTop = atBottom ? el.scrollHeight : top;
    }
  });
}

// refreshRegions asks for this page again and swaps its live regions in. The request says what it is
// for: the server then leaves out what the page follows through a stream of its own.
async function refreshRegions() {
  const res = await fetch(window.location.href, { cache: 'no-store', headers: { Accept: 'text/html', 'X-Portal-Refresh': 'regions' } });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  const doc = new DOMParser().parseFromString(await res.text(), 'text/html');
  // The tables of the page shown carry the labels of their columns: the ones of the page asked for
  // get them too, so that a region is drawn anew only when what the server drew in it changed.
  labelTables(doc);
  swapRegions(doc);
  return doc;
}

// The foot of a page says when it was read. A refresh that fails says so there, where it stays until
// one succeeds: the page may be showing what is no longer true.
function markStale(reason) {
  const foot = $('#page-read-at');
  if (!foot || $('[data-stale]', foot)) return;
  foot.insertAdjacentHTML('beforeend', ` <span class="badge badge-sm badge-warning" data-stale>${ico('triangle-alert', 'size-3 shrink-0')}not refreshing: ${esc(reason)}</span>`);
}

function startLive() {
  const main = $('#main');
  const every = Number(main?.dataset.liveInterval || 0);
  if (!every) return;
  let failures = 0;
  const schedule = (ms = every * 1000) => setTimeout(tick, ms);
  async function tick() {
    if (liveBusy()) return schedule(1500);
    try {
      const doc = await refreshRegions();
      failures = 0;
      $$('#page-read-at [data-stale]').forEach((el) => el.remove());
      if (doc.getElementById('main')?.dataset.liveInterval) schedule();
      else toast('This page is up to date: nothing is running any more.', 'info');
    } catch (e) {
      failures++;
      markStale(e.message);
      schedule(Math.min(60000, every * 1000 * (failures + 1)));
    }
  }
  schedule();
}

window.addEventListener('DOMContentLoaded', () => {
  $$('[data-live-region]').forEach(markDrawn);
  startLive();
});
