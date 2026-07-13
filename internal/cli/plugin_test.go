package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tau "github.com/taucentral/tau/pkg/tau"
)

// newPluginTestArgs returns Args configured for a non-TTY plugin test: cwd
// set to dir, NoSetup true so maybeSetup is skipped.
func newPluginTestArgs(dir string) Args {
	return Args{Cwd: dir, NoSetup: true}
}

// sha256Hex returns the hex sha256 of data.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// --- parsePluginInstallFlags ------------------------------------------------

func TestParsePluginInstallFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want pluginInstallFlags
		err  bool
	}{
		{name: "bare source", args: []string{"https://example.com/tau-plugin-x"}, want: pluginInstallFlags{source: "https://example.com/tau-plugin-x"}},
		{name: "with sha256 space", args: []string{"--sha256", "abc", "url"}, want: pluginInstallFlags{source: "url", sha256: "abc"}},
		{name: "with sha256 equals", args: []string{"--sha256=abc", "url"}, want: pluginInstallFlags{source: "url", sha256: "abc"}},
		{name: "with yes", args: []string{"--yes", "url"}, want: pluginInstallFlags{source: "url", yes: true}},
		{name: "with y", args: []string{"-y", "url"}, want: pluginInstallFlags{source: "url", yes: true}},
		{name: "missing source", args: []string{}, err: true},
		{name: "unknown flag", args: []string{"--bogus", "url"}, err: true},
		{name: "extra positional", args: []string{"url1", "url2"}, err: true},
		{name: "sha256 missing value", args: []string{"--sha256"}, err: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePluginInstallFlags(tc.args)
			if tc.err {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// --- derivePluginShortName -------------------------------------------------

func TestDerivePluginShortName(t *testing.T) {
	cases := []struct {
		name string
		r    resolvedSource
		want string
		err  bool
	}{
		{name: "url bare", r: resolvedSource{downloadURL: "https://x.com/tau-plugin-git"}, want: "git"},
		{name: "url with ext", r: resolvedSource{downloadURL: "https://x.com/tau-plugin-git.tgz"}, want: "git"},
		{name: "url tar.gz", r: resolvedSource{downloadURL: "https://x.com/tau-plugin-git.tar.gz"}, want: "git"},
		{name: "url exe", r: resolvedSource{downloadURL: "https://x.com/tau-plugin-git.exe"}, want: "git"},
		{name: "github asset", r: resolvedSource{downloadURL: "https://x.com/tau-plugin-git-linux-amd64", version: "v1"}, want: "git"},
		{name: "local path", r: resolvedSource{downloadURL: "/tmp/tau-plugin-lint"}, want: "lint"},
		{name: "bare prefix only", r: resolvedSource{downloadURL: "/tmp/tau-plugin-"}, err: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := derivePluginShortName(tc.r)
			if tc.err {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- resolveChecksum --------------------------------------------------------

func TestResolveChecksum_Explicit(t *testing.T) {
	sha, status, err := resolveChecksum(context.Background(), "https://x.com/f", "abc123")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if sha != "abc123" {
		t.Errorf("sha = %q", sha)
	}
	if !strings.Contains(status, "explicit") {
		t.Errorf("status = %q", status)
	}
}

func TestResolveChecksum_Sibling(t *testing.T) {
	want := sha256Hex([]byte("plugin bytes"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			fmt.Fprint(w, want+"  tau-plugin-x\n")
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sha, status, err := resolveChecksum(context.Background(), srv.URL+"/tau-plugin-x", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if sha != want {
		t.Errorf("sha = %q, want %q", sha, want)
	}
	if !strings.Contains(status, "verified") {
		t.Errorf("status = %q", status)
	}
}

func TestResolveChecksum_None(t *testing.T) {
	// Local path: no sibling fetch, returns empty.
	sha, status, err := resolveChecksum(context.Background(), "/tmp/tau-plugin-x", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if sha != "" {
		t.Errorf("sha = %q, want empty", sha)
	}
	if !strings.Contains(status, "unverified") {
		t.Errorf("status = %q", status)
	}
}

// --- resolveGitHubLatest ---------------------------------------------------

// withGithubAPIBase overrides the package-level githubAPIBase for the
// duration of the test, restoring it on cleanup. Lets resolveGitHubLatest
// be exercised against an httptest server without going to the network.
func withGithubAPIBase(t *testing.T, base string) {
	t.Helper()
	old := githubAPIBase
	githubAPIBase = base
	t.Cleanup(func() { githubAPIBase = old })
}

func TestResolveGitHubLatest_404_HasHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Mirror GitHub's actual 404 payload shape so the test asserts
		// the behavior against a realistic response.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found","documentation_url":"https://docs.github.com/rest/releases/releases#get-the-latest-release","status":"404"}`)
	}))
	defer srv.Close()
	withGithubAPIBase(t, srv.URL)

	_, err := resolveGitHubLatest(context.Background(), "taucentral/sdd")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	msg := err.Error()
	for want, why := range map[string]string{
		"taucentral/sdd":             "should name the repo shorthand so the user knows which input failed",
		"no published releases":      "should explain the most likely cause",
		runtime.GOOS:                 "should name the current GOOS so the user knows what asset name to publish",
		runtime.GOARCH:               "should name the current GOARCH for the same reason",
		"direct HTTPS URL or local":  "should offer the two non-GitHub workarounds",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q (%s); got:\n%s", want, why, msg)
		}
	}
	// The raw GitHub JSON body must NOT leak through — that was the whole
	// point of the special-case.
	if strings.Contains(msg, "documentation_url") {
		t.Errorf("error leaked raw GitHub JSON; got:\n%s", msg)
	}
}

func TestResolveGitHubLatest_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/releases/latest") {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Pick an asset name that matches the current GOOS/GOARCH so the
		// matcher selects it.
		assetName := fmt.Sprintf("tau-plugin-sdd-%s-%s", runtime.GOOS, runtime.GOARCH)
		assetURL := "http://download.test/" + assetName
		fmt.Fprintf(w, `{"tag_name":"v0.1.0","assets":[{"name":%q,"browser_download_url":%q}]}`, assetName, assetURL)
	}))
	defer srv.Close()
	withGithubAPIBase(t, srv.URL)

	r, err := resolveGitHubLatest(context.Background(), "taucentral/sdd")
	if err != nil {
		t.Fatalf("resolveGitHubLatest: %v", err)
	}
	if r.version != "v0.1.0" {
		t.Errorf("version = %q", r.version)
	}
	if want := "http://download.test/tau-plugin-sdd-" + runtime.GOOS + "-" + runtime.GOARCH; r.downloadURL != want {
		t.Errorf("downloadURL = %q, want %q", r.downloadURL, want)
	}
}

func TestResolveGitHubLatest_NoMatchingAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Release exists but the only asset is for a different OS.
		fmt.Fprintf(w, `{"tag_name":"v0.1.0","assets":[{"name":"tau-plugin-sdd-plan9-amd64","browser_download_url":"http://x/plan9"}]}`)
	}))
	defer srv.Close()
	withGithubAPIBase(t, srv.URL)

	_, err := resolveGitHubLatest(context.Background(), "taucentral/sdd")
	if err == nil {
		t.Fatalf("expected no-matching-asset error")
	}
	if !strings.Contains(err.Error(), "no asset matching") {
		t.Errorf("err = %v, want a no-matching-asset message", err)
	}
}

// --- fetchVerifyInstall -----------------------------------------------------

func TestFetchVerifyInstall_HTTPRaw(t *testing.T) {
	data := []byte("fake plugin binary")
	want := sha256Hex(data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(data)
	}))
	defer srv.Close()

	dir := t.TempDir()
	pluginsDir := filepath.Join(dir, "plugins")
	r := resolvedSource{downloadURL: srv.URL + "/tau-plugin-x", mediaType: "raw"}
	dstPath := pluginBinaryPath(pluginsDir, "x")

	actual, err := fetchVerifyInstall(context.Background(), r, dstPath, pluginsDir, want)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if actual != want {
		t.Errorf("actual = %q, want %q", actual, want)
	}
	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("dst content mismatch")
	}
}

func TestFetchVerifyInstall_LocalFile(t *testing.T) {
	data := []byte("local plugin binary")
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "tau-plugin-local")
	if err := os.WriteFile(srcPath, data, 0o700); err != nil {
		t.Fatal(err)
	}
	dstDir := t.TempDir()
	pluginsDir := filepath.Join(dstDir, "plugins")
	r := resolvedSource{downloadURL: srcPath, mediaType: "raw"}
	dstPath := pluginBinaryPath(pluginsDir, "local")

	_, err := fetchVerifyInstall(context.Background(), r, dstPath, pluginsDir, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("dst content mismatch")
	}
}

// TestFetchVerifyInstall_Tarball_PinShaMatchesBinary guards against a
// regression where the saved pin sha was the archive sha instead of the
// extracted binary sha. The pin must match what checksumStatusFor computes
// over the on-disk binary; otherwise `tau plugin list` always reports
// "mismatch" for tarball installs.
func TestFetchVerifyInstall_Tarball_PinShaMatchesBinary(t *testing.T) {
	// Build a .tar.gz holding a single tau-plugin-tarred entry.
	binaryBytes := []byte("the actual plugin binary")
	archiveDir := t.TempDir()
	archivePath := filepath.Join(archiveDir, "tau-plugin-tarred.tar.gz")
	if err := buildTarball(t, archivePath, "tau-plugin-tarred", binaryBytes); err != nil {
		t.Fatalf("build tarball: %v", err)
	}
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(archiveBytes)
	}))
	defer srv.Close()

	dir := t.TempDir()
	pluginsDir := filepath.Join(dir, "plugins")
	r := resolvedSource{downloadURL: srv.URL + "/tau-plugin-tarred.tar.gz", mediaType: "tarball"}
	dstPath := pluginBinaryPath(pluginsDir, "tarred")

	archiveSha := sha256Hex(archiveBytes)
	binarySha := sha256Hex(binaryBytes)
	if archiveSha == binarySha {
		t.Fatalf("test setup invariant failed: archive and binary shas match")
	}

	gotSha, err := fetchVerifyInstall(context.Background(), r, dstPath, pluginsDir, archiveSha)
	if err != nil {
		t.Fatalf("fetchVerifyInstall: %v", err)
	}
	if gotSha != binarySha {
		t.Errorf("returned sha = %q (archive), want %q (binary)", gotSha, binarySha)
	}
	// Re-deriving the on-disk sha must match what the pin would store.
	onDisk, err := sha256File(dstPath)
	if err != nil {
		t.Fatalf("sha256File: %v", err)
	}
	if onDisk != gotSha {
		t.Errorf("on-disk sha %q != returned sha %q", onDisk, gotSha)
	}
}

// buildTarball writes a .tar.gz at outPath containing one regular file
// named entryName with the supplied contents.
func buildTarball(t *testing.T, outPath, entryName string, contents []byte) error {
	t.Helper()
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	hdr := &tar.Header{
		Name:     entryName,
		Typeflag: tar.TypeReg,
		Size:     int64(len(contents)),
		Mode:     0o700,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := tw.Write(contents); err != nil {
		return err
	}
	return nil
}

func TestFetchVerifyInstall_ChecksumMismatch(t *testing.T) {
	data := []byte("plugin")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(data)
	}))
	defer srv.Close()

	dir := t.TempDir()
	pluginsDir := filepath.Join(dir, "plugins")
	r := resolvedSource{downloadURL: srv.URL, mediaType: "raw"}
	dstPath := pluginBinaryPath(pluginsDir, "x")

	_, err := fetchVerifyInstall(context.Background(), r, dstPath, pluginsDir, "bogus-sha")
	if !errors.Is(err, errChecksumMismatch) {
		t.Errorf("err = %v, want errChecksumMismatch", err)
	}
}

func TestFetchVerifyInstall_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	r := resolvedSource{downloadURL: srv.URL, mediaType: "raw"}
	_, err := fetchVerifyInstall(context.Background(), r, pluginBinaryPath(dir, "x"), dir, "")
	if err == nil || errors.Is(err, errChecksumMismatch) {
		t.Errorf("err = %v, want a non-checksum error", err)
	}
}

// --- Full install flow ------------------------------------------------------

// runPluginInstallForTest invokes runPluginInstall with stderr captured.
func runPluginInstallForTest(ctx context.Context, args Args, subArgs []string) (string, error) {
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	err := runPluginInstall(ctx, args, subArgs)
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String(), err
}

func TestRunPluginInstall_HTTPSWithYes(t *testing.T) {
	configDir := withConfigDir(t)
	data := []byte("plugin binary")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			fmt.Fprint(w, sha256Hex(data)+"  tau-plugin-x\n")
			return
		}
		w.Write(data)
	}))
	defer srv.Close()

	cwd := t.TempDir()
	args := newPluginTestArgs(cwd)
	_, err := runPluginInstallForTest(context.Background(), args, []string{srv.URL + "/tau-plugin-x", "--yes"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	// Verify the binary landed in <ConfigDir>/plugins (the global dir).
	binPath := filepath.Join(configDir, "plugins", "tau-plugin-x")
	if _, err := os.Stat(binPath); err != nil {
		t.Fatalf("binary not installed: %v", err)
	}

	// Verify the pin.
	pins, err := loadPluginPins(context.Background(), cwd)
	if err != nil {
		t.Fatalf("load pins: %v", err)
	}
	pin, ok := pins["x"]
	if !ok {
		t.Fatalf("pin not saved; pins = %+v", pins)
	}
	if pin.Sha256 != sha256Hex(data) {
		t.Errorf("pin sha256 = %q, want %q", pin.Sha256, sha256Hex(data))
	}
	if pin.Source != srv.URL+"/tau-plugin-x" {
		t.Errorf("pin source = %q", pin.Source)
	}
}

func TestRunPluginInstall_HTTPSChecksumMismatch(t *testing.T) {
	withConfigDir(t)
	data := []byte("plugin")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(data)
	}))
	defer srv.Close()

	cwd := t.TempDir()
	args := newPluginTestArgs(cwd)
	// Explicit --sha256 that won't match the downloaded bytes.
	_, err := runPluginInstallForTest(context.Background(), args, []string{srv.URL + "/tau-plugin-x", "--sha256", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "--yes"})
	if err == nil {
		t.Fatalf("expected checksum mismatch error")
	}
}

func TestRunPluginInstall_NoChecksumWarnsAndInstalls(t *testing.T) {
	withConfigDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("plugin"))
	}))
	defer srv.Close()

	cwd := t.TempDir()
	args := newPluginTestArgs(cwd)
	out, err := runPluginInstallForTest(context.Background(), args, []string{srv.URL + "/tau-plugin-x", "--yes"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(out, "unverified") {
		t.Errorf("expected 'unverified' in output; got:\n%s", out)
	}
}

func TestRunPluginInstall_LocalFile(t *testing.T) {
	configDir := withConfigDir(t)
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "tau-plugin-local")
	data := []byte("#!/bin/sh\necho local\n")
	if err := os.WriteFile(srcPath, data, 0o700); err != nil {
		t.Fatal(err)
	}

	cwd := t.TempDir()
	args := newPluginTestArgs(cwd)
	_, err := runPluginInstallForTest(context.Background(), args, []string{srcPath, "--yes"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	binPath := filepath.Join(configDir, "plugins", "tau-plugin-local")
	if _, err := os.Stat(binPath); err != nil {
		t.Fatalf("binary not installed: %v", err)
	}
}

func TestRunPluginInstall_Sha256FlagOverride(t *testing.T) {
	withConfigDir(t)
	data := []byte("plugin")
	want := sha256Hex(data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			fmt.Fprint(w, "bogus\n") // sibling is wrong
			return
		}
		w.Write(data)
	}))
	defer srv.Close()

	cwd := t.TempDir()
	args := newPluginTestArgs(cwd)
	// Explicit --sha256 must override the bogus sibling.
	_, err := runPluginInstallForTest(context.Background(), args, []string{srv.URL + "/tau-plugin-x", "--sha256", want, "--yes"})
	if err != nil {
		t.Fatalf("install with explicit sha: %v", err)
	}
}

func TestRunPluginInstall_NonTTYWithoutYes_Refuses(t *testing.T) {
	withConfigDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("plugin"))
	}))
	defer srv.Close()

	cwd := t.TempDir()
	args := newPluginTestArgs(cwd)
	_, err := runPluginInstallForTest(context.Background(), args, []string{srv.URL + "/tau-plugin-x"})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("err = %v, want a --yes refusal", err)
	}
}

func TestRunPluginInstall_Overwrite(t *testing.T) {
	configDir := withConfigDir(t)
	// Pre-install a plugin.
	data1 := []byte("v1")
	data2 := []byte("v2-longer-than-v1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(data2)
	}))
	defer srv.Close()

	pluginsDir := filepath.Join(configDir, "plugins")
	if err := os.MkdirAll(pluginsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := pluginBinaryPath(pluginsDir, "x")
	if err := os.WriteFile(existing, data1, 0o700); err != nil {
		t.Fatal(err)
	}

	cwd := t.TempDir()
	args := newPluginTestArgs(cwd)
	_, err := runPluginInstallForTest(context.Background(), args, []string{srv.URL + "/tau-plugin-x", "--yes"})
	if err != nil {
		t.Fatalf("overwrite install: %v", err)
	}
	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(data2) {
		t.Errorf("content was not overwritten")
	}
}

func TestRunPluginInstall_FetchFailure(t *testing.T) {
	withConfigDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cwd := t.TempDir()
	args := newPluginTestArgs(cwd)
	_, err := runPluginInstallForTest(context.Background(), args, []string{srv.URL + "/tau-plugin-x", "--yes"})
	if err == nil {
		t.Fatalf("expected error for 500")
	}
}

// --- List -------------------------------------------------------------------

func TestRunPluginList_ZeroPlugins(t *testing.T) {
	withConfigDir(t)
	cwd := t.TempDir()
	old := os.Stdout
	tmp, _ := os.CreateTemp(cwd, "out")
	os.Stdout = tmp
	err := runPluginList(context.Background(), newPluginTestArgs(cwd), nil)
	os.Stdout = old
	tmp.Close()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	b, _ := os.ReadFile(tmp.Name())
	output := string(b)
	if !strings.Contains(output, "SOURCE") {
		t.Errorf("missing header; got:\n%s", output)
	}
}

func TestRunPluginList_OneGlobalWithPin(t *testing.T) {
	withConfigDir(t)
	cwd := t.TempDir()

	// Install a minimal plugin binary.
	if testing.Short() {
		t.Skip("skipping plugin build test in -short mode")
	}
	installMinimalPluginIntoConfigDir(t)

	old := os.Stdout
	tmp, _ := os.CreateTemp(cwd, "out")
	os.Stdout = tmp
	err := runPluginList(context.Background(), newPluginTestArgs(cwd), nil)
	os.Stdout = old
	tmp.Close()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	b, _ := os.ReadFile(tmp.Name())
	output := string(b)
	if !strings.Contains(output, "minimal") {
		t.Errorf("output missing 'minimal'; got:\n%s", output)
	}
	if !strings.Contains(output, "manual") {
		t.Errorf("output missing 'manual' status; got:\n%s", output)
	}
}

// --- Remove -----------------------------------------------------------------

func TestRunPluginRemove_UnknownName(t *testing.T) {
	withConfigDir(t)
	cwd := t.TempDir()
	err := runPluginRemove(context.Background(), newPluginTestArgs(cwd), []string{"bogus", "--yes"})
	if err == nil {
		t.Fatalf("expected error for unknown name")
	}
}

func TestRunPluginRemove_NoArg(t *testing.T) {
	withConfigDir(t)
	cwd := t.TempDir()
	err := runPluginRemove(context.Background(), newPluginTestArgs(cwd), nil)
	if err == nil {
		t.Fatalf("expected error for missing arg")
	}
}

func TestRunPluginRemove_NonTTYWithoutYes_Refuses(t *testing.T) {
	withConfigDir(t)
	cwd := t.TempDir()
	if testing.Short() {
		t.Skip("skipping plugin build test in -short mode")
	}
	installMinimalPluginIntoConfigDir(t)
	err := runPluginRemove(context.Background(), newPluginTestArgs(cwd), []string{"minimal"})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("err = %v, want a --yes refusal", err)
	}
}

func TestRunPluginRemove_PinnedPlugin(t *testing.T) {
	configDir := withConfigDir(t)
	cwd := t.TempDir()
	if testing.Short() {
		t.Skip("skipping plugin build test in -short mode")
	}
	installMinimalPluginIntoConfigDir(t)

	// Save a pin.
	if err := savePluginPin(context.Background(), cwd, "minimal", tau.PluginPin{
		Source: "test",
		Sha256: "abc",
	}); err != nil {
		t.Fatalf("save pin: %v", err)
	}

	err := runPluginRemove(context.Background(), newPluginTestArgs(cwd), []string{"minimal", "--yes"})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}

	// Binary gone.
	binPath := filepath.Join(configDir, "plugins", "tau-plugin-minimal")
	if _, err := os.Stat(binPath); !os.IsNotExist(err) {
		t.Errorf("binary still exists after remove")
	}
	// Pin gone.
	pins, _ := loadPluginPins(context.Background(), cwd)
	if _, ok := pins["minimal"]; ok {
		t.Errorf("pin still exists after remove")
	}
}

func TestRunPluginRemove_ManualPlugin(t *testing.T) {
	configDir := withConfigDir(t)
	cwd := t.TempDir()
	if testing.Short() {
		t.Skip("skipping plugin build test in -short mode")
	}
	installMinimalPluginIntoConfigDir(t)

	// No pin saved — this is a "manual" install.
	err := runPluginRemove(context.Background(), newPluginTestArgs(cwd), []string{"minimal", "--yes"})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	binPath := filepath.Join(configDir, "plugins", "tau-plugin-minimal")
	if _, err := os.Stat(binPath); !os.IsNotExist(err) {
		t.Errorf("binary still exists")
	}
}

// --- Update -----------------------------------------------------------------

func TestRunPluginUpdate_ManualRefusal(t *testing.T) {
	withConfigDir(t)
	cwd := t.TempDir()
	// No pin saved.
	err := runPluginUpdate(context.Background(), newPluginTestArgs(cwd), []string{"bogus", "--yes"})
	if err == nil {
		t.Fatalf("expected error for unpinned plugin")
	}
	if !strings.Contains(err.Error(), "no recorded source") {
		t.Errorf("err = %v, want 'no recorded source'", err)
	}
}

func TestRunPluginUpdate_PinnedPlugin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short mode")
	}
	configDir := withConfigDir(t)
	cwd := t.TempDir()

	// Serve the plugin binary.
	data := []byte("updated plugin")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(data)
	}))
	defer srv.Close()

	// Save a pin pointing to the test server.
	if err := savePluginPin(context.Background(), cwd, "x", tau.PluginPin{
		Source: srv.URL + "/tau-plugin-x",
		Sha256: sha256Hex(data),
	}); err != nil {
		t.Fatalf("save pin: %v", err)
	}

	err := runPluginUpdate(context.Background(), newPluginTestArgs(cwd), []string{"x", "--yes"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(configDir, "plugins", "tau-plugin-x"))
	if err != nil {
		t.Fatalf("read installed: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("content mismatch after update")
	}
}

func TestRunPluginUpdate_ChecksumMismatchWithoutForce_Errors(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short mode")
	}
	withConfigDir(t)
	cwd := t.TempDir()

	data := []byte("new bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(data)
	}))
	defer srv.Close()

	// Pin has an OLD checksum that won't match the new bytes.
	if err := savePluginPin(context.Background(), cwd, "x", tau.PluginPin{
		Source: srv.URL + "/tau-plugin-x",
		Sha256: "old-sha-from-previous-install",
	}); err != nil {
		t.Fatalf("save pin: %v", err)
	}

	err := runPluginUpdate(context.Background(), newPluginTestArgs(cwd), []string{"x", "--yes"})
	if err == nil {
		t.Fatalf("expected checksum mismatch error")
	}
}

func TestRunPluginUpdate_ChecksumMismatchWithForce_Succeeds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short mode")
	}
	withConfigDir(t)
	cwd := t.TempDir()

	data := []byte("new bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(data)
	}))
	defer srv.Close()

	if err := savePluginPin(context.Background(), cwd, "x", tau.PluginPin{
		Source: srv.URL + "/tau-plugin-x",
		Sha256: "old-sha",
	}); err != nil {
		t.Fatalf("save pin: %v", err)
	}

	err := runPluginUpdate(context.Background(), newPluginTestArgs(cwd), []string{"x", "--force", "--yes"})
	if err != nil {
		t.Fatalf("update --force: %v", err)
	}

	// Pin should now have the actual sha of the new bytes.
	pins, _ := loadPluginPins(context.Background(), cwd)
	if pins["x"].Sha256 != sha256Hex(data) {
		t.Errorf("pin sha = %q, want %q", pins["x"].Sha256, sha256Hex(data))
	}
}

// --- Dispatch wiring --------------------------------------------------------

func TestDispatch_SubcommandPlugin_NoAction(t *testing.T) {
	err := Dispatch(rerunCtx, Args{Subcommand: "plugin"})
	if err == nil {
		t.Fatalf("expected error for plugin with no action")
	}
}

func TestDispatch_SubcommandPlugin_UnknownAction(t *testing.T) {
	err := Dispatch(rerunCtx, Args{Subcommand: "plugin", SubcommandArgs: []string{"bogus"}})
	if err == nil {
		t.Fatalf("expected error for unknown plugin action")
	}
}

// --- Helpers ----------------------------------------------------------------

// installMinimalPluginIntoConfigDir builds the minimalplugin test binary
// and places it in the global plugins dir (<ConfigDir>/plugins).
func installMinimalPluginIntoConfigDir(t *testing.T) {
	t.Helper()
	// The global plugins dir is <ConfigDir>/plugins, which is what PluginsDir
	// returns at index 0. This is NOT <AgentDir>/plugins.
	configDir := os.Getenv("TAU_CONFIG_DIR")
	if configDir == "" {
		t.Fatal("TAU_CONFIG_DIR not set")
	}
	pluginsDir := filepath.Join(configDir, "plugins")
	if err := os.MkdirAll(pluginsDir, 0o700); err != nil {
		t.Fatalf("mkdir plugins: %v", err)
	}
	binPath := filepath.Join(pluginsDir, "tau-plugin-minimal")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	cmd := exec.Command("go", "build", "-o="+binPath,
		"github.com/taucentral/tau/internal/plugins/testdata/minimalplugin")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build minimalplugin: %v\n%s", err, out)
	}
	_ = os.Chmod(binPath, 0o700)
}

