#!/usr/bin/env node
// Runs the foreman-kit binary this package carries for the platform it is installed on
// (vendor/<os>-<arch>/foreman-kit): the arguments, the standard streams and the exit code pass
// through unchanged, and so do the signals that stop it.
"use strict";

const { spawn } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

const goos = { darwin: "darwin", linux: "linux" }[process.platform];
const goarch = { x64: "amd64", arm64: "arm64" }[process.arch];
const platform = `${process.platform}/${process.arch}`;

if (!goos || !goarch) {
  console.error(`foreman-kit: ${platform} is not supported (darwin or linux, x64 or arm64)`);
  process.exit(1);
}

const binary = path.join(__dirname, "..", "vendor", `${goos}-${goarch}`, "foreman-kit");
if (!fs.existsSync(binary)) {
  const vendor = path.join(__dirname, "..", "vendor");
  const carried = fs.existsSync(vendor) ? fs.readdirSync(vendor).join(", ") : "none";
  console.error(`foreman-kit: this package carries no binary for ${goos}-${goarch} (it carries: ${carried}).`);
  console.error("Build the package on this platform, or with every platform: scripts/npm-pack.sh --all");
  process.exit(1);
}
try {
  // npm may unpack the binary without its executable bit.
  fs.accessSync(binary, fs.constants.X_OK);
} catch {
  try {
    fs.chmodSync(binary, 0o755);
  } catch (err) {
    console.error(`foreman-kit: ${binary} is not executable and cannot be made so: ${err.message}`);
    process.exit(1);
  }
}

const child = spawn(binary, process.argv.slice(2), { stdio: "inherit" });
for (const signal of ["SIGINT", "SIGTERM", "SIGHUP", "SIGQUIT"]) {
  process.on(signal, () => {
    if (child.exitCode === null) child.kill(signal);
  });
}
child.on("error", (err) => {
  console.error(`foreman-kit: cannot start ${binary}: ${err.message}`);
  process.exit(1);
});
child.on("exit", (code, signal) => {
  if (signal) {
    // Ends the way the binary did, so a shell sees the same signal.
    process.removeAllListeners(signal);
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
