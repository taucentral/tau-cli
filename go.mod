module github.com/taucentral/tau-cli

go 1.25.0

// Phase 4: pin to a placeholder version. The replace directive below
// points at the local core checkout so phases 4-8 can iterate without
// publishing. Phase 9 drops the replace and runs `go mod tidy` against
// the published core tag (or main-branch HEAD, which Go resolves to a
// pseudo-version of the form v0.0.0-<utc-timestamp>-<short-sha>).

replace github.com/taucentral/tau => /home/bigpod/dev/tau/tau
