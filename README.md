# tau-cli

The canonical `tau` binary, depending on the tau SDK
([`github.com/taucentral/tau`](https://github.com/taucentral/tau)) via
`pkg/tau` and the public `pkg/tau/modes` subpackage.

This repo ships the command-line entry point (`cmd/tau`), the CLI wire
layer (`internal/cli`), the bubbletea-based TUI (`internal/tui`), and the
interactive run-mode handler (`internal/interactivemode`). Print and RPC
run-mode handlers live in core's `pkg/tau/modes` so any embedder building
their own CLI can reuse them.

The split exists because Go's module graph materializes every transitive
`require` into every consumer's `go.sum`. Library consumers of `tau` should
not have to download the charmbracelet TUI stack when they never render a
TTY.

## Build

```sh
make build       # bin/tau (static binary, CGO disabled)
make install     # go install
make test        # go test ./...
make e2e         # TAU_RUN_E2E=1 go test ./test/e2e/...
make lint        # golangci-lint run ./...
```

## Run

```sh
./bin/tau                                # interactive TUI (default mode)
./bin/tau --model <id> --provider <name> # pick model + provider explicitly
./bin/tau --print "<prompt>"             # non-interactive single-turn
./bin/tau --rpc                          # JSON-RPC over stdin/stdout (IDEs)
./bin/tau config                         # open settings.json
./bin/tau --help                         # full flag list
```

Configuration lives under `~/.config/tau/agent/`:

| File | Purpose |
|---|---|
| `settings.json` | `DefaultModel`, `DefaultProvider`, keybindings, theme, trust |
| `models.json` | Provider definitions + model registry (id, contextWindow, pricing) |
| `auth.json` | Per-provider API keys (written by the first-run setup wizard) |
| `sessions/` | Append-only state tree per session |

API keys resolve in this order: `auth.json` → provider-specific env var (e.g.
`ANTHROPIC_API_KEY`). For custom providers defined in `models.json`, set
`apiKey` to `$ENV_VAR_NAME` to read from the environment. The first-run
setup wizard (skippable via `--no-setup`) writes `auth.json` interactively.

## Layout

```
tau-cli/
├── cmd/tau/main.go              # binary entry point
├── internal/
│   ├── cli/                     # args, dispatch, wire, subcommands, startup, isatty
│   ├── interactivemode/         # interactive.go — TUI-bound run mode
│   └── tui/                     # bubbletea app, keys, theme, components
└── test/e2e/                    # modes_integration_test.go
```

The interactive package is named `interactivemode` (not `modes`) to avoid
colliding with the imported `pkg/tau/modes` package at every reference.
