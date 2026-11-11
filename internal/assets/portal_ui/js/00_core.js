'use strict';
// ---------------------------------------------------------------------------
// The portal is server-rendered: every page has its own URL and links are
// plain links. This script only enhances pages: it posts the actions of
// [data-post] buttons and [data-api] forms to the JSON API, shows dialogs and
// toasts, remembers the theme, and submits [data-autosubmit] filters.
// ---------------------------------------------------------------------------

const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const ico = (name, cls = 'size-4 shrink-0') => `<svg class="${cls}" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><use href="${document.documentElement.dataset.sprite || '/assets/icons.svg'}#${name}"></use></svg>`;

// ---- toasts -----------------------------------------------------------------------------
const TOAST = {
  success: ['alert alert-success max-w-sm shadow-lg', ico('circle-check')],
  error: ['alert alert-error max-w-sm shadow-lg', ico('octagon-alert')],
  info: ['alert alert-info max-w-sm shadow-lg', ico('info')],
};
function toast(msg, kind = 'success') {
  const box = $('#toasts');
  if (!box) return;
  const [cls, icon] = TOAST[kind] || TOAST.info;
  const el = document.createElement('div');
  el.className = cls;
  el.innerHTML = `${icon}<span>${esc(msg)}</span>`;
  box.appendChild(el);
  setTimeout(() => el.remove(), kind === 'error' ? 9000 : 4000);
}
// A message shown on the next page, after an action navigates or reloads.
function flash(msg, kind = 'success') {
  try { sessionStorage.setItem('portal.flash', JSON.stringify({ msg, kind })); } catch { /* storage unavailable */ }
}
function showFlash() {
  try {
    const f = JSON.parse(sessionStorage.getItem('portal.flash') || 'null');
    sessionStorage.removeItem('portal.flash');
    if (f?.msg) toast(f.msg, f.kind);
  } catch { /* storage unavailable */ }
}

// ---- dialogs (daisyUI modal on <dialog>) ---------------------------------------------------
function confirmDialog({ text, title, ok, danger }) {
  const d = $('#confirm-dialog');
  if (!d) return Promise.resolve(window.confirm(text));
  $('[data-confirm-title]', d).textContent = title || 'Are you sure?';
  $('[data-confirm-text]', d).textContent = text || '';
  const okBtn = $('[data-confirm-ok]', d);
  okBtn.textContent = ok || 'Confirm';
  okBtn.className = `btn btn-sm touch:min-h-11 ${danger ? 'btn-error' : 'btn-primary'}`;
  d.returnValue = '';
  d.showModal();
  okBtn.focus();
  return new Promise((resolve) => d.addEventListener('close', () => resolve(d.returnValue === 'ok'), { once: true }));
}
function showOutput(title, text) {
  const d = $('#output-dialog');
  $('[data-output-title]', d).textContent = title;
  $('[data-output-body]', d).textContent = text || '(no output)';
  d.showModal();
  return new Promise((resolve) => d.addEventListener('close', resolve, { once: true }));
}

// ---- API --------------------------------------------------------------------------------
async function api(url, { method = 'POST', body } = {}) {
  const res = await fetch(url, {
    method,
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: method === 'GET' ? undefined : JSON.stringify(body ?? {}),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    // The API says what went wrong as {code, message}.
    const e = data.error || {};
    const err = new Error(e.message || `The portal answered ${res.status} and did not say why.`);
    err.code = e.code || '';
    throw err;
  }
  return data;
}

// ---- errors of an action ----------------------------------------------------------------
// An action that failed says so next to the control that started it, and the message stays there
// until the person dismisses it or tries again: it does not leave on a timer.
function errorHost(control) {
  return control.closest('form') || control.closest('td, li') || control.parentElement;
}
function clearError(control) {
  const host = errorHost(control);
  $$('[data-action-error]', host).forEach((el) => el.remove());
  if (host.nextElementSibling?.matches('[data-action-error]')) host.nextElementSibling.remove();
}
function showError(control, err) {
  clearError(control);
  const box = document.createElement('div');
  box.setAttribute('role', 'alert');
  box.dataset.actionError = '';
  // The words are in the color of the page's text, which every theme draws to be read on a tint of
  // its error color; the icon keeps the tone.
  box.className = 'alert alert-error alert-soft mt-3 w-full items-start text-start text-sm text-base-content';
  box.innerHTML = `${ico('octagon-alert', 'size-4 shrink-0 text-error')}<div class="min-w-0 grow"><p class="break-words">${esc(err.message || String(err))}</p>${err.code ? `<p class="font-mono text-xs">${esc(err.code)}</p>` : ''}</div>`
    + `<button type="button" class="btn btn-xs btn-ghost touch:min-h-11 touch:min-w-11" data-dismiss-error aria-label="Dismiss this error">${ico('x')}</button>`;
  const host = errorHost(control);
  // Inside a form, a table cell or a list row the message is part of it; next to a lone button it
  // goes after the group the button is in, so that it does not break the row of buttons.
  if (host.matches('form, td')) host.append(box);
  else if (host.matches('li')) { box.classList.add('list-col-wrap'); host.append(box); } else host.after(box);
  box.scrollIntoView({ block: 'nearest' });
}

function go(url, msg) {
  if (msg) flash(msg);
  window.location.assign(url);
}
function reload(msg, kind = 'success') {
  if (msg) flash(msg, kind);
  window.location.reload();
}

// What happens after an action succeeded: open what it started (the action says where its page is),
// show its output, bring the regions of the page up to date, or reload the page.
async function afterAction(res, opts, title) {
  if (res.href) return go(res.href, opts.toast || `${title}: started`);
  if (opts.refresh === 'regions') {
    await refreshRegions();
    // An action that says how it went (a ping that found its engine down did what it was asked)
    // says it here, when its control has no words of its own for it.
    return toast(opts.toast || res.summary || 'Done', !opts.toast && res.ok === false ? 'error' : 'success');
  }
  if (opts.title) {
    await showOutput(opts.title, typeof res.output === 'string' ? res.output : JSON.stringify(res, null, 2));
    return reload();
  }
  const msg = res.summary || opts.toast || 'Done';
  return reload(msg, res.ok === false ? 'error' : 'success');
}

function busy(el, on) {
  el.disabled = on;
  el.setAttribute('aria-busy', on ? 'true' : 'false');
  if (on) el.insertAdjacentHTML('afterbegin', '<span class="loading loading-spinner loading-xs" data-spin></span>');
  else $$('[data-spin]', el).forEach((s) => s.remove());
}

async function runButton(b) {
  const d = b.dataset;
  if (d.confirm && !(await confirmDialog({ text: d.confirm, title: d.confirmTitle, ok: d.confirmOk, danger: d.danger === '1' }))) return;
  busy(b, true);
  clearError(b);
  if (d.busy) toast(d.busy, 'info');
  try {
    const res = await api(d.post, { method: d.method || 'POST', body: d.body ? JSON.parse(d.body) : {} });
    await afterAction(res, d, d.title || b.textContent.trim());
  } catch (e) {
    showError(b, e);
  } finally {
    busy(b, false);
  }
}

async function submitForm(f, submitter) {
  const data = Object.fromEntries(new FormData(f, submitter));
  (f.dataset.bool || '').split(',').filter(Boolean).forEach((k) => { data[k] = ['on', '1', 'true'].includes(data[k]); });
  if (f.dataset.extra) Object.assign(data, JSON.parse(f.dataset.extra));
  if (data.dry_run) data.dry_run = true;
  const btn = submitter || $('button[type=submit]', f);
  if (btn) busy(btn, true);
  clearError(f);
  try {
    const res = await api(f.dataset.api, { body: data });
    if (submitter && 'noReload' in submitter.dataset) {
      toast(submitter.dataset.toastOk || 'Done');
      return;
    }
    f.closest('dialog')?.close();
    await afterAction(res, f.dataset, f.dataset.title || 'Action');
  } catch (e) {
    showError(f, e);
  } finally {
    if (btn) busy(btn, false);
  }
}

// ---- themes -----------------------------------------------------------------------------
// The chosen theme goes in a cookie, so the server renders every later page in it.
function setTheme(name) {
  document.documentElement.dataset.theme = name;
  document.cookie = `portal_theme=${encodeURIComponent(name)}; path=/; max-age=31536000; samesite=lax`;
  $$('input.theme-controller').forEach((i) => { i.checked = i.value === name; });
}

// ---- stacked tables ---------------------------------------------------------------------
// Below 1280 px a table.table-stack shows each row as a card (portal.css), and each cell names its
// column: the text of its header, copied here so that no template repeats it. A header that is only
// for screen readers (the actions column) gives no label. root is the page, or a page asked for
// again and not shown yet: labelled the same way, a region that did not change is told from one
// that did.
function labelTables(root = document) {
  $$('table.table-stack', root).forEach((t) => {
    const heads = $$('thead th', t).map((th) => (th.querySelector('.sr-only') ? '' : th.textContent.trim()));
    $$('tbody tr', t).forEach((tr) => {
      [...tr.children].forEach((cell, i) => {
        if (heads[i]) cell.dataset.label = heads[i];
        else delete cell.dataset.label;
      });
    });
  });
}

// ---- texts shown by their first lines -----------------------------------------------------
// A details[data-clamp] shows the first lines of a text and opens to show the rest. When no line is
// cut there is no rest: the text is then a plain paragraph, and not a control that does nothing.
function fitClamps() {
  $$('details[data-clamp]').forEach((d) => {
    const text = $('[data-clamp-text]', d);
    if (!text || d.open || text.scrollHeight > text.clientHeight + 1) return;
    const p = document.createElement('p');
    p.className = d.className;
    p.textContent = text.textContent;
    d.replaceWith(p);
  });
}

// ---- navigation drawer ------------------------------------------------------------------
// The label is the button; the checkbox behind it holds the state (daisyUI). Enter and Space work
// on the button, Escape closes the drawer and gives the focus back to it.
function drawerButtonState(open) {
  const btn = $('label[for="nav-drawer"]');
  btn?.setAttribute('aria-expanded', open ? 'true' : 'false');
  btn?.setAttribute('aria-label', open ? 'Close the navigation' : 'Open the navigation');
  return btn;
}
function setDrawer(open) {
  const box = $('#nav-drawer');
  if (!box) return;
  box.checked = open;
  const btn = drawerButtonState(open);
  if (!open) btn?.focus();
}

// ---- events -----------------------------------------------------------------------------
document.addEventListener('click', (ev) => {
  const dismiss = ev.target.closest('[data-dismiss-error]');
  if (dismiss) { dismiss.closest('[data-action-error]')?.remove(); return; }
  const b = ev.target.closest('[data-post]');
  if (b) { ev.preventDefault(); runButton(b); return; }
  const open = ev.target.closest('[data-open-dialog]');
  if (open) { document.getElementById(open.dataset.openDialog)?.showModal(); return; }
  const close = ev.target.closest('[data-close-dialog]');
  if (close) close.closest('dialog')?.close();
});

document.addEventListener('submit', (ev) => {
  const f = ev.target.closest('form[data-api]');
  if (!f) return;
  ev.preventDefault();
  submitForm(f, ev.submitter);
});

document.addEventListener('change', (ev) => {
  const t = ev.target;
  if (t.matches('input.theme-controller')) {
    setTheme(t.value);
    $('#theme-menu')?.hidePopover?.();
    toast(`Theme: ${t.getAttribute('aria-label') || t.value}`, 'info');
    return;
  }
  if (t.id === 'nav-drawer') {
    drawerButtonState(t.checked);
    return;
  }
  const auto = t.closest('[data-autosubmit]');
  if (auto) (auto.form || auto.closest('form') || auto).requestSubmit?.();
});

document.addEventListener('keydown', (ev) => {
  if (ev.key === 'Escape' && $('#nav-drawer')?.checked && !$('dialog[open]') && !document.querySelector(':popover-open')) {
    ev.preventDefault();
    setDrawer(false);
    return;
  }
  if ((ev.key === 'Enter' || ev.key === ' ') && ev.target.matches?.('label[for="nav-drawer"]')) {
    ev.preventDefault();
    setDrawer(!$('#nav-drawer').checked);
    return;
  }
  if (ev.key !== '/' || ev.metaKey || ev.ctrlKey || ev.altKey) return;
  if (/^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName) || document.activeElement?.isContentEditable) return;
  const input = $('#goto-input');
  if (input && input.offsetParent) { ev.preventDefault(); input.focus(); }
});

window.addEventListener('DOMContentLoaded', () => { showFlash(); labelTables(); fitClamps(); });
