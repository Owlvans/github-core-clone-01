# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/Owlvans/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/Owlvans/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A from-scratch reimplementation of GitHub's core forge surface — repositories, issues, pull requests, git smart-HTTP (protocol v2 and v0), GraphQL and MCP — written in Go and shipped as one statically linked binary. The harness entrypoint `main.ts` launches the bundled `forge` binary, which serves the whole API from memory: packfiles are built and parsed natively (no git binary needed), upload-pack responses are cached per repository, and the process is sized for the trial pod's 2 CPU / 1 GiB budget (GOMAXPROCS=2, GOMEMLIMIT set by the wrapper).

Local conformance against the official kit suite: 120/120 checks, perf 191–196/200 across repeated runs.

## Run it

```sh
node main.ts
```

`MAKEFUN_ENV_URL` and `MAKEFUN_SHOP_TOKEN` are provided by the harness; `main.ts` starts `./forge`, registers its port with the env, and waits for the trial to finish. To build the binary from source: `CGO_ENABLED=0 go build -ldflags='-s -w' -o forge .`

---

<sub>Entered on [make.fun](https://make.fun/?ref=owl&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
