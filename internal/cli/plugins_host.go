// plugins_host.go — constructs the host-side HostServer the tau binary
// exposes to plugin subprocesses.
//
// The HostServer is the gRPC service plugins call back to for Log,
// Notify, and GetConfig. tau-cli wires it through the broker so every
// spawned plugin can reach it. The ConfigSource adapter below surfaces
// a curated subset of Settings; anything not on the whitelist is
// redacted (returns found=false) so plugins cannot exfiltrate secrets
// stored in settings.json.

package cli

import (
	"io"

	tau "github.com/taucentral/tau/pkg/tau"
)

// settingsConfigSource adapts tau.Settings to the tau.ConfigSource
// interface plugins reach via Host.GetConfig. Only a curated set of
// keys is exposed; everything else returns found=false so sensitive
// values (API keys, auth tokens) stay hidden.
//
// Exposed keys (Decision 4 from enable-cli-plugins/design.md):
//
//   - model.default      → Settings.DefaultModel
//   - provider.default   → Settings.DefaultProvider
//   - thinking.default   → Settings.DefaultThinkingLevel
//
// All other keys — including any plugins.* — return ("", false). The
// Settings.Plugins map stores install provenance (source/sha256), not
// plugin-readable runtime config, so it is intentionally not surfaced.
type settingsConfigSource struct {
	settings tau.Settings
}

// GetConfig resolves the dotted key against the settings snapshot.
// The method is safe for concurrent use: tau.Settings is read-only after
// load and the pointer fields are never mutated in-place.
func (s settingsConfigSource) GetConfig(key string) (string, bool) {
	switch key {
	case "model.default":
		if s.settings.DefaultModel != nil && *s.settings.DefaultModel != "" {
			return *s.settings.DefaultModel, true
		}
		return "", false
	case "provider.default":
		if s.settings.DefaultProvider != nil && *s.settings.DefaultProvider != "" {
			return *s.settings.DefaultProvider, true
		}
		return "", false
	case "thinking.default":
		if s.settings.DefaultThinkingLevel != nil && *s.settings.DefaultThinkingLevel != "" {
			return string(*s.settings.DefaultThinkingLevel), true
		}
		return "", false
	default:
		return "", false
	}
}

// newPluginsHostServer constructs the HostServer the plugin manager
// passes to every spawned plugin. logWriter receives Host.Log output
// (plugin diagnostics); settings backs the ConfigSource for
// Host.GetConfig.
func newPluginsHostServer(logWriter io.Writer, settings tau.Settings) tau.HostServer {
	return tau.NewHostServer(logWriter, settingsConfigSource{settings: settings}, nil)
}
