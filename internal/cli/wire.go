// wire.go — turn parsed Args into a wired *tau.AgentSession.
//
// The wire layer is the seam between the cli layer (which only knows about
// argv parsing and config loading) and the agent layer (which only knows
// about turn loops and state trees). Both layers stay clean because this
// file owns the impedance matching.
//
// Inputs : Args + environment (cwd, env vars, config files on disk)
// Outputs: a ready-to-Run AgentSession and the underlying runtime (so
//          callers can defer rt.Shutdown for cleanup).
//
// The wire layer never writes to stdout/stderr except via the returned
// error — all UI happens in the run-mode handlers.

package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	tau "github.com/taucentral/tau/pkg/tau"
)

// fauxModelID is the canonical identifier for the built-in faux provider.
// When args.Model == fauxModelID, wireSession skips provider/auth resolution
// and uses tau.NewFauxProviderFromEnv() directly.
const fauxModelID = "faux"

// wiredSession is the bundle returned by wireSession: an AgentSession ready
// to Run, plus the underlying runtime for shutdown and inspection.
type wiredSession struct {
	Session *tau.AgentSession
	Runtime *tau.AgentSessionRuntime
}

// wireSession turns Args into a fully-wired AgentSession. Performs:
//
//   - Resolves cwd (args.Cwd or os.Getwd).
//   - Loads Settings from disk (global + project when trusted).
//   - Loads ModelsFile (may be missing — built-in defaults still apply).
//   - Resolves Model (args.Model or Settings.DefaultModel; "faux" routes
//     to the built-in faux provider for tests).
//   - Resolves Provider and API key (skipped for "faux").
//   - Constructs the LLMClient (anthropic.Client / openai.Client / faux).
//   - Builds the built-in tool set.
//   - Assembles SessionOptions and calls tau.CreateAgentSessionRuntime.
//   - Wraps the runtime in an AgentSession.
//
// Returns ErrNoModel when no model is configured. Returns provider-specific
// errors when auth resolution fails for non-faux models. Returns
// ErrNoRecentSession / ErrSessionNotFound / ErrForkWithoutSource from the
// resolveStateManager helper when session-resume flags cannot be resolved.
//
// The returned cleanup func MUST be deferred by the caller even when it is
// a no-op: when resolveStateManager injects a caller-owned tau.StateManager,
// the runtime adopts it without ownership (ownsState=false, no Close on
// Shutdown). cleanup closes that injected manager. nil cleanup on error
// return means there is nothing for the caller to clean.
func wireSession(ctx context.Context, args Args) (*wiredSession, func(), error) {
	cwd, err := resolveCwd(args)
	if err != nil {
		return nil, nil, err
	}

	// Resolve session-resume flags (--continue, --resume, --session,
	// --fork, --no-session) into an injected tau.StateManager (or nil for
	// "let the runtime create fresh"). When a manager is injected the
	// runtime adopts it without ownership — the cleanup closure returned
	// here closes it. See resolveStateManager for the deviation notes.
	mgr, sessionID, err := resolveStateManager(args, cwd)
	if err != nil {
		return nil, nil, err
	}

	// Load Settings (global + project when trusted). Trust decisions are
	// owned by config/trust.json; for v1 of the wire layer we treat every
	// cwd as trusted so users can iterate on project-scoped overrides
	// without tripping the prompt every run. The Phase 9 trust prompt
	// (per spec D2.1) layers on top later.
	settings, err := loadEffectiveSettings(ctx, cwd)
	if err != nil {
		if mgr != nil {
			_ = mgr.Close()
		}
		return nil, nil, err
	}

	modelID := resolveModel(args, settings)
	if modelID == "" {
		if mgr != nil {
			_ = mgr.Close()
		}
		return nil, nil, ErrNoModel
	}

	// Load the user's models.json (may be empty / missing).
	modelsFile, err := loadModelsFile()
	if err != nil {
		if mgr != nil {
			_ = mgr.Close()
		}
		return nil, nil, err
	}

	// Build the LLM client.
	var client tau.LLMClient
	var providerAPI tau.ModelAPI
	if modelID == fauxModelID {
		client = tau.NewFauxProviderFromEnv()
		// providerAPI stays empty: faux doesn't speak any real protocol,
		// so /model treats it as "no validation enforced".
	} else {
		c, api, err := buildRealClient(args, settings, modelsFile, modelID)
		if err != nil {
			if mgr != nil {
				_ = mgr.Close()
			}
			return nil, nil, err
		}
		client = c
		providerAPI = api
	}

	// Build the built-in tool set. Each tool gets the OS-backed Operations
	// implementation; tests that need fake operations construct their own
	// tool instances instead of going through this path.
	toolSet := defaultBuiltinTools()

	// Context window: prefer models.json entry; fall back to a sane default.
	contextWindow := lookupContextWindow(modelsFile, modelID)
	if contextWindow == 0 {
		contextWindow = defaultContextWindow
	}

	opts := tau.SessionOptions{
		Model:         modelID,
		Settings:      settings,
		Tools:         toolSet,
		LLMClient:     client,
		ContextWindow: contextWindow,
		KnownModels:   modelsFile.AllKnownModels(),
		ProviderAPI:   providerAPI,
		StateManager:  mgr, // nil OK: runtime creates a fresh owned session.
		SessionID:     sessionID,
	}
	if args.Thinking != "" {
		opts.ThinkingLevel = tau.ThinkingLevel(args.Thinking)
	}

	rt, err := tau.CreateAgentSessionRuntime(ctx, cwd, opts)
	if err != nil {
		// We own mgr (runtime did not adopt it). Close is idempotent per
		// manager.go:127 — safe even if a future runtime change adopts
		// the manager partway through CreateAgentSessionRuntime.
		if mgr != nil {
			_ = mgr.Close()
		}
		return nil, nil, err
	}

	// Build the cleanup closure. When mgr is nil, cleanup is a no-op so
	// callers can `defer cleanup()` unconditionally. Capturing mgr into a
	// local here keeps the closure's lifetime independent of later mutations
	// to the variable (there are none today, but it's the right shape).
	cleanup := func() {}
	if mgr != nil {
		m := mgr
		cleanup = func() { _ = m.Close() }
	}

	return &wiredSession{
		Session: tau.NewAgentSession(rt),
		Runtime: rt,
	}, cleanup, nil
}

// resolveStateManager resolves session-resume flags into an injected
// tau.StateManager (or nil for "let the runtime create fresh"). Returns
// (mgr, sessionID, err). sessionID is the resolved leaf entry ID, advisory
// for opts.SessionID; empty for fresh-session and for --no-session.
//
// Resolution order (design.md D2):
//
//  1. --no-session wins; everything else ignored. Returns an in-memory
//     manager whose data never touches disk.
//  2. At most one source flag (--continue, --resume, --session);
//     multiple → usage error (NOT a typed sentinel).
//  3. --fork requires one of the source flags.
//
// tau deviates from design.md D3 in two ways:
//
//   - tau.OpenManager calls bbolt.Open which CREATES missing files, so
//     we os.Stat first and translate os.IsNotExist to ErrSessionNotFound.
//     Without this check, --resume <bogus> would silently create an empty
//     session file — the worst possible failure mode.
//   - The design referenced free functions state.DefaultSessionsDir,
//     state.ContinueRecent, and state.InMemory that do not exist in the
//     landed code. We use the real primitives: tau.ListSessions (added
//     for this change), tau.OpenManager, tau.NewInMemoryManager, and
//     tau.SessionsDir.
//
// tau deviates from design.md D4: Manager.SourcePath() is NOT added to
// the interface (the pkg/tau/sdk.go:284 type alias makes any interface-
// method addition an SDK-breaking change). The source path is re-derived
// here from args + cwd.
//
// Sentinel error wrapping discipline: ErrNoRecentSession,
// ErrSessionNotFound, and ErrForkWithoutSource are returned naked (no
// %w) so errors.Is matches without unwrap chains. All other errors wrap
// their cause.
func resolveStateManager(args Args, cwd string) (tau.StateManager, string, error) {
	if args.NoSession {
		return tau.NewInMemoryManager(cwd), "", nil
	}

	sources := 0
	if args.Continue {
		sources++
	}
	if args.Resume != "" {
		sources++
	}
	if args.Session != "" {
		sources++
	}
	if sources > 1 {
		return nil, "", fmt.Errorf("cli: at most one of --continue, --resume, --session may be set (got %d); pick one", sources)
	}

	if args.Fork && sources == 0 {
		return nil, "", ErrForkWithoutSource
	}

	var (
		mgr        tau.StateManager
		sourcePath string
		sessionID  string
	)

	switch {
	case args.Continue:
		sessions, err := tau.ListSessions(cwd)
		if err != nil {
			return nil, "", fmt.Errorf("cli: list sessions for --continue: %w", err)
		}
		if len(sessions) == 0 {
			return nil, "", ErrNoRecentSession
		}
		sourcePath = sessions[0].Path
		mgr, err = tau.OpenManager(sourcePath, cwd)
		if err != nil {
			return nil, "", fmt.Errorf("cli: open recent session %s: %w", sourcePath, err)
		}
		sessionID = mgr.LeafID()

	case args.Resume != "":
		sessionsDir, err := tau.SessionsDir(cwd)
		if err != nil {
			return nil, "", fmt.Errorf("cli: resolve sessions dir: %w", err)
		}
		sourcePath = filepath.Join(sessionsDir, args.Resume)
		if _, err := os.Stat(sourcePath); err != nil {
			if os.IsNotExist(err) {
				return nil, "", ErrSessionNotFound
			}
			return nil, "", fmt.Errorf("cli: stat session %s: %w", sourcePath, err)
		}
		mgr, err = tau.OpenManager(sourcePath, cwd)
		if err != nil {
			return nil, "", fmt.Errorf("cli: open resumed session %s: %w", sourcePath, err)
		}
		sessionID = mgr.LeafID()

	case args.Session != "":
		sourcePath = args.Session
		if _, err := os.Stat(sourcePath); err != nil {
			if os.IsNotExist(err) {
				return nil, "", ErrSessionNotFound
			}
			return nil, "", fmt.Errorf("cli: stat session %s: %w", sourcePath, err)
		}
		var err error
		mgr, err = tau.OpenManager(sourcePath, cwd)
		if err != nil {
			return nil, "", fmt.Errorf("cli: open session %s: %w", sourcePath, err)
		}
		sessionID = mgr.LeafID()

	default:
		// Fresh session; let the runtime create. (--fork alone already
		// errored above.)
		return nil, "", nil
	}

	if args.Fork {
		// Forking needs a destination manager whose backing file is NOT
		// sourcePath (otherwise ForkFrom's internal bbolt.Open on sourcePath
		// deadlocks against our own handle — bbolt file locks are process-wide
		// and openTimeout is 5s per store_bbolt.go:33). Create a fresh
		// destination session under cwd, close the source handle so its
		// lock is released, then call ForkFrom on the destination.
		//
		// This mirrors the existing pattern at
		// internal/state/manager_test.go:451-475: src.Close() happens
		// BEFORE dst.ForkFrom(srcPath). The naive shape
		// `mgr.ForkFrom(sourcePath)` while mgr still holds sourcePath is
		// a bug.
		if err := mgr.Close(); err != nil {
			return nil, "", fmt.Errorf("cli: close source before fork: %w", err)
		}
		dst, err := tau.CreateManager(cwd, tau.SessionHeaderPayload{
			Cwd:           cwd,
			Model:         "", // forked sessions don't pin a model at header time
			ParentSession: sourcePath,
		})
		if err != nil {
			return nil, "", fmt.Errorf("cli: create fork destination: %w", err)
		}
		forked, err := dst.ForkFrom(sourcePath)
		if err != nil {
			_ = dst.Close()
			return nil, "", fmt.Errorf("cli: fork from %s: %w", sourcePath, err)
		}
		// dst and forked share the same new .bolt file; closing dst would
		// close forked's backing store. Return forked; the caller owns it
		// and will close it via the wireSession cleanup closure.
		_ = dst.Close()
		return forked, forked.LeafID(), nil
	}

	return mgr, sessionID, nil
}

// resolveCwd returns args.Cwd when non-empty, otherwise os.Getwd.
func resolveCwd(args Args) (string, error) {
	if args.Cwd != "" {
		return filepath.Abs(args.Cwd)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	return cwd, nil
}

// loadEffectiveSettings loads global + project (when trusted) Settings.
// Errors only when the on-disk files are unparseable; missing files fall
// back to defaults.
func loadEffectiveSettings(ctx context.Context, cwd string) (tau.Settings, error) {
	agentDir, err := tau.AgentDir()
	if err != nil {
		return tau.DefaultSettings(), nil
	}
	storage, err := tau.NewFileSettingsStorage(agentDir, cwd, true /* trusted */)
	if err != nil {
		return tau.Settings{}, fmt.Errorf("open settings storage: %w", err)
	}
	defer storage.Close()
	s, err := storage.Load(ctx)
	if err != nil {
		return tau.Settings{}, fmt.Errorf("load settings: %w", err)
	}
	return s, nil
}

// loadModelsFile reads <agentDir>/models.json. Missing file → empty ModelsFile.
func loadModelsFile() (*tau.ModelsFile, error) {
	agentDir, err := tau.AgentDir()
	if err != nil {
		//nolint:nilerr // no agent dir → no models.json; empty ModelsFile is the documented default.
		return &tau.ModelsFile{}, nil
	}
	path := filepath.Join(agentDir, "models.json")
	mf, err := tau.LoadModelsFile(path)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	return mf, nil
}

// resolveModel returns args.Model, else Settings.DefaultModel, else "".
func resolveModel(args Args, settings tau.Settings) string {
	if args.Model != "" {
		return args.Model
	}
	if settings.DefaultModel != nil && *settings.DefaultModel != "" {
		return *settings.DefaultModel
	}
	return ""
}

// buildRealClient builds an LLM client for a non-faux model and reports
// which API family was selected. Resolves:
//
//   - Provider (args.Provider or Settings.DefaultProvider or infer from
//     models.json model entry).
//   - Provider definition (from models.json when present).
//   - API key via the provider's auth resolution chain.
//   - Base URL (from provider definition, env var, or provider default).
//
// The returned ModelAPI lets the caller (wireSession) populate
// SessionOptions.ProviderAPI so /model can refuse cross-API switches
// honestly instead of mutating the model id while leaving the wrong
// protocol client wired up.
func buildRealClient(args Args, settings tau.Settings, mf *tau.ModelsFile, modelID string) (tau.LLMClient, tau.ModelAPI, error) {
	provider := args.Provider
	if provider == "" && settings.DefaultProvider != nil {
		provider = *settings.DefaultProvider
	}

	// Try models.json first for an explicit API + baseURL.
	if provider != "" {
		if pdef, ok := mf.Providers[provider]; ok {
			api := pdef.API
			md := tau.ResolveModel(mf.Providers, mf.Models, provider, modelID)
			if md != nil {
				c, err := buildClientFromModel(string(api), provider, md.BaseURL, pdef)
				return c, api, err
			}
			c, err := buildClientFromModel(string(api), provider, pdef.BaseURL, pdef)
			return c, api, err
		}
	}

	// Infer from model ID prefix when no provider is configured.
	api := inferAPIFromModel(modelID)
	switch api {
	case tau.APIAnthropic:
		c, err := buildAnthropicClient(mf, provider)
		return c, api, err
	case tau.APIOpenAI:
		c, err := buildOpenAIClient(mf, provider)
		return c, api, err
	case tau.APIGemini, tau.APIMistral, tau.APIBedrock:
		// Providers defined in models.json but not yet wired in tau; the
		// default branch's error message tells the caller how to proceed.
		return nil, api, fmt.Errorf("wire: provider API %q not yet implemented for model %q (set --provider or models.json providers.<name>.models)", api, modelID)
	default:
		return nil, "", fmt.Errorf("wire: cannot infer provider for model %q (set --provider or models.json providers.<name>.models)", modelID)
	}
}

// buildClientFromModel dispatches on API to the right provider constructor.
// Used when models.json supplies the provider definition. The caller is
// responsible for picking the right BaseURL (model-level or provider-level)
// before invoking; the per-model definition itself is not needed here.
func buildClientFromModel(api, providerName, baseURL string, pdef tau.ProviderDefinition) (tau.LLMClient, error) {
	authStore, _ := openAuthStore()
	switch api {
	case string(tau.APIAnthropic):
		apiKey, err := resolveAnthropicAuth(pdef.APIKey, authStore)
		if err != nil {
			return nil, err
		}
		opts := tau.AnthropicProviderOptions{APIKey: apiKey, BaseURL: baseURL, HTTPClient: buildHTTPClient()}
		if baseURL == "" {
			opts.BaseURL = tau.AnthropicDefaultBaseURL
		}
		return tau.NewAnthropicProvider(opts)
	case string(tau.APIOpenAI):
		envVar := openaiEnvVarFor(providerName)
		apiKey, err := resolveOpenAIAuth(providerName, envVar, pdef.APIKey, authStore)
		if err != nil {
			return nil, err
		}
		opts := tau.OpenAIProviderOptions{APIKey: apiKey, BaseURL: baseURL, HTTPClient: buildHTTPClient()}
		if baseURL == "" {
			opts.BaseURL = tau.OpenAIDefaultBaseURL
		}
		return tau.NewOpenAIProvider(opts)
	default:
		return nil, fmt.Errorf("wire: provider API %q is not supported yet (configure as anthropic or openai)", api)
	}
}

// buildAnthropicClient resolves auth and constructs an anthropic.Client.
func buildAnthropicClient(mf *tau.ModelsFile, providerName string) (tau.LLMClient, error) {
	var explicitKey string
	var baseURL string
	if providerName != "" {
		if pdef, ok := mf.Providers[providerName]; ok {
			explicitKey = pdef.APIKey
			baseURL = pdef.BaseURL
		}
	}
	authStore, _ := openAuthStore()
	apiKey, err := resolveAnthropicAuth(explicitKey, authStore)
	if err != nil {
		return nil, err
	}
	opts := tau.AnthropicProviderOptions{APIKey: apiKey, BaseURL: baseURL, HTTPClient: buildHTTPClient()}
	if baseURL == "" {
		opts.BaseURL = tau.AnthropicDefaultBaseURL
	}
	return tau.NewAnthropicProvider(opts)
}

// buildOpenAIClient resolves auth and constructs an openai.Client.
func buildOpenAIClient(mf *tau.ModelsFile, providerName string) (tau.LLMClient, error) {
	var explicitKey string
	var baseURL string
	envVar := tau.OpenAIEnvVar
	if providerName != "" {
		if pdef, ok := mf.Providers[providerName]; ok {
			explicitKey = pdef.APIKey
			baseURL = pdef.BaseURL
			envVar = openaiEnvVarFor(providerName)
		}
	}
	authStore, _ := openAuthStore()
	apiKey, err := resolveOpenAIAuth(providerName, envVar, explicitKey, authStore)
	if err != nil {
		return nil, err
	}
	opts := tau.OpenAIProviderOptions{APIKey: apiKey, BaseURL: baseURL, HTTPClient: buildHTTPClient()}
	if baseURL == "" {
		opts.BaseURL = tau.OpenAIDefaultBaseURL
	}
	return tau.NewOpenAIProvider(opts)
}

// resolveAnthropicAuth uses the tau.ResolveAnthropicAuth chain. Wraps the
// "no credential" error as ErrNoCredentials so the cli layer can render
// a friendlier message.
func resolveAnthropicAuth(explicit string, auth tau.AuthStore) (string, error) {
	apiKey, _, err := tau.ResolveAnthropicAuth(explicit, auth)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoCredentials, err)
	}
	return apiKey, nil
}

// resolveOpenAIAuth uses the tau.ResolveOpenAIAuth chain.
func resolveOpenAIAuth(providerName, envVar, explicit string, auth tau.AuthStore) (string, error) {
	apiKey, _, err := tau.ResolveOpenAIAuth(providerName, envVar, explicit, auth)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoCredentials, err)
	}
	return apiKey, nil
}

// openAuthStore opens auth.json from AgentDir. Missing file → nil store
// (provider auth chains treat nil as "skip auth.json step").
func openAuthStore() (tau.AuthStore, error) {
	agentDir, err := tau.AgentDir()
	if err != nil {
		return nil, err
	}
	return tau.NewFileAuthStore(filepath.Join(agentDir, "auth.json"), nil), nil
}

// openaiEnvVarFor maps a provider name to its expected env var. Defaults
// to OPENAI_API_KEY for unknown providers.
func openaiEnvVarFor(providerName string) string {
	switch strings.ToLower(providerName) {
	case "deepseek":
		return "DEEPSEEK_API_KEY"
	case "groq":
		return "GROQ_API_KEY"
	case "openrouter":
		return "OPENROUTER_API_KEY"
	}
	return tau.OpenAIEnvVar
}

// inferAPIFromModel guesses the API family from the model id. Recognizes
// the common prefixes; unknown ids return "" so the caller can render
// "configure models.json" guidance.
func inferAPIFromModel(id string) tau.ModelAPI {
	switch {
	case strings.HasPrefix(id, "claude"):
		return tau.APIAnthropic
	case strings.HasPrefix(id, "gpt"), strings.HasPrefix(id, "o1"), strings.HasPrefix(id, "o3"), strings.HasPrefix(id, "o4"):
		return tau.APIOpenAI
	case strings.HasPrefix(id, "deepseek"):
		return tau.APIOpenAI
	case strings.HasPrefix(id, "llama"), strings.HasPrefix(id, "qwen"):
		return tau.APIOpenAI
	}
	return ""
}

// lookupContextWindow scans models.json for modelID and returns its
// ContextWindow when present. Returns 0 when the model isn't listed;
// the caller falls back to defaultContextWindow.
func lookupContextWindow(mf *tau.ModelsFile, modelID string) int {
	// Provider-attached models take precedence — they inherit context
	// window from their provider entry when set.
	for name, p := range mf.Providers {
		for _, md := range p.Models {
			if md.ID == modelID && md.ContextWindow > 0 {
				return md.ContextWindow
			}
		}
		_ = name // silence unused-name if no provider models matched
	}
	for _, md := range mf.Models {
		if md.ID == modelID && md.ContextWindow > 0 {
			return md.ContextWindow
		}
	}
	return 0
}

// defaultContextWindow is the fallback when neither models.json nor the
// caller supplies a window. 200k matches the common Claude / GPT-4o
// range; the compactor gracefully degrades below this.
const defaultContextWindow = 200000

// defaultBuiltinTools returns the standard built-in tool set (read, bash,
// edit, write, grep, find, ls), all wired to their OS-backed Operations
// implementations. The construction is delegated to tau.BuiltinTools so
// the SDK owns the canonical factory list; this thin wrapper exists only
// so callers in this package have a local name to reach for.
//
// The return type is []tau.HeadlessTool (the functional contract) so
// that callers that want to mix in additional headless tools can append
// without conversion. Every concrete type returned also satisfies Tool
// (the TUI rendering contract), which is preserved by the interface
// embedding relationship.
func defaultBuiltinTools() []tau.HeadlessTool {
	return tau.BuiltinTools()
}

// buildHTTPClient returns an *http.Client with reasonable defaults for
// LLM provider calls. Proxy is honored from HTTP_PROXY / https_proxy.
// Idle timeout is generous because streams can be long-lived.
func buildHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// Keep idle conns bounded so a long-lived tau process doesn't
		// accumulate file descriptors.
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
	// Honor HTTPProxy from Settings if it parses as a URL. This mirrors
	// pi's behavior: explicit settings proxy beats env proxy.
	if proxy := os.Getenv("TAU_HTTP_PROXY"); proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{
		Transport: transport,
		// No overall timeout — the streaming endpoints hold the body
		// open for the duration of the model's reply. Per-request
		// timeouts are applied at the retry layer.
	}
}

// ErrNoModel is returned when no model is configured. Callers should
// render a friendly message ("set --model, Settings.DefaultModel, or run
// `tau --list-models`").
var ErrNoModel = errors.New("no model configured")

// ErrNoCredentials is returned when the provider auth chain failed. The
// underlying error message identifies which step the chain exhausted.
var ErrNoCredentials = errors.New("no API credentials")

// ErrNoRecentSession is returned by resolveStateManager when --continue is
// set but no prior session exists under cwd. The runtime falls back to a
// fresh session when the CLI surfaces this to the user; programmatic
// callers may handle it via errors.Is.
var ErrNoRecentSession = errors.New("no recent session to continue")

// ErrSessionNotFound is returned by resolveStateManager when --resume <id>
// or --session <path> targets a path that does not exist on disk. tau
// deviates from a naive tau.OpenManager call because bbolt.Open creates
// missing files — without this stat-first check, --resume <bogus-id>
// would silently create an empty session file.
var ErrSessionNotFound = errors.New("session not found")

// ErrForkWithoutSource is returned by resolveStateManager when --fork is
// set without one of --continue / --resume / --session. Forking requires
// a source session to branch from.
var ErrForkWithoutSource = errors.New("--fork requires one of --continue, --resume, or --session")
