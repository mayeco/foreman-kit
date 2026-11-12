'use strict';

// Tests of the launcher that need no network: the checksums line it picks, the binary it prefers,
// and the cache it keeps per version. Run with: node --test npm/test/

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');

const launcher = require('../bin/foreman-kit.js');
const { version } = require('../package.json');

const SUM = 'a'.repeat(64);

test('findAsset picks the raw binary of the platform', () => {
  const checksums = [
    `${'b'.repeat(64)}  foreman-kit-linux-amd64`,
    `${SUM}  foreman-kit-darwin-arm64`,
    `${'c'.repeat(64)}  foreman-kit-darwin-arm64.tar.gz`,
    '',
  ].join('\n');
  assert.deepEqual(launcher.findAsset(checksums, 'darwin', 'arm64'), {
    name: 'foreman-kit-darwin-arm64',
    sha256: SUM,
  });
});

test('findAsset accepts the binary-mode marker of sha256sum', () => {
  const asset = launcher.findAsset(`${SUM} *foreman-kit-linux-arm64\n`, 'linux', 'arm64');
  assert.equal(asset.name, 'foreman-kit-linux-arm64');
});

test('findAsset says which binary the release lacks', () => {
  assert.throws(
    () => launcher.findAsset(`${SUM}  foreman-kit-linux-amd64\n`, 'darwin', 'amd64'),
    /has no foreman-kit-darwin-amd64/,
  );
});

test('the cache is per version and follows XDG_CACHE_HOME', () => {
  const saved = process.env.XDG_CACHE_HOME;
  try {
    process.env.XDG_CACHE_HOME = '/tmp/xdg';
    assert.equal(launcher.cacheDir(), path.join('/tmp/xdg', 'foreman-kit', version));
    delete process.env.XDG_CACHE_HOME;
    assert.equal(launcher.cacheDir(), path.join(os.homedir(), '.cache', 'foreman-kit', version));
  } finally {
    if (saved === undefined) delete process.env.XDG_CACHE_HOME;
    else process.env.XDG_CACHE_HOME = saved;
  }
});

test('ensureBinary prefers a cached binary to a download', async (t) => {
  const { goos, goarch } = launcher.platform();
  if (launcher.bundled(goos, goarch)) {
    t.skip('this checkout carries a binary for the platform');
    return;
  }
  const saved = process.env.XDG_CACHE_HOME;
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'foreman-kit-npm-'));
  try {
    process.env.XDG_CACHE_HOME = dir;
    const cached = path.join(dir, 'foreman-kit', version, 'foreman-kit');
    fs.mkdirSync(path.dirname(cached), { recursive: true });
    fs.writeFileSync(cached, '#!/bin/sh\n', { mode: 0o755 });
    assert.equal(await launcher.ensureBinary(), cached);
  } finally {
    if (saved === undefined) delete process.env.XDG_CACHE_HOME;
    else process.env.XDG_CACHE_HOME = saved;
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

// Serves checksums.txt and the binary of a fake release in place of GitHub, through a redirect as
// GitHub does, and returns the URLs asked.
function fakeRelease(t, body, sha256) {
  const https = require('node:https');
  const { PassThrough } = require('node:stream');
  const asked = [];
  t.mock.method(https, 'get', (url, _opts, cb) => {
    asked.push(url);
    const req = new PassThrough();
    req.setTimeout = () => req;
    const res = new PassThrough();
    process.nextTick(() => {
      const { goos, goarch } = launcher.platform();
      if (url.endsWith('/checksums.txt')) {
        res.statusCode = 200;
        cb(res);
        res.end(`${sha256}  foreman-kit-${goos}-${goarch}\n`);
      } else if (url.includes('/releases/download/')) {
        res.statusCode = 302;
        res.headers = { location: 'https://objects.example/blob' };
        cb(res);
        res.end();
      } else {
        res.statusCode = 200;
        cb(res);
        res.end(body);
      }
    });
    return req;
  });
  return asked;
}

async function withEmptyCache(t, fn) {
  const { goos, goarch } = launcher.platform();
  if (launcher.bundled(goos, goarch)) {
    t.skip('this checkout carries a binary for the platform');
    return;
  }
  const saved = process.env.XDG_CACHE_HOME;
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'foreman-kit-npm-'));
  const stderr = t.mock.method(process.stderr, 'write', () => true);
  try {
    process.env.XDG_CACHE_HOME = dir;
    await fn(dir);
  } finally {
    stderr.mock.restore();
    if (saved === undefined) delete process.env.XDG_CACHE_HOME;
    else process.env.XDG_CACHE_HOME = saved;
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

test('ensureBinary downloads the binary of the release, checks it and caches it', async (t) => {
  const body = Buffer.from('#!/bin/sh\necho fake\n');
  const sha = require('node:crypto').createHash('sha256').update(body).digest('hex');
  await withEmptyCache(t, async (dir) => {
    const asked = fakeRelease(t, body, sha);
    const got = await launcher.ensureBinary();
    assert.equal(got, path.join(dir, 'foreman-kit', version, 'foreman-kit'));
    assert.deepEqual(fs.readFileSync(got), body);
    assert.ok(fs.statSync(got).mode & 0o100, 'the cached binary is executable');
    assert.ok(asked[0].endsWith(`/mayeco/foreman-kit/releases/download/v${version}/checksums.txt`));
    assert.equal(fs.readdirSync(path.dirname(got)).length, 1, 'no temporary file is left');
  });
});

test('ensureBinary refuses a binary whose checksum does not match', async (t) => {
  await withEmptyCache(t, async (dir) => {
    fakeRelease(t, Buffer.from('tampered'), 'd'.repeat(64));
    await assert.rejects(launcher.ensureBinary(), /checksum mismatch/);
    assert.equal(fs.existsSync(path.join(dir, 'foreman-kit', version, 'foreman-kit')), false);
  });
});
