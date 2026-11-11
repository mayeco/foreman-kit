# foreman-kit

The single static binary that lets AI coding agents specify, build, verify and integrate an
application with [OpenSpec](https://openspec.dev). This package carries the binary for each
platform it was built for (macOS and Linux, x64 and arm64) and a small Node launcher that runs
the one of the machine it is installed on.

It needs, on `PATH`, the two programs an application is kept with:
[`openspec`](https://github.com/Fission-AI/OpenSpec) and
[`agent-memory`](https://github.com/xChuCx/agent-memory).

```bash
foreman-kit version
foreman-kit init ../my-platform --agents claude
```

The package is built from the repository with `scripts/npm-pack.sh` and installed with
`scripts/npm-install.sh`; see the repository's README.
