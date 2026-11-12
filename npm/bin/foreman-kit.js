#!/usr/bin/env node
'use strict';

// Launcher of foreman-kit. It runs, in this order:
//
//   1. the binary this package carries for the platform (vendor/<os>-<arch>/foreman-kit), the one
//      scripts/npm-pack.sh puts in it;
//   2. the one downloaded before for this version (~/.cache/foreman-kit/<version>/foreman-kit,
//      $XDG_CACHE_HOME when it is set);
//   3. otherwise it downloads foreman-kit-<os>-<arch> from the GitHub release v<version>, checks
//      its SHA-256 against the release's checksums.txt, keeps it in that cache and runs it.
//
// The version is the one of this package, which the release workflow stamps from the tag: a new
// release of the package runs, and downloads if it must, the binary of that same release. The
// arguments, the standard streams, the exit code and the signals that stop it pass through
// unchanged. Node built-ins only.

const { spawn } = require('node:child_process');
const crypto = require('node:crypto');
const fs = require('node:fs');
const https = require('node:https');
const os = require('node:os');
const path = require('node:path');

const REPO = 'mayeco/foreman-kit';
const BIN = 'foreman-kit';
const { version } = require('../package.json');

const GOOS = { darwin: 'darwin', linux: 'linux' };
const GOARCH = { x64: 'amd64', arm64: 'arm64' };

// How long a download may go without receiving a byte before it is abandoned.
const IDLE_TIMEOUT_MS = 60_000;

function platform() {
  const goos = GOOS[process.platform];
  const goarch = GOARCH[process.arch];
  if (!goos || !goarch) {
    throw new Error(
      `unsupported platform ${process.platform}/${process.arch} (darwin or linux, x64 or arm64); ` +
        `download a binary from https://github.com/${REPO}/releases`,
    );
  }
  return { goos, goarch };
}

function fetch(url, redirects = 0) {
  return new Promise((resolve, reject) => {
    if (redirects > 10) {
      reject(new Error('too many redirects'));
      return;
    }
    const req = https.get(url, { headers: { 'User-Agent': `${BIN}-npm/${version}` } }, (res) => {
      const status = res.statusCode || 0;
      if (status >= 300 && status < 400 && res.headers.location) {
        res.resume();
        resolve(fetch(new URL(res.headers.location, url).toString(), redirects + 1));
        return;
      }
      if (status !== 200) {
        res.resume();
        reject(new Error(`GET ${url} -> HTTP ${status}`));
        return;
      }
      const chunks = [];
      res.on('data', (chunk) => chunks.push(chunk));
      res.on('end', () => resolve(Buffer.concat(chunks)));
      res.on('error', reject);
    });
    req.setTimeout(IDLE_TIMEOUT_MS, () => req.destroy(new Error(`GET ${url}: no data for 60s`)));
    req.on('error', reject);
  });
}

// The line of checksums.txt (GoReleaser's) that names this platform's binary.
function findAsset(checksums, goos, goarch) {
  const want = `${BIN}-${goos}-${goarch}`;
  for (const line of checksums.split('\n')) {
    const m = line.trim().match(/^([a-f0-9]{64})\s+\*?(.+)$/i);
    if (m && m[2] === want) {
      return { name: want, sha256: m[1].toLowerCase() };
    }
  }
  throw new Error(`release v${version} has no ${want} in checksums.txt`);
}

// The binary the package carries, made executable when npm unpacked it without the bit; null when
// it carries none for this platform or it cannot be made executable.
function bundled(goos, goarch) {
  const file = path.join(__dirname, '..', 'vendor', `${goos}-${goarch}`, BIN);
  if (!fs.existsSync(file)) return null;
  try {
    fs.accessSync(file, fs.constants.X_OK);
    return file;
  } catch {
    try {
      fs.chmodSync(file, 0o755);
      return file;
    } catch {
      return null;
    }
  }
}

function cacheDir() {
  const base = process.env.XDG_CACHE_HOME || path.join(os.homedir(), '.cache');
  return path.join(base, BIN, version);
}

async function download(goos, goarch, dest) {
  const base = `https://github.com/${REPO}/releases/download/v${version}`;
  let checksums;
  try {
    checksums = (await fetch(`${base}/checksums.txt`)).toString('utf8');
  } catch (err) {
    if (/HTTP 404$/.test(err.message)) {
      throw new Error(`no public GitHub release v${version} of ${REPO} to download the binary from`);
    }
    throw err;
  }
  const asset = findAsset(checksums, goos, goarch);
  process.stderr.write(`foreman-kit: downloading ${asset.name} of v${version}...\n`);
  const bin = await fetch(`${base}/${asset.name}`);

  const got = crypto.createHash('sha256').update(bin).digest('hex');
  if (got !== asset.sha256) {
    throw new Error(`checksum mismatch for ${asset.name}: expected ${asset.sha256}, got ${got}`);
  }

  fs.mkdirSync(path.dirname(dest), { recursive: true });
  // Written beside and renamed: a launcher running at the same time never sees half a binary.
  const tmp = `${dest}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, bin, { mode: 0o755 });
  fs.renameSync(tmp, dest);
  return dest;
}

async function ensureBinary() {
  const { goos, goarch } = platform();
  const carried = bundled(goos, goarch);
  if (carried) return carried;

  const cached = path.join(cacheDir(), BIN);
  if (fs.existsSync(cached)) return cached;
  return download(goos, goarch, cached);
}

async function main() {
  let binary;
  try {
    binary = await ensureBinary();
  } catch (err) {
    process.stderr.write(`foreman-kit: ${err.message}\n`);
    process.stderr.write(`Install it by hand from https://github.com/${REPO}/releases\n`);
    process.exit(1);
  }

  const child = spawn(binary, process.argv.slice(2), { stdio: 'inherit' });
  for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP', 'SIGQUIT']) {
    process.on(signal, () => {
      if (child.exitCode === null) child.kill(signal);
    });
  }
  child.on('error', (err) => {
    process.stderr.write(`foreman-kit: cannot start ${binary}: ${err.message}\n`);
    process.exit(1);
  });
  child.on('exit', (code, signal) => {
    if (signal) {
      // Ends the way the binary did, so a shell sees the same signal.
      process.removeAllListeners(signal);
      process.kill(process.pid, signal);
      return;
    }
    process.exit(code ?? 1);
  });
}

// Runs only when invoked (npx, the bin link); stays importable for the tests.
if (require.main === module) {
  main();
}

module.exports = { platform, findAsset, bundled, cacheDir, ensureBinary };
