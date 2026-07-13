// plugin_pins.go — helpers that read and mutate the Settings.Plugins map.
//
// Each helper opens its own short-lived FileSettingsStorage so the flock
// + atomic-rename machinery in internal/config is reused without exposing
// a long-lived handle to callers. This mirrors loadEffectiveSettings in
// wire.go.
//
// Pins are stored in the GLOBAL scope (<agentDir>/settings.json) because
// plugin binaries are installed under <ConfigDir>/plugins (the global
// plugins dir). Project-scoped plugin pins are not supported in v1.

package cli

import (
	"context"
	"fmt"

	tau "github.com/taucentral/tau/pkg/tau"
)

// loadPluginPins returns the Settings.Plugins map from the effective
// settings (global + project when trusted). Returns a non-nil empty map
// when no plugins are pinned so callers can range without a nil check.
func loadPluginPins(ctx context.Context, cwd string) (map[string]tau.PluginPin, error) {
	settings, err := loadEffectiveSettings(ctx, cwd)
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	if settings.Plugins == nil {
		return map[string]tau.PluginPin{}, nil
	}
	return settings.Plugins, nil
}

// savePluginPin writes a pin entry into the GLOBAL settings scope,
// replacing any existing entry with the same short name. Opens its own
// short-lived FileSettingsStorage.
func savePluginPin(ctx context.Context, cwd, shortName string, pin tau.PluginPin) error {
	agentDir, err := tau.AgentDir()
	if err != nil {
		return fmt.Errorf("resolve agent dir: %w", err)
	}
	storage, err := tau.NewFileSettingsStorage(agentDir, cwd, true /* trusted */)
	if err != nil {
		return fmt.Errorf("open settings storage: %w", err)
	}
	defer storage.Close()
	return storage.Save(ctx, tau.ScopeGlobal, func(current tau.Settings) tau.Settings {
		if current.Plugins == nil {
			current.Plugins = map[string]tau.PluginPin{}
		}
		current.Plugins[shortName] = pin
		return current
	})
}

// removePluginPin deletes a pin entry from the GLOBAL settings scope.
// Idempotent: removing a short name that has no pin is not an error.
func removePluginPin(ctx context.Context, cwd, shortName string) error {
	agentDir, err := tau.AgentDir()
	if err != nil {
		return fmt.Errorf("resolve agent dir: %w", err)
	}
	storage, err := tau.NewFileSettingsStorage(agentDir, cwd, true /* trusted */)
	if err != nil {
		return fmt.Errorf("open settings storage: %w", err)
	}
	defer storage.Close()
	return storage.Save(ctx, tau.ScopeGlobal, func(current tau.Settings) tau.Settings {
		if current.Plugins == nil {
			return current
		}
		delete(current.Plugins, shortName)
		return current
	})
}
