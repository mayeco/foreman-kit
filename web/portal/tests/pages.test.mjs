// The pages of the portal, opened in a headless Chromium.
//
// `npm test` runs this file through the Go test TestThePagesInABrowser, which serves a portal that
// holds the states the render tests draw and says here where it is (PORTAL_URL) and what to open:
// PORTAL_PAGES, the pages the redesign has rewritten; PORTAL_ACTION_PAGE, a page with an action a
// person can take; PORTAL_LIVE_PAGE, a page that keeps itself up to date; PORTAL_REPORT_PAGE, a page
// with an action that answers with something to read; PORTAL_BOARD_PAGE, the board of changes.
//
// Every page is checked the same way:
//
//   - it loads: no error in the console, no script that throws, no request that fails;
//   - it passes the automatic accessibility check of axe-core, on the WCAG A and AA rules;
//   - the keyboard reaches every control, shows where the focus is and never traps it;
//   - no text is drawn smaller than 12 px, on a desk or on a phone;
//   - on a phone (375 px wide) and on a tablet, upright (820 px) and on its side (1180 px), nothing
//     overflows sideways and every control is 44 px tall and wide.
//
// The last tests check what belongs to no single page: the navigation, the dialogs and the menus on
// a phone, that the error of an action stays until a person dismisses it, that what an action
// answers is shown, that a card of the board is moved by dropping it on a lane, that what a person
// opened stays open when a page brings itself up to date, that the three questions a person comes
// with are answered within two clicks of the first page, and that every theme the menu offers draws
// every page with text that is read.
import { after, before, describe, test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { chromium } from 'playwright-core';

const require = createRequire(import.meta.url);
const axeSource = readFileSync(require.resolve('axe-core/axe.min.js'), 'utf8');

const base = process.env.PORTAL_URL;
const pages = JSON.parse(process.env.PORTAL_PAGES || '[]');
const actionPage = process.env.PORTAL_ACTION_PAGE;
const livePage = process.env.PORTAL_LIVE_PAGE;
const reportPage = process.env.PORTAL_REPORT_PAGE;
const boardPage = process.env.PORTAL_BOARD_PAGE;
if (!base || pages.length === 0 || !actionPage || !livePage || !reportPage || !boardPage) {
  throw new Error('PORTAL_URL, PORTAL_PAGES, PORTAL_ACTION_PAGE, PORTAL_LIVE_PAGE, PORTAL_REPORT_PAGE and PORTAL_BOARD_PAGE are not set: run `npm test` in web/portal, which starts the portal these tests open');
}

const desktop = { viewport: { width: 1280, height: 900 } };
const phone = { viewport: { width: 375, height: 812 }, hasTouch: true, isMobile: true, deviceScaleFactor: 2 };
// A tablet is a screen a finger points at that is wider than a phone: upright it is as wide as a
// narrow window, and on its side as wide as a small desk. What makes a control large there is the
// finger, not the width.
const tablets = {
  upright: { viewport: { width: 820, height: 1180 }, hasTouch: true, isMobile: true, deviceScaleFactor: 2 },
  'on its side': { viewport: { width: 1180, height: 820 }, hasTouch: true, isMobile: true, deviceScaleFactor: 2 },
};
// The shortest side a control may have on a touch screen, in CSS pixels. Half a pixel is rounding.
const touchSize = 44 - 0.5;
// The smallest size a text may be drawn at, in CSS pixels. A hundredth of a pixel is rounding.
const smallestText = 12 - 0.01;
const wcag = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'];

let browser;
before(async () => { browser = await chromium.launch(); });
after(async () => { await browser?.close(); });

// open loads a page of the portal and collects what goes wrong while it is open. prepare runs on the
// page before it loads.
async function open(path, options, prepare) {
  const context = await browser.newContext(options);
  const page = await context.newPage();
  const problems = [];
  let watching = true;
  page.on('console', (msg) => { if (watching && msg.type() === 'error') problems.push(`console error: ${msg.text()}`); });
  page.on('pageerror', (err) => { if (watching) problems.push(`script error: ${err.message}`); });
  page.on('requestfailed', (req) => { if (watching) problems.push(`request failed: ${req.method()} ${req.url()} (${req.failure()?.errorText})`); });
  page.on('response', (res) => { if (watching && res.status() >= 400) problems.push(`${res.status()} for ${res.request().method()} ${res.url()}`); });
  if (prepare) await prepare(page);
  const response = await page.goto(base + path, { waitUntil: 'load' });
  return {
    page,
    response,
    problems,
    // Requests the page still has open (the stream of an output, a refresh) end with the page.
    close: async () => { watching = false; await context.close(); },
  };
}

// The functions below are sent to the page as text, so that one can use another.

// describeElement names an element in a report.
const describeElement = `(el) => {
  const text = (el.getAttribute('aria-label') || el.textContent || el.value || el.name || '').trim().replace(/\\s+/g, ' ').slice(0, 50);
  const id = el.id ? '#' + el.id : '';
  const href = el.getAttribute('href') ? ' -> ' + el.getAttribute('href') : '';
  return '<' + el.tagName.toLowerCase() + id + '> "' + text + '"' + href;
}`;

// isSeen reports whether a person sees an element.
const isSeen = `(el) => {
  const box = el.getBoundingClientRect();
  return box.width > 0 && box.height > 0 && el.checkVisibility({ visibilityProperty: true, contentVisibilityAuto: true });
}`;

// smallControls lists the controls under the elements that match a selector that are too small for
// a finger. A box that is checked, and a field inside its label, are touched through the label. A
// link inside a sentence is as tall as its line: the text around it is not a target.
const smallControls = (roots) => `(() => {
  const describe = ${describeElement};
  const seen = ${isSeen};
  const found = [];
  for (const root of document.querySelectorAll(${JSON.stringify(roots)})) {
    for (const el of root.querySelectorAll('a[href], button, input, select, textarea, summary, [role="button"], label.btn')) {
      if (el.type === 'hidden' || el.disabled) continue;
      const target = el.matches('input, select, textarea') ? (el.closest('label') || el) : el;
      if (!seen(target)) continue;
      if (el.matches('a') && getComputedStyle(el).display === 'inline' && el.parentElement.textContent.trim().length > el.textContent.trim().length) continue;
      const box = target.getBoundingClientRect();
      if (box.height < ${touchSize} || box.width < ${touchSize}) found.push(describe(el) + ' is ' + Math.round(box.width) + ' x ' + Math.round(box.height) + ' px');
    }
  }
  return found;
})()`;

// smallTexts lists the texts of the page that are drawn smaller than 12 px, each with its size and
// the element that holds it. It reads every text the page holds, shown now or not: what a details
// keeps closed, and what a narrower or a wider screen shows, is read by a person in its turn. What
// only a screen reader gets is not drawn at any size.
const smallTexts = `(() => {
  const describe = ${describeElement};
  const found = new Set();
  const note = (el, size, what) => { if (size < ${smallestText}) found.add(describe(el) + what + ' is ' + Math.round(size * 100) / 100 + ' px'); };
  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    const el = node.parentElement;
    if (!node.textContent.trim() || !el || el.closest('script, style, template, .sr-only')) continue;
    note(el, parseFloat(getComputedStyle(el).fontSize), '');
  }
  // The words a style sheet writes before or after an element (the tip of a tooltip, the number of
  // a step) are text too: what it writes between quotes, and what it counts.
  for (const el of document.body.querySelectorAll('*')) {
    for (const part of ['::before', '::after']) {
      const style = getComputedStyle(el, part);
      const written = (style.content.match(/"(?:[^"\\\\]|\\\\.)*"/g) || []).join('');
      if (/[\\p{L}\\p{N}]/u.test(written) || /\\bcounters?\\(/.test(style.content)) note(el, parseFloat(style.fontSize), ' ' + part);
    }
  }
  return [...found];
})()`;

// markControls numbers, in the page, every control a person can see and use, and returns their names.
// The radios of a group share a number: the keyboard stops on one of them and the arrows move inside.
async function markControls(page) {
  return page.evaluate(`(() => {
    const describe = ${describeElement};
    const seen = ${isSeen};
    const names = [];
    const groups = new Map();
    for (const el of document.querySelectorAll('a[href], button, input, select, textarea, summary, [tabindex]')) {
      if (!seen(el) || el.tabIndex < 0 || el.disabled || el.type === 'hidden' || el.closest('[inert]')) continue;
      if (el.type === 'radio' && el.name) {
        const key = (el.form ? el.form.id + ':' : ':') + el.name;
        if (groups.has(key)) { el.dataset.kb = groups.get(key); continue; }
        groups.set(key, String(names.length));
      }
      el.dataset.kb = String(names.length);
      names.push(describe(el));
    }
    return names;
  })()`);
}

// focusedControl says where the focus is: the number markControls gave the control, its name, and
// whether it shows the focus with an outline, its own or the one of the box that wraps it (the
// label of a field, the collapse of a summary).
const focusedControl = `(() => {
  const el = document.activeElement;
  if (!el || el === document.body || el === document.documentElement) return null;
  const clear = (color) => color === 'transparent' || /,\\s*0\\)$/.test(color) || /\\/\\s*0\\)$/.test(color);
  let ring = false;
  for (let box = el, up = 0; box && up < 3 && !ring; box = box.parentElement, up++) {
    const s = getComputedStyle(box);
    ring = s.outlineStyle !== 'none' && parseFloat(s.outlineWidth) > 0 && !clear(s.outlineColor);
  }
  return { kb: el.dataset.kb ?? null, what: (${describeElement})(el), ring };
})()`;

// axeViolations runs the accessibility check on the page, or on the elements that match a selector:
// the WCAG A and AA rules of axe-core, or only the rules named.
//
// axe does not decide the contrast of a text when what is behind it has an image, and daisyUI draws
// a texture on every button and badge; nor when the text is one character. Those are the texts on
// the strongest colors of a theme, so their ratio is worked out here, from the colors the browser
// draws and without the image (a texture moves a color by a few hundredths): the color of the text
// over the colors behind it, down to the first one that is opaque.
async function axeViolations(page, selector, rules) {
  if (!(await page.evaluate(() => Boolean(window.axe)))) await page.evaluate(axeSource);
  return page.evaluate(async ([tags, scope, only]) => {
    const runOnly = only ? { type: 'rule', values: only } : { type: 'tag', values: tags };
    // A color that is still changing is not the one a person reads: a button fades from one color to
    // the next in a fifth of a second, and a color read halfway is neither. What is in transition is
    // taken to its end before anything is measured, and what axe measured while something began to
    // change is measured again. (Once in some 1,800 pages a button inside the preview of Appearance,
    // which is a container the browser styles in two passes, was read 57% of the way from no color
    // to its own: 4.1 to 1, where it draws 8.3 to 1.)
    const settle = () => {
      const moving = document.getAnimations().filter((a) => a instanceof CSSTransition);
      moving.forEach((a) => a.finish());
      return moving.length;
    };
    settle();
    const measure = () => window.axe.run(scope || document, { runOnly, resultTypes: ['violations', 'incomplete'] });
    let res = await measure();
    if (settle() > 0) res = await measure();
    settle();
    const found = res.violations.map((v) => `${v.id}: ${v.help}\n` + v.nodes.map((n) => `    ${n.target.join(' ')}\n      ${n.failureSummary.replace(/\n\s*/g, ' ')}`).join('\n'));

    const canvas = document.createElement('canvas');
    canvas.width = canvas.height = 1;
    const pen = canvas.getContext('2d', { willReadFrequently: true });
    // rgba reads a color of a computed style, in whatever notation, as the browser draws it.
    const rgba = (css) => {
      if (!CSS.supports('color', css)) throw new Error(`a color the browser does not read: ${css}`);
      pen.clearRect(0, 0, 1, 1);
      pen.fillStyle = css;
      pen.fillRect(0, 0, 1, 1);
      return [...pen.getImageData(0, 0, 1, 1).data];
    };
    const over = (top, under) => [0, 1, 2].map((i) => (top[i] * top[3] + under[i] * (255 - top[3])) / 255);
    const luminance = (c) => { const f = (v) => (v / 255 <= 0.03928 ? v / 255 / 12.92 : ((v / 255 + 0.055) / 1.055) ** 2.4); return 0.2126 * f(c[0]) + 0.7152 * f(c[1]) + 0.0722 * f(c[2]); };
    const hex = (c) => '#' + c.map((v) => Math.round(v).toString(16).padStart(2, '0')).join('');
    const hard = [];
    for (const rule of res.incomplete) {
      if (rule.id !== 'color-contrast') continue;
      for (const node of rule.nodes) {
        const why = node.any[0]?.data?.messageKey;
        if (why !== 'bgImage' && why !== 'shortTextContent') continue;
        const el = document.querySelector(node.target[node.target.length - 1]);
        if (!el) continue;
        const layers = [];
        for (let box = el; box; box = box.parentElement) {
          const color = rgba(getComputedStyle(box).backgroundColor);
          if (color[3] > 0) layers.push(color);
          if (color[3] === 255) break;
        }
        const behind = layers.reduceRight((under, layer) => over(layer, under), [255, 255, 255]);
        const style = getComputedStyle(el);
        const ink = rgba(style.color);
        // A text with no color is not drawn: the words of the backdrop that closes a dialog are
        // there for a screen reader, and nobody reads them on the screen.
        if (ink[3] === 0) continue;
        const text = over(ink, behind);
        const [lighter, darker] = [luminance(text), luminance(behind)].sort((a, b) => b - a);
        const ratio = (lighter + 0.05) / (darker + 0.05);
        // Large text needs 3 to 1: 18 pt, or 14 pt in bold.
        const points = Math.ceil(parseFloat(style.fontSize) * 72) / 96;
        const needed = points >= 18 || (points >= 14 && Number(style.fontWeight) >= 700) ? 3 : 4.5;
        if (ratio < needed) hard.push(`    ${node.target.join(' ')}\n      draws ${hex(text)} on ${hex(behind)}: ${Math.floor(ratio * 100) / 100} to 1, and it needs ${needed} to 1`);
      }
    }
    if (hard.length) found.push(`color-contrast, worked out where axe does not decide: text that is hard to read\n${hard.join('\n')}`);
    return found;
  }, [wcag, selector, rules]);
}

// settled waits until the element that matches a selector, and what is in it, has the size and the
// colors it stays with. A dialog and a menu grow and fade in as they open: measured or checked
// before they finish, they are smaller and paler than a person sees them. Reading the style first
// makes the browser start a transition that is due, so one about to begin is waited for too.
// redrawn brings the page up to date after making one of its regions differ from what the server
// draws, so that the region is drawn anew for certain.
async function redrawn(page, id) {
  await page.evaluate((region) => {
    document.getElementById(region).insertAdjacentHTML('afterbegin', '<p data-stale-mark>stale</p>');
    return refreshRegions();
  }, id);
  assert.equal(await page.locator(`#${id} [data-stale-mark]`).count(), 0, `the region ${id} was not drawn anew`);
}

function settled(page, selector) {
  return page.waitForFunction((sel) => {
    const el = document.querySelector(sel);
    if (!el) return false;
    const style = getComputedStyle(el);
    if (style.opacity !== '1' || !['none', '1'].includes(style.scale)) return false;
    return el.getAnimations({ subtree: true }).every((a) => a.playState === 'finished');
  }, selector);
}

// visited gives a browser the list of the pages it opened lately, which the portal keeps in the
// browser and not on the server: the page of that list is then checked with rows in it, of today
// and of the day before, and with one tab for each section they are in.
const visited = (page) => page.addInitScript((list) => {
  try {
    if (!localStorage.getItem('portal.recent')) localStorage.setItem('portal.recent', JSON.stringify(list));
  } catch { /* a browser without storage keeps no list */ }
}, [
  { path: '/changes/add-billing', title: 'add-billing', at: new Date().toISOString() },
  { path: '/inbox', title: 'Needs you', at: new Date(Date.now() - 60_000).toISOString() },
  { path: '/agents', title: 'Agents', at: new Date(Date.now() - 120_000).toISOString() },
  { path: '/system/settings', title: 'Settings', at: new Date(Date.now() - 86_400_000).toISOString() },
  { path: '/backlog', title: 'Backlog', at: new Date(Date.now() - 86_460_000).toISOString() },
]);

for (const { name, path } of pages) {
  describe(`${name} (${path})`, () => {
    let view;
    before(async () => { view = await open(path, desktop, visited); });
    after(async () => { await view?.close(); });

    test('loads without an error', async () => {
      assert.equal(view.response.status(), 200);
      assert.ok((await view.page.title()).trim(), 'the page has no title');
      // A file the page renders brings the headings of its own document; they are not the page's.
      const headings = await view.page.evaluate(() => [...document.querySelectorAll('main h1')].filter((h) => !h.closest('[data-v="md"]')).length);
      assert.equal(headings, 1, 'the page does not have exactly one main heading');
      // A pattern the browser cannot compile is one it ignores: the field then takes anything, and
      // says so only in the console, once a person types in it.
      const patterns = await view.page.evaluate(() => [...document.querySelectorAll('[pattern]')].flatMap((el) => {
        try { new RegExp(`^(?:${el.getAttribute('pattern')})$`, 'v'); return []; } catch { return [`${el.name || el.id}: ${el.getAttribute('pattern')}`]; }
      }));
      assert.deepEqual(patterns, [], 'a field has a pattern the browser does not compile');
      assert.deepEqual(view.problems, []);
    });

    test('passes the accessibility check', async () => {
      const found = await axeViolations(view.page);
      assert.deepEqual(found, [], `axe found ${found.length} kind(s) of violation:\n${found.join('\n')}`);
    });

    test('is walked whole with the keyboard', async () => {
      const { page } = view;
      await page.evaluate(() => { document.activeElement?.blur(); window.scrollTo(0, 0); });
      const controls = await markControls(page);
      assert.ok(controls.length > 0, 'the page has no control to reach');
      const reached = new Set();
      const unseen = new Set();
      const unmarked = new Set();
      let last = '';
      // Twice around at most: a page may start with the focus in the middle of its order.
      for (let press = 0; press < 2 * controls.length + 10 && reached.size < controls.length; press++) {
        await page.keyboard.press('Tab');
        const at = await page.evaluate(focusedControl);
        if (at === null) { last = ''; continue; }
        assert.notEqual(at.what, last, `the focus is trapped on ${at.what}`);
        last = at.what;
        if (at.kb === null) { unseen.add(at.what); continue; }
        reached.add(Number(at.kb));
        if (!at.ring) unmarked.add(at.what);
      }
      const missed = controls.filter((_, i) => !reached.has(i));
      assert.deepEqual(missed, [], `the keyboard does not reach ${missed.length} of ${controls.length} control(s)`);
      assert.deepEqual([...unseen], [], 'the focus lands on something nobody sees');
      assert.deepEqual([...unmarked], [], 'a control with the focus does not show it');
    });

    test('draws no text smaller than 12 px', async () => {
      const found = await view.page.evaluate(smallTexts);
      assert.deepEqual(found, [], `${found.length} text(s) are drawn smaller than 12 px`);
    });

    test('fits a tablet, upright and on its side', async () => {
      for (const [how, device] of Object.entries(tablets)) {
        const wide = await open(path, device, visited);
        try {
          const overflow = await wide.page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
          assert.ok(overflow <= 0, `the page is ${overflow} px wider than a tablet ${how}`);
          const found = await wide.page.evaluate(smallControls('body'));
          assert.deepEqual(found, [], `${found.length} control(s) are smaller than 44 px on a tablet ${how}`);
          assert.deepEqual(wide.problems, []);
        } finally {
          await wide.close();
        }
      }
    });

    test('fits a phone', async () => {
      const small = await open(path, phone, visited);
      try {
        const overflow = await small.page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
        assert.ok(overflow <= 0, `the page is ${overflow} px wider than the screen`);
        const found = await small.page.evaluate(smallControls('body'));
        assert.deepEqual(found, [], `${found.length} control(s) are smaller than 44 px`);
        const tiny = await small.page.evaluate(smallTexts);
        assert.deepEqual(tiny, [], `${tiny.length} text(s) are drawn smaller than 12 px on a phone`);
        assert.deepEqual(small.problems, []);
      } finally {
        await small.close();
      }
    });
  });
}

describe('what every page has, on a phone', () => {
  let view;
  before(async () => { view = await open(pages[0].path, phone); });
  after(async () => { await view?.close(); });

  test('the navigation opens with controls a finger can use, and closes', async () => {
    const { page } = view;
    const button = page.locator('label[for="nav-drawer"][role="button"]');
    await button.click();
    await page.locator('#nav-menu a').first().waitFor({ state: 'visible' });
    assert.equal(await button.getAttribute('aria-expanded'), 'true');
    const found = await page.evaluate(smallControls('aside'));
    assert.deepEqual(found, [], `${found.length} control(s) of the navigation are smaller than 44 px`);
    await page.keyboard.press('Escape');
    await page.locator('#nav-menu a').first().waitFor({ state: 'hidden' });
    assert.equal(await button.getAttribute('aria-expanded'), 'false');
  });

  test('a dialog has controls a finger can use', async () => {
    const { page } = view;
    await page.locator('[data-open-dialog="pause-dialog"]').click();
    try {
      await page.locator('#pause-dialog[open]').waitFor({ state: 'visible' });
      // The dialog fades in and its box grows: both are checked as they stay.
      await settled(page, '#pause-dialog');
      await settled(page, '#pause-dialog .modal-box');
      const found = await page.evaluate(smallControls('#pause-dialog .modal-box'));
      assert.deepEqual(found, [], `${found.length} control(s) of the dialog are smaller than 44 px`);
      const flaws = await axeViolations(page, '#pause-dialog');
      assert.deepEqual(flaws, [], `axe found ${flaws.length} kind(s) of violation in the dialog:\n${flaws.join('\n')}`);
      await page.locator('#pause-dialog [data-close-dialog]').click();
      await page.locator('#pause-dialog').waitFor({ state: 'hidden' });
    } finally {
      // A dialog left open would cover the page for the tests that follow.
      await page.evaluate(() => document.querySelector('#pause-dialog')?.close());
    }
  });

  test('the menu of themes has controls a finger can use', async () => {
    const { page } = view;
    await page.locator('[popovertarget="theme-menu"]').click();
    await page.locator('#theme-menu').waitFor({ state: 'visible' });
    // The menu grows to its size as it opens: it is measured once it has it.
    await settled(page, '#theme-menu');
    const found = await page.evaluate(smallControls('#theme-menu'));
    assert.deepEqual(found, [], `${found.length} control(s) of the menu of themes are smaller than 44 px`);
    await page.keyboard.press('Escape');
    await page.locator('#theme-menu').waitFor({ state: 'hidden' });
  });
});

describe('the error of an action', () => {
  const refusal = { code: 'conflict', message: 'The portal refused it, and says why here.' };

  test('stays next to its control until a person dismisses it', async () => {
    // The clock of the page is ours: whatever a timer would do in ten minutes happens in the test.
    const view = await open(actionPage, desktop, (page) => page.clock.install());
    try {
      const { page } = view;
      const control = page.locator('main button[data-post]:not([data-confirm])').first();
      const action = await control.getAttribute('data-post');
      await page.route(`**${action}`, (route) => route.fulfill({ status: 409, json: { error: refusal } }));
      await control.click();
      const box = page.locator('[data-action-error]');
      await box.waitFor({ state: 'visible' });
      assert.match(await box.textContent(), /The portal refused it, and says why here\./);
      assert.equal(await box.getAttribute('role'), 'alert');
      assert.equal(await control.isDisabled(), false, 'the control stays disabled after its action failed');

      await page.clock.fastForward('10:00');
      assert.equal(await box.isVisible(), true, 'the error went away by itself');
      assert.equal(page.url(), base + actionPage, 'the page left after an action that failed');
      const flaws = await axeViolations(page, '[data-action-error]');
      assert.deepEqual(flaws, [], `axe found ${flaws.length} kind(s) of violation in the error:\n${flaws.join('\n')}`);

      await box.locator('[data-dismiss-error]').click();
      assert.equal(await box.count(), 0, 'the error is still there after it was dismissed');
    } finally {
      await view.close();
    }
  });

  test('stays when the page brings its regions up to date', async () => {
    const view = await open(livePage, desktop);
    try {
      const { page } = view;
      // An error next to a control inside a region the page refreshes, and a region that is stale.
      const placed = await page.evaluate(([code, message]) => {
        const control = document.querySelector('main [data-live-region][id] a.btn, main [data-live-region][id] button');
        if (!control) return null;
        const region = control.closest('[data-live-region][id]');
        showError(control, Object.assign(new Error(message), { code }));
        if (!region.querySelector('[data-action-error]')) return 'outside';
        region.insertAdjacentHTML('afterbegin', '<p data-stale-mark>stale</p>');
        return region.id;
      }, [refusal.code, refusal.message]);
      assert.ok(placed && placed !== 'outside', `the page has no control inside a region it refreshes (${placed})`);
      await page.evaluate(() => refreshRegions());
      assert.equal(await page.locator('[data-stale-mark]').count(), 0, 'the region was not brought up to date');
      const box = page.locator(`#${placed} [data-action-error]`);
      assert.equal(await box.count(), 1, 'the error left with the refresh of its region');
      assert.match(await box.textContent(), /The portal refused it, and says why here\./);
      await box.locator('[data-dismiss-error]').click();
      assert.equal(await box.count(), 0, 'the error is still there after it was dismissed');
    } finally {
      await view.close();
    }
  });
});

describe('what an action answers', () => {
  test('is shown in a dialog a person closes, on a phone too', async () => {
    const view = await open(reportPage, phone);
    try {
      const { page } = view;
      await page.locator('main button[data-post][data-title]:not([data-confirm])').first().click();
      const dialog = page.locator('#output-dialog[open]');
      await dialog.waitFor({ state: 'visible' });
      await settled(page, '#output-dialog');
      await settled(page, '#output-dialog .modal-box');
      const text = await dialog.locator('[data-output-body]').textContent();
      assert.ok(text.trim().length > 0 && text.trim() !== '(no output)', `the dialog shows nothing of what the action answered: ${JSON.stringify(text)}`);
      const found = await page.evaluate(smallControls('#output-dialog .modal-box'));
      assert.deepEqual(found, [], `${found.length} control(s) of the dialog are smaller than 44 px`);
      const flaws = await axeViolations(page, '#output-dialog');
      assert.deepEqual(flaws, [], `axe found ${flaws.length} kind(s) of violation in the dialog:\n${flaws.join('\n')}`);
      assert.deepEqual(view.problems, []);
    } finally {
      await view.close();
    }
  });
});

describe('the board of changes', () => {
  // With cards in its five lanes of work and the two at the ends folded, the board asks for
  // 5 lanes of 188 px, 2 strips of 48 px and 6 gaps of 8 px: what a window 1440 px wide leaves it.
  test('fits a window 1440 px wide, and no lane holds something wider than it', async () => {
    for (const path of [boardPage, `${boardPage}?open=discarded,integrated`]) {
      const view = await open(path, { viewport: { width: 1440, height: 900 } });
      try {
        const got = await view.page.evaluate(() => {
          const board = document.getElementById('board');
          const lanes = [...board.querySelectorAll('[data-lane]')];
          return {
            client: board.clientWidth,
            scroll: board.scrollWidth,
            page: document.documentElement.scrollWidth - document.documentElement.clientWidth,
            withCards: lanes.filter((lane) => lane.querySelector('[data-card]')).length,
            wide: lanes.filter((lane) => lane.scrollWidth > lane.clientWidth).map((lane) => `${lane.dataset.lane} (${lane.scrollWidth} px in ${lane.clientWidth})`),
          };
        });
        assert.deepEqual(got.wide, [], `${path}: what a lane holds is wider than the lane`);
        assert.equal(got.page, 0, `${path}: the page is ${got.page} px wider than the window`);
        if (path === boardPage) {
          assert.equal(got.withCards, 5, 'the board of the test does not have cards in its five lanes of work, and measures nothing');
          assert.ok(got.scroll <= got.client, `the board asks for ${got.scroll} px and the window leaves it ${got.client}`);
        }
      } finally {
        await view.close();
      }
    }
  });

  // The card of a change that waits in the backlog: a person can abandon it, and nothing else.
  const card = '[data-card="add-ui"]';
  const abandon = '**/api/changes/add-ui/abandon';

  test('a card dropped on a lane presses the button that moves it there, which asks first', async () => {
    const view = await open(boardPage, desktop);
    try {
      const { page } = view;
      // The portal is not asked to abandon anything: the request is answered here, and counted.
      const asked = [];
      await page.route(abandon, (route) => { asked.push(route.request().method()); return route.fulfill({ status: 200, json: { state: 'discarded' } }); });

      // While the card is dragged, the lanes say which of them takes it.
      const grip = { x: 12, y: 8 };
      await page.locator(card).hover({ position: grip });
      await page.mouse.down();
      await page.locator('#lane-building').hover();
      await page.locator('#lane-building').hover();
      assert.equal(await page.locator('#lane-discarded').getAttribute('data-drop'), 'yes', 'the lane a card can go to does not say so while the card is dragged');
      assert.equal(await page.locator('#lane-building').getAttribute('data-drop'), 'no', 'a lane only the kit puts cards in takes a card');
      assert.equal(await page.locator('#lane-backlog').getAttribute('data-drop'), 'no', 'the lane a card is in takes it again');
      assert.equal(await page.evaluate(() => liveBusy()), true, 'the page would refresh under a card that is being dragged');
      // Dropped on a lane that does not take it, the card stays, and nothing is asked.
      await page.mouse.up();
      assert.equal(await page.locator('#lane-building').getAttribute('data-drop'), null, 'the lanes still say where a card can go after it was dropped');
      assert.equal(await page.locator('#confirm-dialog[open]').count(), 0, 'a card dropped where it cannot go asks to confirm something');
      assert.deepEqual(asked, []);

      // Dropped on Discarded, it asks before it abandons the change: that move is not undone.
      await page.locator(card).dragTo(page.locator('#lane-discarded'), { sourcePosition: grip });
      const dialog = page.locator('#confirm-dialog[open]');
      await dialog.waitFor({ state: 'visible' });
      assert.match(await dialog.locator('[data-confirm-title]').textContent(), /Abandon this change\?/);
      assert.deepEqual(asked, [], 'the card was moved before the person confirmed');
      await dialog.locator('button[value="cancel"], [data-confirm-cancel]').first().click();
      await page.locator('#confirm-dialog').waitFor({ state: 'hidden' });
      assert.deepEqual(asked, [], 'a move that was not confirmed was made');

      await page.locator(card).dragTo(page.locator('#lane-discarded'), { sourcePosition: grip });
      await dialog.waitFor({ state: 'visible' });
      await dialog.locator('[data-confirm-ok]').click();
      await page.locator('#toasts .alert').first().waitFor({ state: 'visible' });
      assert.deepEqual(asked, ['POST'], 'the drop did not post the action of the button, once');
      assert.equal(await page.evaluate(() => liveBusy()), false, 'the page thinks a card is still being dragged');
      assert.deepEqual(view.problems, []);
    } finally {
      await view.close();
    }
  });

  test('the tasks a person opened or closed stay that way when the board is brought up to date', async () => {
    const view = await open(boardPage, desktop);
    try {
      const { page } = view;
      // The tasks of a change a worker builds start open, and the others closed.
      const building = page.locator('#tasks-add-search');
      const blocked = page.locator('#tasks-add-billing');
      assert.equal(await building.evaluate((d) => d.open), true);
      assert.equal(await blocked.evaluate((d) => d.open), false);
      await building.locator('summary').click();
      await blocked.locator('summary').click();
      assert.equal(await blocked.locator('li').first().isVisible(), true, 'the tasks of a card do not show when they are opened');

      await page.evaluate(() => { document.querySelector('#board').insertAdjacentHTML('afterbegin', '<p data-stale-mark>stale</p>'); return refreshRegions(); });
      assert.equal(await page.locator('[data-stale-mark]').count(), 0, 'the board was not brought up to date');
      assert.equal(await building.evaluate((d) => d.open), false, 'the tasks a person closed were opened again by the refresh');
      assert.equal(await blocked.evaluate((d) => d.open), true, 'the tasks a person opened were closed by the refresh');
      const bar = blocked.locator('progress');
      assert.deepEqual([await bar.getAttribute('value'), await bar.getAttribute('max')], ['2', '5']);
      assert.deepEqual(view.problems, []);
    } finally {
      await view.close();
    }
  });

  test('a move the portal refuses says so on its card, and stays there when the board is brought up to date', async () => {
    const view = await open(boardPage, desktop);
    try {
      const { page } = view;
      // The card of a change whose worker is gone: giving its lease back asks nothing first.
      const lost = page.locator('[data-card="add-export"]');
      await page.route('**/api/changes/add-export/reclaim', (route) => route.fulfill({ status: 409, json: { error: { code: 'conflict', message: 'Its worker is alive after all.' } } }));
      await lost.locator('button[data-move-to="backlog"]').click();
      const box = lost.locator('[data-action-error]');
      await box.waitFor({ state: 'visible' });
      assert.match(await box.textContent(), /Its worker is alive after all\./);
      await redrawn(page, 'board');
      assert.equal(await page.locator('[data-card="add-export"] [data-action-error]').count(), 1, 'the error left its card with the refresh of the board');
      assert.equal(await page.locator('[data-action-error]').count(), 1);
      await page.locator('[data-card="add-export"] [data-dismiss-error]').click();
      assert.equal(await page.locator('[data-action-error]').count(), 0, 'the error is still there after it was dismissed');
    } finally {
      await view.close();
    }
  });

  test('an error stays on its own card when a change and a proposal have the same name', async () => {
    const view = await open(`${boardPage}?open=discarded`, desktop);
    try {
      const { page } = view;
      const change = page.locator('[data-kind="change"][data-card="add-orphan"]');
      const proposal = page.locator('[data-kind="proposal"][data-card="add-orphan"]');
      assert.equal(await change.count(), 1, 'the board has no card of the change add-orphan');
      assert.equal(await proposal.count(), 1, 'the board has no card of the proposal add-orphan');
      await page.route('**/api/proposals/add-orphan/approve', (route) => route.fulfill({ status: 409, json: { error: { code: 'conflict', message: 'It was decided a moment ago.' } } }));
      await proposal.locator('button[data-move-to="backlog"]').click();
      await proposal.locator('[data-action-error]').waitFor({ state: 'visible' });
      await redrawn(page, 'board');
      assert.equal(await proposal.locator('[data-action-error]').count(), 1, 'the error left the card of the proposal');
      assert.equal(await change.locator('[data-action-error]').count(), 0, 'the error went to the card of the change of the same name');
    } finally {
      await view.close();
    }
  });

  test('the focus stays on its control when the board is brought up to date', async () => {
    const view = await open(boardPage, desktop);
    try {
      const { page } = view;
      const button = page.locator(`${card} button[data-move-to="discarded"]`);
      await button.focus();
      await page.evaluate(() => { document.querySelector('#board').insertAdjacentHTML('afterbegin', '<p data-stale-mark>stale</p>'); return refreshRegions(); });
      assert.equal(await page.locator('[data-stale-mark]').count(), 0, 'the board was not brought up to date');
      const focused = await page.evaluate(() => [document.activeElement.dataset.post, document.activeElement.closest('[data-card]')?.dataset.card]);
      assert.deepEqual(focused, ['/api/changes/add-ui/abandon', 'add-ui'], 'the refresh of the board took the focus from the button a person was on');
      // The tasks of a card are opened with the keyboard, and the focus stays there too.
      await page.locator('#tasks-add-billing summary').focus();
      await page.keyboard.press('Enter');
      await page.evaluate(() => refreshRegions());
      assert.equal(await page.evaluate(() => document.activeElement.matches('#tasks-add-billing > summary')), true, 'the refresh took the focus from the tasks a person opened');
      assert.equal(await page.locator('#tasks-add-billing').evaluate((d) => d.open), true);
      assert.deepEqual(view.problems, []);
    } finally {
      await view.close();
    }
  });

  test('on a phone a lane fills the screen, and its cards are moved with their buttons', async () => {
    const view = await open(boardPage, phone);
    try {
      const { page } = view;
      const width = await page.evaluate(() => document.querySelector('#board').clientWidth);
      const lane = await page.locator('#lane-backlog').boundingBox();
      assert.ok(Math.abs(lane.width - width) < 2, `a lane is ${lane.width} px wide on a phone, and the board ${width} px`);
      assert.equal(await page.locator('#lane-discarded').isVisible(), false, 'a folded lane takes room beside the lanes on a phone');
      // The list of lanes leads to each of them, and opens a folded one.
      assert.match(await page.locator('#board-lanes a', { hasText: 'Discarded' }).getAttribute('href'), /open=discarded#lane-discarded$/);
      const button = page.locator(`${card} button[data-move-to="discarded"]`);
      const box = await button.boundingBox();
      assert.ok(box.height >= touchSize && box.width >= touchSize, `the button that moves a card is ${box.width} x ${box.height} px on a phone`);
    } finally {
      await view.close();
    }
  });
});

// A page that keeps itself up to date draws a region anew when what the server draws in it changed,
// and only then; and what a person did in the region is theirs: it is drawn anew around it.
describe('a region the page keeps up to date', () => {
  test('keeps open what a person opened, with the focus on it, and closed what they closed', async () => {
    const view = await open('/inbox', desktop);
    try {
      const { page } = view;
      const first = () => page.locator('#inbox-rows details:not([data-clamp])').first();
      assert.ok(await first().count() > 0, 'the portal under test has nothing to open among what waits for a person');
      await first().locator('summary').focus();
      await page.keyboard.press('Enter');
      assert.equal(await first().evaluate((d) => d.open), true, 'Enter on the summary does not open it');
      await redrawn(page, 'inbox-rows');
      assert.equal(await first().evaluate((d) => d.open), true, 'the page closed what the person had opened');
      assert.equal(await first().locator('summary').evaluate((s) => s === document.activeElement), true, 'the focus left the summary the person was on');
      await page.keyboard.press('Enter');
      assert.equal(await first().evaluate((d) => d.open), false, 'Enter on the summary does not close it');
      await redrawn(page, 'inbox-rows');
      assert.equal(await first().evaluate((d) => d.open), false, 'the page opened what the person had closed');
      assert.deepEqual(view.problems, []);
    } finally {
      await view.close();
    }
  });

  test('gives no control to a text that fits, however many times it is drawn', async () => {
    const view = await open('/inbox', desktop);
    try {
      const { page } = view;
      // A text of which no line is cut is a paragraph: the control that would open it does nothing.
      const idle = () => page.evaluate(() => [...document.querySelectorAll('#inbox-rows details[data-clamp]')].filter((d) => {
        const text = d.querySelector('[data-clamp-text]');
        return !d.open && text.scrollHeight <= text.clientHeight + 1;
      }).length);
      assert.equal(await idle(), 0, 'a text that fits has a control when the page loads');
      await redrawn(page, 'inbox-rows');
      assert.equal(await idle(), 0, 'a text that fits has a control once the page brought itself up to date');
    } finally {
      await view.close();
    }
  });

  test('is left alone when what the server draws in it did not change', async () => {
    const view = await open('/agents', desktop);
    try {
      const { page } = view;
      // A time that goes by changes what the server draws: such a try says nothing, and is made again.
      for (let attempt = 0; attempt < 5; attempt++) {
        const result = await page.evaluate(async () => {
          const region = document.getElementById('agents-list');
          const before = region.innerHTML;
          region.querySelector('tbody tr').kept = true;
          await refreshRegions();
          return { same: region.innerHTML === before, kept: region.querySelector('tbody tr').kept === true };
        });
        if (!result.same) continue;
        assert.ok(result.kept, 'the page drew anew a region the server drew the same');
        return;
      }
      assert.fail('what the server draws of the agents changed on every try');
    } finally {
      await view.close();
    }
  });
});

// The three questions a person comes to the portal with are answered within two clicks of its
// first page: what runs now, what waits for me, and why this change stopped. A click here is a
// click on something the page shows, the way a person does it: no address is typed. What the
// portal has to answer is read from its API.
describe('the three questions of an operator', () => {
  let view;
  let clicks;
  const read = async (path) => (await fetch(base + path)).json();
  before(async () => { view = await open('/', desktop); });
  after(async () => { await view?.close(); });
  const fromTheFirstPage = async () => {
    clicks = 0;
    await view.page.goto(base + '/', { waitUntil: 'load' });
  };
  // follow clicks a link of the page and waits for the page it leads to.
  const follow = async (selector) => {
    const { page } = view;
    const link = page.locator(selector).first();
    await link.waitFor({ state: 'visible' });
    const href = await link.getAttribute('href');
    await Promise.all([page.waitForURL((url) => url.pathname + url.search === href), link.click()]);
    clicks++;
  };

  test('what runs now is on the first page', async () => {
    const now = await read('/api/dashboard');
    assert.ok(now.runs.length > 0, 'the portal under test has nothing running');
    await fromTheFirstPage();
    for (const run of now.runs) {
      const named = view.page.locator(`#dashboard-running a[href="/changes/${run.change_name}"]`).first();
      assert.ok(await named.isVisible(), `the first page does not name ${run.change_name}, which runs now`);
    }
    assert.equal(clicks, 0);
  });

  test('what waits for me is one click away', async () => {
    const inbox = await read('/api/inbox');
    assert.ok(inbox.count > 0, 'the portal under test has nothing that waits for a person');
    await fromTheFirstPage();
    await follow('#dashboard-waiting a[href="/inbox"]');
    assert.equal(clicks, 1);
    assert.equal(await view.page.locator('#inbox-rows li.list-row').count(), inbox.count, 'the page does not list as many things as wait for a person');
    // Each thing that waits is named in the section of its kind.
    const kinds = {
      blocked: ['inbox-blocked', (x) => x.change_name],
      questions: ['inbox-questions', (x) => x.question],
      broken_bases: ['inbox-bases', (x) => x.repo],
      lost_runs: ['inbox-lost', (x) => x.change_name],
      proposals: ['inbox-proposals', (x) => x.id],
    };
    for (const [kind, [heading, name]] of Object.entries(kinds)) {
      if (inbox[kind].length === 0) continue;
      const section = await view.page.locator(`section[aria-labelledby="${heading}"]`).innerText();
      for (const thing of inbox[kind]) assert.ok(section.includes(name(thing)), `the page does not name ${name(thing)} among the ${kind}`);
    }
  });

  test('why a change stopped is two clicks away', async () => {
    const [stopped] = (await read('/api/inbox')).blocked;
    assert.ok(stopped?.details, 'the portal under test has no blocked change that recorded why');
    await fromTheFirstPage();
    await follow('#dashboard-waiting a[href="/inbox"]');
    const row = view.page.locator('section[aria-labelledby="inbox-blocked"] li.list-row', { has: view.page.locator(`a[href="/changes/${stopped.change_name}"]`) });
    // One click in, the row says in words what stopped the change.
    const reason = (await row.locator('.badge').first().innerText()).trim();
    assert.ok(reason.length > 0, 'the row of the blocked change does not say what stopped it');
    // The second click opens what the kit recorded when it blocked it.
    await row.locator('summary').click();
    clicks++;
    const why = row.locator('details pre');
    await why.waitFor({ state: 'visible' });
    assert.equal((await why.innerText()).trim(), stopped.details);
    assert.equal(clicks, 2);
  });

  test('the page of a change that stopped is two clicks away, and says why at its top', async () => {
    const [stopped] = (await read('/api/inbox')).blocked;
    assert.ok(stopped, 'the portal under test has no blocked change');
    await fromTheFirstPage();
    await follow('#dashboard-waiting a[href="/inbox"]');
    const reason = (await view.page.locator('section[aria-labelledby="inbox-blocked"] li.list-row .badge').first().innerText()).trim();
    await follow(`section[aria-labelledby="inbox-blocked"] a[href="/changes/${stopped.change_name}"]`);
    assert.equal(clicks, 2);
    const top = await view.page.locator('#change-stopped').innerText();
    assert.ok(top.includes(reason), `the page of ${stopped.change_name} does not say "${reason}" where it says it waits`);
  });
});

// The check of contrast measures what a person reads, not a color on its way to another.
describe('the check of contrast', () => {
  test('reads a color that is changing as the color it ends at', async () => {
    const view = await open(actionPage, desktop);
    try {
      const { page } = view;
      const moving = await page.evaluate(() => {
        const button = document.querySelector('main .btn-primary');
        if (!button) return -1;
        // The button has no color, and then fades in to its own, slowly enough to be still at it
        // when it is measured: until it ends, its words are nearly the color of what is behind them.
        button.style.transition = 'none';
        button.style.backgroundColor = 'transparent';
        void getComputedStyle(button).backgroundColor;
        button.style.transition = 'background-color 60s linear';
        button.style.backgroundColor = '';
        return document.getAnimations().filter((a) => a instanceof CSSTransition && a.effect.target === button).length;
      });
      assert.notEqual(moving, -1, 'the page under test has no primary button');
      assert.ok(moving > 0, 'the button did not start to change');
      const found = await axeViolations(page, 'main', ['color-contrast']);
      assert.deepEqual(found, [], 'a color was read while it changed');
    } finally {
      await view.close();
    }
  });
});

// Every theme the menu offers draws its text so that it is read: 4.5 to 1 against what is behind it,
// and 3 to 1 for large text (WCAG AA). The check of each page above runs in the theme of the kit;
// here every theme of the menu draws every one of those pages, and the three kinds of message the
// portal shows over a page. A theme that fails is not one to excuse: it leaves the menu
// (portalThemes, and input.css with it).
describe('the themes the menu offers', () => {
  test('are a light one and a dark one at least, and each draws every page so that it is read', async (t) => {
    const menu = await open(pages[0].path, desktop);
    let themes;
    try {
      themes = await menu.page.evaluate(() => [...document.querySelectorAll('#theme-menu input[name=theme]')].map((el) => el.value));
    } finally {
      await menu.close();
    }
    assert.ok(themes.length > 0, 'the menu offers no theme');
    const schemes = new Set();
    for (const theme of themes) {
      await t.test(theme, async () => {
        const context = await browser.newContext(desktop);
        try {
          await context.addCookies([{ name: 'portal_theme', value: theme, url: base }]);
          const page = await context.newPage();
          const found = [];
          for (const { path } of pages) {
            await page.goto(base + path, { waitUntil: 'load' });
            assert.equal(await page.evaluate(() => document.documentElement.dataset.theme), theme, `${path} is not drawn in the theme ${theme}`);
            if (path === pages[0].path) {
              schemes.add(await page.evaluate(() => getComputedStyle(document.documentElement).colorScheme));
              // The messages stay for seconds: they are checked with the page they are shown over.
              await page.evaluate(() => { toast('Done as asked', 'success'); toast('The portal refused it', 'error'); toast('Nothing runs any more', 'info'); });
              assert.equal(await page.locator('#toasts .alert').count(), 3, 'the three kinds of message are not shown');
            }
            const flaws = await axeViolations(page, undefined, ['color-contrast']);
            found.push(...flaws.map((flaw) => `${path}\n${flaw}`));
          }
          assert.deepEqual(found, [], `the theme ${theme} draws text that is hard to read on ${found.length} page(s)`);
        } finally {
          await context.close();
        }
      });
    }
    assert.ok(schemes.has('light') && schemes.has('dark'), `the themes of the menu are ${[...schemes].join(' and ') || 'none'}: it offers a light one and a dark one at least`);
  });
});
