# foreman-kit

The single static binary that lets AI coding agents specify, build, verify and integrate an
application with [OpenSpec](https://openspec.dev). This package carries the binary for each
platform it was built for (macOS and Linux, x64 and arm64) and a small Node launcher that runs
the one of the machine it is installed on. On a platform it carries none for, the launcher
downloads the binary of the GitHub release of the same version, checks its SHA-256 against the
release's `checksums.txt`, and keeps it in `~/.cache/foreman-kit/<version>/` for the next runs.
The version of the package is always the version of the release: updating the package
(`npm install -g @mayeco/foreman-kit@latest`) updates the binary.

It needs, on `PATH`, the two programs an application is kept with:
[`openspec`](https://github.com/Fission-AI/OpenSpec) and
[`agent-memory`](https://github.com/xChuCx/agent-memory).

```bash
foreman-kit version
foreman-kit init ../my-platform --agents claude
```

Without npm: `curl -fsSL https://raw.githubusercontent.com/mayeco/foreman-kit/master/install.sh | sh`.

The package is built from the repository with `scripts/npm-pack.sh` and installed with
`scripts/npm-install.sh`; see the repository's README.
