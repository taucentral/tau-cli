// plugin.go — `tau plugin install|list|remove|update` handlers.
//
// Plugins are discovered by the Manager (internal/plugins/manager.go) via
// the tau-plugin-* naming convention in the plugins directory. This file
// owns the lifecycle commands that populate that directory and record
// provenance in Settings.Plugins (the pin map).
//
// Install sources:
//   - HTTPS URL (passthrough)
//   - GitHub shorthand "owner/repo" (resolved via the GitHub releases API)
//   - Local file path (copied verbatim)
//
// Checksums:
//   - Explicit --sha256 wins.
//   - Else GET <url>.sha256 and parse the first hex token.
//   - Else warn and install unverified.
//
// Non-TTY install/remove/update refuse without --yes (Decision 5).

package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	tau "github.com/taucentral/tau/pkg/tau"
)

// pluginSubAction labels the four plugin subcommand actions.
type pluginSubAction string

const (
	pluginInstall pluginSubAction = "install"
	pluginList    pluginSubAction = "list"
	pluginRemove  pluginSubAction = "remove"
	pluginUpdate  pluginSubAction = "update"
)

// runPluginSubcommand dispatches `tau plugin <action> [args]`.
func runPluginSubcommand(ctx context.Context, args Args) error {
	if len(args.SubcommandArgs) == 0 {
		pluginUsage(os.Stderr)
		return fmt.Errorf("plugin: no action specified")
	}
	action, rest := args.SubcommandArgs[0], args.SubcommandArgs[1:]
	switch pluginSubAction(action) {
	case pluginInstall:
		return runPluginInstall(ctx, args, rest)
	case pluginList:
		return runPluginList(ctx, args, rest)
	case pluginRemove:
		return runPluginRemove(ctx, args, rest)
	case pluginUpdate:
		return runPluginUpdate(ctx, args, rest)
	default:
		pluginUsage(os.Stderr)
		return fmt.Errorf("plugin: unknown action %q", action)
	}
}

// pluginUsage writes the four-action summary to w.
func pluginUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: tau plugin <action> [args]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Actions:")
	fmt.Fprintln(w, "  install <source> [--sha256 <hex>] [--yes]   Install a plugin from URL, GitHub shorthand, or local file")
	fmt.Fprintln(w, "  list                                        List installed plugins and their provenance")
	fmt.Fprintln(w, "  remove <name> [--yes]                       Remove a plugin binary and its pin")
	fmt.Fprintln(w, "  update [<name>] [--version <v>] [--sha256 <hex>] [--force] [--yes]")
	fmt.Fprintln(w, "                                              Update one or all plugins from their recorded source")
}

// --- Install ---------------------------------------------------------------

// pluginInstallFlags captures the flags accepted by `tau plugin install`.
type pluginInstallFlags struct {
	source string
	sha256 string
	yes    bool
}

func parsePluginInstallFlags(args []string) (pluginInstallFlags, error) {
	f := pluginInstallFlags{}
	i := 0
	for i < len(args) {
		tok := args[i]
		switch {
		case tok == "--sha256":
			if i+1 >= len(args) {
				return f, fmt.Errorf("plugin install: --sha256 requires a value")
			}
			f.sha256 = args[i+1]
			i += 2
		case strings.HasPrefix(tok, "--sha256="):
			f.sha256 = strings.TrimPrefix(tok, "--sha256=")
			i++
		case tok == "--yes", tok == "-y":
			f.yes = true
			i++
		case strings.HasPrefix(tok, "-"):
			return f, fmt.Errorf("plugin install: unknown flag %q", tok)
		default:
			if f.source != "" {
				return f, fmt.Errorf("plugin install: unexpected extra argument %q", tok)
			}
			f.source = tok
			i++
		}
	}
	if f.source == "" {
		return f, fmt.Errorf("plugin install: source is required (URL, GitHub shorthand, or local path)")
	}
	return f, nil
}

// runPluginInstall handles `tau plugin install <source>`.
func runPluginInstall(ctx context.Context, parsed Args, subArgs []string) error {
	flags, err := parsePluginInstallFlags(subArgs)
	if err != nil {
		return err
	}

	cwd, err := resolveCwd(parsed)
	if err != nil {
		return err
	}

	resolved, err := resolvePluginSource(ctx, flags.source)
	if err != nil {
		return err
	}

	// Derive the short name from the source (used for the binary name and
	// the pin key).
	shortName, err := derivePluginShortName(resolved)
	if err != nil {
		return err
	}

	// Resolve the destination path. Plugins install into the GLOBAL dir
	// (dirs[0] from PluginsDir), which is <ConfigDir>/plugins. This must
	// match what the Manager discovers via PluginsDir.
	dirs, err := tau.PluginsDir(cwd)
	if err != nil {
		return fmt.Errorf("resolve plugin dirs: %w", err)
	}
	pluginsDir := dirs[0]
	dstPath := pluginBinaryPath(pluginsDir, shortName)

	// Check for an existing install.
	existing := false
	if info, statErr := os.Stat(dstPath); statErr == nil && !info.IsDir() {
		existing = true
	}

	// Resolve checksum.
	checksum, checksumStatus, err := resolveChecksum(ctx, resolved.downloadURL, flags.sha256)
	if err != nil {
		return err
	}

	// Non-TTY guard (Decision 5).
	if !isTTY(os.Stdin) && !flags.yes {
		return errors.New("plugin install: requires --yes for non-interactive use")
	}

	// Always print the checksum status so non-TTY users see it.
	fmt.Fprintf(os.Stderr, "plugin install: %q from %s\n", shortName, resolved.displayURL)
	fmt.Fprintf(os.Stderr, "  checksum: %s\n", checksumStatus)
	fmt.Fprintf(os.Stderr, "  target:   %s\n", dstPath)

	// Prompt.
	if isTTY(os.Stdin) {
		if existing {
			ok, err := confirmPrompt(fmt.Sprintf("Overwrite existing plugin %q at %s? [y/N] ", shortName, dstPath))
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintln(os.Stderr, "plugin install: aborted")
				return nil
			}
		}
		fmt.Fprintf(os.Stderr, "Install plugin %q from %s\n", shortName, resolved.displayURL)
		fmt.Fprintf(os.Stderr, "  checksum: %s\n", checksumStatus)
		fmt.Fprintf(os.Stderr, "  target:   %s\n", dstPath)
		ok, err := confirmPrompt("Proceed? [y/N] ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "plugin install: aborted")
			return nil
		}
	}

	// Fetch + verify + install.
	actualSha, err := fetchVerifyInstall(ctx, resolved, dstPath, pluginsDir, checksum)
	if err != nil {
		return err
	}

	// Save pin.
	pin := tau.PluginPin{
		Source:      flags.source,
		Version:     resolved.version,
		Sha256:      actualSha,
		InstalledAt: time.Now().UTC(),
	}
	if err := savePluginPin(ctx, cwd, shortName, pin); err != nil {
		return fmt.Errorf("plugin install: save pin: %w", err)
	}

	fmt.Fprintf(os.Stderr, "plugin install: %s installed (sha256: %s)\n", shortName, actualSha)
	return nil
}

// --- List ------------------------------------------------------------------

// runPluginList handles `tau plugin list`.
func runPluginList(ctx context.Context, parsed Args, _ []string) error {
	cwd, err := resolveCwd(parsed)
	if err != nil {
		return err
	}
	dirs, err := tau.PluginsDir(cwd)
	if err != nil {
		return fmt.Errorf("resolve plugin dirs: %w", err)
	}
	globalDir := dirs[0]
	var projectDir string
	if len(dirs) > 1 {
		projectDir = dirs[1]
	}

	// Discover on-disk entries (no spawn).
	hostSrv := tau.NoopHostServer()
	mgr, err := tau.NewPluginManager(projectDir, globalDir, hostSrv)
	if err != nil {
		return fmt.Errorf("plugin list: %w", err)
	}
	paths, _ := mgr.Discover()

	// Load pins.
	pins, err := loadPluginPins(ctx, cwd)
	if err != nil {
		return fmt.Errorf("plugin list: %w", err)
	}

	// Compute checksum status per entry.
	type row struct {
		source   string
		short    string
		version  string
		cksum    string
		origin   string
	}
	rows := make([]row, 0, len(paths))
	for _, p := range paths {
		var version, cksum, origin string
		if pin, ok := pins[p.ShortName]; ok {
			version = pin.Version
			origin = pin.Source
			cksum = checksumStatusFor(p.AbsPath, pin.Sha256)
		} else {
			cksum = "manual"
		}
		rows = append(rows, row{
			source:  p.Source.String(),
			short:   p.ShortName,
			version: version,
			cksum:   cksum,
			origin:  origin,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].source != rows[j].source {
			return rows[i].source < rows[j].source
		}
		return rows[i].short < rows[j].short
	})

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SOURCE\tNAME\tVERSION\tCHECKSUM\tORIGIN")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.source, r.short, r.version, r.cksum, r.origin)
	}
	return tw.Flush()
}

// checksumStatusFor computes the on-disk sha256 and compares it to the
// recorded pin sha256. Returns "verified", "mismatch", or "unverified"
// (when the pin has no sha256).
func checksumStatusFor(path, pinSha string) string {
	if pinSha == "" {
		return "unverified"
	}
	actual, err := sha256File(path)
	if err != nil {
		return "mismatch"
	}
	if actual == pinSha {
		return "verified"
	}
	return "mismatch"
}

// --- Remove ----------------------------------------------------------------

// runPluginRemove handles `tau plugin remove <name>`.
func runPluginRemove(ctx context.Context, parsed Args, subArgs []string) error {
	if len(subArgs) == 0 || strings.HasPrefix(subArgs[0], "-") {
		return fmt.Errorf("plugin remove: short name is required")
	}
	shortName := subArgs[0]
	yes := false
	for _, a := range subArgs[1:] {
		if a == "--yes" || a == "-y" {
			yes = true
		}
	}

	cwd, err := resolveCwd(parsed)
	if err != nil {
		return err
	}
	dirs, err := tau.PluginsDir(cwd)
	if err != nil {
		return fmt.Errorf("resolve plugin dirs: %w", err)
	}
	globalDir := dirs[0]
	var projectDir string
	if len(dirs) > 1 {
		projectDir = dirs[1]
	}
	hostSrv := tau.NoopHostServer()
	mgr, err := tau.NewPluginManager(projectDir, globalDir, hostSrv)
	if err != nil {
		return fmt.Errorf("plugin remove: %w", err)
	}
	paths, _ := mgr.Discover()

	var target string
	for _, p := range paths {
		if p.ShortName == shortName {
			if target != "" && target != p.AbsPath {
				return fmt.Errorf("plugin remove: %q matches multiple paths (%s, %s)", shortName, target, p.AbsPath)
			}
			target = p.AbsPath
		}
	}
	if target == "" {
		return fmt.Errorf("plugin remove: %q is not installed", shortName)
	}

	if !isTTY(os.Stdin) && !yes {
		return errors.New("plugin remove: requires --yes for non-interactive use")
	}
	if isTTY(os.Stdin) {
		ok, err := confirmPrompt(fmt.Sprintf("Remove %s? [y/N] ", target))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "plugin remove: aborted")
			return nil
		}
	}

	if err := os.Remove(target); err != nil {
		return fmt.Errorf("plugin remove: %w", err)
	}
	if err := removePluginPin(ctx, cwd, shortName); err != nil {
		return fmt.Errorf("plugin remove: delete pin: %w", err)
	}
	fmt.Fprintf(os.Stderr, "plugin remove: %s removed\n", shortName)
	return nil
}

// --- Update ----------------------------------------------------------------

// pluginUpdateFlags captures the flags accepted by `tau plugin update`.
type pluginUpdateFlags struct {
	name    string
	version string
	sha256  string
	force   bool
	yes     bool
}

func parsePluginUpdateFlags(args []string) (pluginUpdateFlags, error) {
	f := pluginUpdateFlags{}
	i := 0
	for i < len(args) {
		tok := args[i]
		switch {
		case tok == "--version":
			if i+1 >= len(args) {
				return f, fmt.Errorf("plugin update: --version requires a value")
			}
			f.version = args[i+1]
			i += 2
		case strings.HasPrefix(tok, "--version="):
			f.version = strings.TrimPrefix(tok, "--version=")
			i++
		case tok == "--sha256":
			if i+1 >= len(args) {
				return f, fmt.Errorf("plugin update: --sha256 requires a value")
			}
			f.sha256 = args[i+1]
			i += 2
		case strings.HasPrefix(tok, "--sha256="):
			f.sha256 = strings.TrimPrefix(tok, "--sha256=")
			i++
		case tok == "--force":
			f.force = true
			i++
		case tok == "--yes", tok == "-y":
			f.yes = true
			i++
		case strings.HasPrefix(tok, "-"):
			return f, fmt.Errorf("plugin update: unknown flag %q", tok)
		default:
			if f.name != "" {
				return f, fmt.Errorf("plugin update: unexpected extra argument %q", tok)
			}
			f.name = tok
			i++
		}
	}
	return f, nil
}

// runPluginUpdate handles `tau plugin update [<name>]`.
func runPluginUpdate(ctx context.Context, parsed Args, subArgs []string) error {
	flags, err := parsePluginUpdateFlags(subArgs)
	if err != nil {
		return err
	}

	cwd, err := resolveCwd(parsed)
	if err != nil {
		return err
	}
	pins, err := loadPluginPins(ctx, cwd)
	if err != nil {
		return fmt.Errorf("plugin update: %w", err)
	}

	if flags.name != "" {
		return updateOne(ctx, cwd, flags.name, flags, pins)
	}

	// Update all.
	if len(pins) == 0 {
		fmt.Fprintln(os.Stderr, "plugin update: no pinned plugins to update")
		return nil
	}
	names := make([]string, 0, len(pins))
	for n := range pins {
		names = append(names, n)
	}
	sort.Strings(names)
	var anyFailed bool
	for _, n := range names {
		if err := updateOne(ctx, cwd, n, flags, pins); err != nil {
			fmt.Fprintf(os.Stderr, "plugin update: %s: %v\n", n, err)
			anyFailed = true
		}
	}
	if anyFailed {
		return errors.New("plugin update: one or more updates failed")
	}
	return nil
}

// updateOne refetches a single pinned plugin and replaces the binary.
func updateOne(ctx context.Context, cwd, shortName string, flags pluginUpdateFlags, pins map[string]tau.PluginPin) error {
	pin, ok := pins[shortName]
	if !ok {
		return fmt.Errorf("plugin %q has no recorded source — use 'tau plugin remove %s && tau plugin install <source>'", shortName, shortName)
	}
	if pin.Source == "" {
		return fmt.Errorf("plugin %q has no recorded source — use 'tau plugin remove %s && tau plugin install <source>'", shortName, shortName)
	}

	// Resolve the source. When --version is given and the source is a
	// GitHub release URL, swap the tag segment.
	source := pin.Source
	if flags.version != "" {
		source = replaceGitHubTag(source, flags.version)
	}

	resolved, err := resolvePluginSource(ctx, source)
	if err != nil {
		return err
	}

	// Resolve checksum: --sha256 flag, else recorded pin sha256, else sibling.
	expectedSha := flags.sha256
	if expectedSha == "" {
		expectedSha = pin.Sha256
	}
	checksum, checksumStatus, err := resolveChecksum(ctx, resolved.downloadURL, expectedSha)
	if err != nil {
		return err
	}

	dirs, err := tau.PluginsDir(cwd)
	if err != nil {
		return fmt.Errorf("resolve plugin dirs: %w", err)
	}
	pluginsDir := dirs[0]
	dstPath := pluginBinaryPath(pluginsDir, shortName)

	if !isTTY(os.Stdin) && !flags.yes {
		return errors.New("plugin update: requires --yes for non-interactive use")
	}
	if isTTY(os.Stdin) {
		fmt.Fprintf(os.Stderr, "Update plugin %q from %s\n", shortName, resolved.displayURL)
		fmt.Fprintf(os.Stderr, "  checksum: %s\n", checksumStatus)
		ok, err := confirmPrompt("Proceed? [y/N] ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "plugin update: aborted")
			return nil
		}
	}

	actualSha, err := fetchVerifyInstall(ctx, resolved, dstPath, pluginsDir, checksum)
	if err != nil {
		if errors.Is(err, errChecksumMismatch) {
			if !flags.force {
				return fmt.Errorf("plugin update: checksum mismatch (use --force to override): %w", err)
			}
			fmt.Fprintf(os.Stderr, "plugin update: %s checksum mismatch, --force in effect\n", shortName)
			// Re-fetch without checksum enforcement.
			actualSha, err = fetchVerifyInstall(ctx, resolved, dstPath, pluginsDir, "")
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	newPin := tau.PluginPin{
		Source:      pin.Source,
		Version:     resolved.version,
		Sha256:      actualSha,
		InstalledAt: time.Now().UTC(),
	}
	if err := savePluginPin(ctx, cwd, shortName, newPin); err != nil {
		return fmt.Errorf("plugin update: save pin: %w", err)
	}
	fmt.Fprintf(os.Stderr, "plugin update: %s updated (sha256: %s)\n", shortName, actualSha)
	return nil
}

// --- Shared source-resolution + fetch helpers ------------------------------

// resolvedSource is the normalized representation of a user-supplied
// install source. downloadURL is the actual bytes URL; displayURL is
// what we show the user (may differ for GitHub shorthand).
type resolvedSource struct {
	downloadURL string
	displayURL  string
	version     string
	mediaType   string // "raw" or "tarball"
}

// githubShorthandRe matches "owner/repo".
var githubShorthandRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// resolvePluginSource turns the user-supplied source string into a
// normalized resolvedSource. For GitHub shorthand it hits the releases
// API; for HTTPS URLs and local paths it is synchronous.
func resolvePluginSource(ctx context.Context, raw string) (resolvedSource, error) {
	// Local file?
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		if info, err := os.Stat(raw); err == nil && !info.IsDir() {
			abs, _ := filepath.Abs(raw)
			return resolvedSource{
				downloadURL: abs,
				displayURL:  abs,
				mediaType:   "raw",
			}, nil
		}
	}

	// HTTP(S) URL.
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		mt := "raw"
		if isTarball(raw) {
			mt = "tarball"
		}
		return resolvedSource{
			downloadURL: raw,
			displayURL:  raw,
			mediaType:   mt,
		}, nil
	}

	// GitHub shorthand.
	if githubShorthandRe.MatchString(raw) {
		return resolveGitHubLatest(ctx, raw)
	}

	return resolvedSource{}, fmt.Errorf("plugin install: unsupported source %q (expected HTTP(S) URL, GitHub shorthand owner/repo, or local file)", raw)
}

// githubAPIBase is the base URL for the GitHub REST API. Overridden by
// tests to point at an httptest server; in production it is always
// https://api.github.com.
var githubAPIBase = "https://api.github.com"

// resolveGitHubLatest hits the GitHub releases API and picks the asset
// matching the current GOOS/GOARCH.
func resolveGitHubLatest(ctx context.Context, shorthand string) (resolvedSource, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/releases/latest", githubAPIBase, shorthand)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return resolvedSource{}, fmt.Errorf("plugin install: github api: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return resolvedSource{}, fmt.Errorf("plugin install: github api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// GitHub returns 404 from /releases/latest both when the repo
		// has no published releases and when the repo is invisible to
		// the caller (private, or doesn't exist). Surface the most
		// likely cause and the two workarounds instead of the raw body.
		if resp.StatusCode == http.StatusNotFound {
			return resolvedSource{}, fmt.Errorf(
				"plugin install: %s has no published releases (or is private). Create a release with an asset whose name contains %q and %q, or install from a direct HTTPS URL or local path",
				shorthand, runtime.GOOS, runtime.GOARCH,
			)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resolvedSource{}, fmt.Errorf("plugin install: github api %s: %s", resp.Status, string(body))
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return resolvedSource{}, fmt.Errorf("plugin install: github api decode: %w", err)
	}
	// Match asset by os/arch naming convention.
	wantOS := runtime.GOOS
	wantArch := runtime.GOARCH
	for _, a := range rel.Assets {
		ln := strings.ToLower(a.Name)
		if strings.Contains(ln, wantOS) && strings.Contains(ln, wantArch) {
			mt := "raw"
			if isTarball(a.Name) {
				mt = "tarball"
			}
			return resolvedSource{
				downloadURL: a.BrowserDownloadURL,
				displayURL:  a.BrowserDownloadURL,
				version:     rel.TagName,
				mediaType:   mt,
			}, nil
		}
	}
	return resolvedSource{}, fmt.Errorf("plugin install: github release %s has no asset matching %s/%s", shorthand, wantOS, wantArch)
}

// derivePluginShortName picks the short name from the resolved source.
// It uses the last path segment, stripped of extensions and the
// tau-plugin- prefix.
func derivePluginShortName(r resolvedSource) (string, error) {
	if r.version != "" {
		// GitHub asset: the asset name is like tau-plugin-git-linux-amd64.
		// Strip everything after the first "-" following "tau-plugin-".
		base := filepath.Base(r.downloadURL)
		base = strings.TrimSuffix(base, ".tar.gz")
		base = strings.TrimSuffix(base, ".tgz")
		if strings.HasPrefix(base, "tau-plugin-") {
			rest := strings.TrimPrefix(base, "tau-plugin-")
			if idx := strings.IndexByte(rest, '-'); idx > 0 {
				return rest[:idx], nil
			}
			return rest, nil
		}
		return base, nil
	}
	// Local file or URL: use the last segment, strip the tau-plugin-
	// prefix and any extension.
	base := filepath.Base(r.downloadURL)
	for _, ext := range []string{".tar.gz", ".tgz", ".exe", ""} {
		base = strings.TrimSuffix(base, ext)
		if ext == "" {
			break
		}
	}
	if strings.HasPrefix(base, "tau-plugin-") {
		base = strings.TrimPrefix(base, "tau-plugin-")
	}
	if base == "" {
		return "", fmt.Errorf("plugin install: cannot derive short name from %q", r.downloadURL)
	}
	return base, nil
}

// resolveChecksum returns the expected sha256 hex for the download URL.
// Resolution order: explicit flag > sibling <url>.sha256 > none.
// The returned status string is human-readable for the prompt.
func resolveChecksum(ctx context.Context, downloadURL, explicit string) (sha string, status string, err error) {
	if explicit != "" {
		return explicit, "explicit (--sha256)", nil
	}
	// Sibling .sha256 only makes sense for HTTP(S) sources.
	if strings.HasPrefix(downloadURL, "http://") || strings.HasPrefix(downloadURL, "https://") {
		sibURL := downloadURL + ".sha256"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, sibURL, nil)
		if err == nil {
			client := &http.Client{Timeout: 15 * time.Second}
			resp, err := client.Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
					if err == nil {
						fields := strings.Fields(string(body))
						if len(fields) > 0 && isHex64(fields[0]) {
							return fields[0], "verified (sibling .sha256)", nil
						}
					}
				}
			}
		}
	}
	return "", "unverified (no checksum available)", nil
}

// errChecksumMismatch is the sentinel returned by fetchVerifyInstall when
// the downloaded bytes don't match the expected sha256.
var errChecksumMismatch = errors.New("plugin install: checksum mismatch")

// fetchVerifyInstall downloads the resolved source, optionally verifies
// the sha256, extracts (if tarball), and writes the binary to dstPath.
// Returns the actual sha256 hex of the installed binary.
func fetchVerifyInstall(ctx context.Context, r resolvedSource, dstPath, pluginsDir, expectedSha string) (string, error) {
	if err := os.MkdirAll(pluginsDir, 0o700); err != nil {
		return "", fmt.Errorf("plugin install: mkdir plugins dir: %w", err)
	}

	// For local files, stream from disk; for HTTP, stream from the network.
	var body io.ReadCloser
	var err error
	if strings.HasPrefix(r.downloadURL, "http://") || strings.HasPrefix(r.downloadURL, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.downloadURL, nil)
		if err != nil {
			return "", fmt.Errorf("plugin install: build request: %w", err)
		}
		client := &http.Client{Timeout: 5 * time.Minute}
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("plugin install: fetch %s: %w", r.downloadURL, err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return "", fmt.Errorf("plugin install: fetch %s: %s", r.downloadURL, resp.Status)
		}
		body = resp.Body
	} else {
		f, err := os.Open(r.downloadURL)
		if err != nil {
			return "", fmt.Errorf("plugin install: open %s: %w", r.downloadURL, err)
		}
		body = f
	}
	defer body.Close()

	// Tee through the sha256 hasher while writing to a temp file.
	hasher := sha256.New()
	tmp, err := os.CreateTemp(pluginsDir, ".tau-plugin-download-*")
	if err != nil {
		return "", fmt.Errorf("plugin install: temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	tee := io.TeeReader(body, hasher)
	if _, err := io.Copy(tmp, tee); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("plugin install: download: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("plugin install: close temp: %w", err)
	}

	actualSha := hex.EncodeToString(hasher.Sum(nil))

	// Verify when expected is known.
	if expectedSha != "" && actualSha != expectedSha {
		return actualSha, fmt.Errorf("%w: expected %s, got %s", errChecksumMismatch, expectedSha, actualSha)
	}

	// Extract or move into place.
	if r.mediaType == "tarball" {
		if err := extractPluginFromTarball(tmpPath, dstPath); err != nil {
			return actualSha, err
		}
	} else {
		if err := os.Rename(tmpPath, dstPath); err != nil {
			return actualSha, fmt.Errorf("plugin install: place binary: %w", err)
		}
	}

	if err := os.Chmod(dstPath, 0o700); err != nil {
		return actualSha, fmt.Errorf("plugin install: chmod: %w", err)
	}

	// For tarballs the download-time hash covered the archive bytes, which
	// is what GitHub release .sha256 siblings document. The pin, however,
	// must record the sha of the binary that actually lands on disk so
	// `tau plugin list` can verify it via checksumStatusFor. Re-hash the
	// extracted file.
	if r.mediaType == "tarball" {
		binarySha, err := sha256File(dstPath)
		if err != nil {
			return actualSha, fmt.Errorf("plugin install: hash extracted binary: %w", err)
		}
		return binarySha, nil
	}
	return actualSha, nil
}

// extractPluginFromTarball opens srcPath as a .tar.gz and writes the
// single entry matching the tau-plugin-* prefix to dstPath. Rejects
// path-traversal entries and absolute paths.
func extractPluginFromTarball(srcPath, dstPath string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("plugin install: open archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("plugin install: ungzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("plugin install: untar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.Base(hdr.Name)
		if !strings.HasPrefix(name, "tau-plugin-") {
			continue
		}
		// Reject symlinks/hardlinks for safety.
		if hdr.Linkname != "" {
			return fmt.Errorf("plugin install: archive entry %q is a link; refusing", hdr.Name)
		}
		// Write to a temp then rename to dst.
		out, err := os.CreateTemp(filepath.Dir(dstPath), ".tau-plugin-extract-*")
		if err != nil {
			return fmt.Errorf("plugin install: temp extract: %w", err)
		}
		outPath := out.Name()
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			_ = os.Remove(outPath)
			return fmt.Errorf("plugin install: extract %q: %w", hdr.Name, err)
		}
		if err := out.Close(); err != nil {
			_ = os.Remove(outPath)
			return fmt.Errorf("plugin install: close extract: %w", err)
		}
		if err := os.Rename(outPath, dstPath); err != nil {
			_ = os.Remove(outPath)
			return fmt.Errorf("plugin install: place extracted binary: %w", err)
		}
		return nil
	}
	return fmt.Errorf("plugin install: archive has no tau-plugin-* entry")
}

// --- Misc helpers ----------------------------------------------------------

// pluginBinaryPath returns the full path for a plugin binary in pluginsDir.
func pluginBinaryPath(pluginsDir, shortName string) string {
	name := "tau-plugin-" + shortName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(pluginsDir, name)
}

// isTarball reports whether name looks like a .tar.gz / .tgz archive.
func isTarball(name string) bool {
	ln := strings.ToLower(name)
	return strings.HasSuffix(ln, ".tar.gz") || strings.HasSuffix(ln, ".tgz")
}

// isHex64 reports whether s is a 64-char lowercase-or-uppercase hex string.
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// sha256File returns the hex-encoded sha256 of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// confirmPrompt prints msg to stderr and reads a y/N line from stdin.
// Anything but y/Y is treated as no.
func confirmPrompt(msg string) (bool, error) {
	fmt.Fprint(os.Stderr, msg)
	var line string
	if _, err := fmt.Fscanln(os.Stdin, &line); err != nil {
		return false, err
	}
	line = strings.TrimSpace(line)
	return line == "y" || line == "Y", nil
}

// replaceGitHubTag is a best-effort tag rewriter for update --version.
// For URLs that contain /releases/download/<tag>/ it replaces <tag>.
// For other URLs it returns the input unchanged.
func replaceGitHubTag(source, version string) string {
	idx := strings.Index(source, "/releases/download/")
	if idx < 0 {
		return source
	}
	prefix := source[:idx+len("/releases/download/")]
	rest := source[idx+len("/releases/download/"):]
	// rest is <tag>/<asset>; replace <tag>.
	if slash := strings.IndexByte(rest, '/'); slash > 0 {
		return prefix + version + rest[slash:]
	}
	return source
}
