package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// rerunCtx is a cancelled-neutral context for tests that don't exercise
// cancellation.
var rerunCtx = context.Background()

func TestDispatch_HelpShortCircuit(t *testing.T) {
	// Capture stdout via swap; Dispatch writes to os.Stdout directly.
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	err = Dispatch(rerunCtx, Args{Help: true, BinaryVersion: "test-1"})
	w.Close()
	if err != nil {
		t.Fatalf("Dispatch(Help) returned error: %v", err)
	}
	var buf bytes.Buffer
	if _, copyErr := buf.ReadFrom(r); copyErr != nil {
		t.Fatalf("read pipe: %v", copyErr)
	}
	if !strings.Contains(buf.String(), "tau test-1") {
		t.Errorf("help output missing version string; got:\n%s", buf.String())
	}
}

func TestDispatch_VersionShortCircuit(t *testing.T) {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	err = Dispatch(rerunCtx, Args{Version: true, BinaryVersion: "v9.9.9"})
	w.Close()
	if err != nil {
		t.Fatalf("Dispatch(Version) returned error: %v", err)
	}
	var buf bytes.Buffer
	if _, copyErr := buf.ReadFrom(r); copyErr != nil {
		t.Fatalf("read pipe: %v", copyErr)
	}
	if !strings.Contains(buf.String(), "tau v9.9.9") {
		t.Errorf("version output missing; got:\n%s", buf.String())
	}
}

func TestDispatch_SubcommandConfig_Succeeds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", dir)
	// Capture stdout via temp file to avoid pipe races with the tabwriter.
	tmp, err := os.CreateTemp(dir, "out")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	old := os.Stdout
	os.Stdout = tmp
	err = Dispatch(rerunCtx, Args{Subcommand: "config"})
	os.Stdout = old
	tmp.Close()
	if err != nil {
		t.Errorf("config subcommand: err = %v, want nil", err)
	}
}

func TestDispatch_SubcommandUpdate_Succeeds(t *testing.T) {
	// Update writes to stderr; no error is expected (v1 stub).
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	err = Dispatch(rerunCtx, Args{Subcommand: "update"})
	_ = w.Close()
	os.Stderr = old
	if err != nil {
		t.Errorf("update subcommand: err = %v, want nil", err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	if !strings.Contains(buf.String(), "package manager") {
		t.Errorf("update output missing expected message; got:\n%s", buf.String())
	}
}

func TestDispatch_SubcommandUnknown(t *testing.T) {
	err := Dispatch(rerunCtx, Args{Subcommand: "bogus"})
	if err == nil || errors.Is(err, ErrNotImplemented) {
		t.Errorf("unknown subcommand: err = %v, want a non-ErrNotImplemented error", err)
	}
}

func TestDispatch_ListModels_Succeeds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", dir)
	tmp, err := os.CreateTemp(dir, "out")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	old := os.Stdout
	os.Stdout = tmp
	err = Dispatch(rerunCtx, Args{ListModels: true})
	os.Stdout = old
	tmp.Close()
	if err != nil {
		t.Errorf("list-models: err = %v, want nil", err)
	}
}

// TestDispatch_RPCMode_NoModel_Errors verifies the wire layer surfaces
// ErrNoModel when --rpc is requested with no model configured. The
// protocol-level RPC round-trip is exercised in pkg/tau/modes tests
// (where the stdin/stdout pipes can be injected cleanly without racing
// on the process's real os.Stdin / os.Stdout globals).
func TestDispatch_RPCMode_NoModel_Errors(t *testing.T) {
	dir := t.TempDir()
	err := Dispatch(rerunCtx, Args{
		RPC: true,
		Cwd: dir,
	})
	if !errors.Is(err, ErrNoModel) {
		t.Errorf("Dispatch(rpc, no model): err = %v, want ErrNoModel", err)
	}
}

// TestDispatch_PrintMode_FauxModel exercises the full print-mode path:
// wire layer + modes.RunPrint + faux provider. Sets the prompt via
// Positional and the faux model via Model so the wire layer can build
// a session without real credentials.
func TestDispatch_PrintMode_FauxModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TAU_FAUX_SCRIPT", "dispatch reply")
	tmp, err := os.CreateTemp(dir, "out")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	old := os.Stdout
	os.Stdout = tmp
	err = Dispatch(rerunCtx, Args{
		Print:      true,
		Model:      "faux",
		Cwd:        dir,
		Positional: []string{"hi"},
	})
	os.Stdout = old
	tmp.Close()
	if err != nil {
		t.Fatalf("Dispatch(print): %v", err)
	}
	b, readErr := os.ReadFile(tmp.Name())
	if readErr != nil {
		t.Fatalf("read tmp: %v", readErr)
	}
	if !strings.Contains(string(b), "dispatch reply") {
		t.Errorf("stdout = %q, want substring 'dispatch reply'", string(b))
	}
}

// TestDispatch_PrintMode_NoModel_Errors verifies the wire layer surfaces
// ErrNoModel when no model is configured.
func TestDispatch_PrintMode_NoModel_Errors(t *testing.T) {
	dir := t.TempDir()
	err := Dispatch(rerunCtx, Args{
		Print:      true,
		Cwd:        dir,
		Positional: []string{"hi"},
	})
	if !errors.Is(err, ErrNoModel) {
		t.Errorf("Dispatch(print, no model): err = %v, want ErrNoModel", err)
	}
}

// TestDispatch_InteractiveMode_WiresSession verifies that explicit
// Mode="interactive" enters runInteractive, which calls wireSession.
// With no model configured, wireSession returns ErrNoModel — proving
// the dispatch path reaches the wire layer instead of an unimplemented
// stub.
func TestDispatch_InteractiveMode_WiresSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", dir)
	err := Dispatch(rerunCtx, Args{Mode: "interactive", Cwd: dir})
	if err == nil {
		t.Fatal("interactive mode with no model: err = nil, want ErrNoModel")
	}
	if errors.Is(err, ErrNotImplemented) {
		t.Errorf("interactive mode: err = %v, want it to NOT be ErrNotImplemented", err)
	}
}

// TestDispatch_PrintTools_BuiltinsOnly verifies --print-tools lists
// built-in tools with source=builtin when no plugins are installed.
func TestDispatch_PrintTools_BuiltinsOnly(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)
	t.Setenv("TAU_AGENT_DIR", "")
	cwd := t.TempDir()
	tmp, err := os.CreateTemp(cwd, "out")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	old := os.Stdout
	os.Stdout = tmp
	err = Dispatch(rerunCtx, Args{
		PrintTools: true,
		Model:      "faux",
		Cwd:        cwd,
	})
	os.Stdout = old
	tmp.Close()
	if err != nil {
		t.Fatalf("Dispatch(print-tools): %v", err)
	}
	b, readErr := os.ReadFile(tmp.Name())
	if readErr != nil {
		t.Fatalf("read tmp: %v", readErr)
	}
	output := string(b)
	if !strings.Contains(output, "SOURCE") {
		t.Errorf("output missing header; got:\n%s", output)
	}
	if !strings.Contains(output, "builtin") {
		t.Errorf("output missing builtin source; got:\n%s", output)
	}
	// Every built-in tool should appear.
	for _, name := range []string{"read", "bash", "edit", "write", "grep", "find", "ls"} {
		if !strings.Contains(output, name) {
			t.Errorf("output missing built-in tool %q; got:\n%s", name, output)
		}
	}
	// No plugin source should appear.
	if strings.Contains(output, "minimal") {
		t.Errorf("output should not contain plugin entries; got:\n%s", output)
	}
}

// TestDispatch_PrintTools_WithPlugin verifies --print-tools lists both
// built-in tools (source=builtin) and plugin tools (source=<plugin
// short name>) when a plugin is installed.
func TestDispatch_PrintTools_WithPlugin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping plugin build test in -short mode")
	}
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)
	t.Setenv("TAU_AGENT_DIR", "")

	// Build and install minimalplugin into the global plugins dir.
	pluginsDir := filepath.Join(configDir, "plugins")
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		t.Fatalf("mkdir plugins: %v", err)
	}
	binPath := filepath.Join(pluginsDir, "tau-plugin-minimal")
	buildCmd := exec.Command("go", "build", "-o="+binPath,
		"github.com/taucentral/tau/internal/plugins/testdata/minimalplugin")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("go build minimalplugin: %v\n%s", err, out)
	}
	_ = os.Chmod(binPath, 0o755)

	cwd := t.TempDir()
	tmp, err := os.CreateTemp(cwd, "out")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	old := os.Stdout
	os.Stdout = tmp
	err = Dispatch(rerunCtx, Args{
		PrintTools: true,
		Model:      "faux",
		Cwd:        cwd,
	})
	os.Stdout = old
	tmp.Close()
	if err != nil {
		t.Fatalf("Dispatch(print-tools): %v", err)
	}
	b, readErr := os.ReadFile(tmp.Name())
	if readErr != nil {
		t.Fatalf("read tmp: %v", readErr)
	}
	output := string(b)
	// Builtins present.
	if !strings.Contains(output, "builtin") || !strings.Contains(output, "read") {
		t.Errorf("output missing builtins; got:\n%s", output)
	}
	// Plugin tools present.
	for _, name := range []string{"minimal.echo", "minimal.fail", "minimal.log"} {
		if !strings.Contains(output, name) {
			t.Errorf("output missing plugin tool %q; got:\n%s", name, output)
		}
	}
	// Plugin source label appears. tabwriter replaces the tab delimiters with
	// space padding, so we tokenize each line and compare the first field.
	lines := strings.Split(output, "\n")
	var sawMinimalSource bool
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "minimal" {
			sawMinimalSource = true
			break
		}
	}
	if !sawMinimalSource {
		t.Errorf("output missing plugin source label 'minimal'; got:\n%s", output)
	}
}

// TestDispatch_PrintTools_BrokenPlugin verifies --print-tools still
// prints built-in tools when a plugin fails to spawn. The broken plugin
// is discovered but never spawns; its tools don't appear, but the
// built-in rows do and the process does not panic.
func TestDispatch_PrintTools_BrokenPlugin(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TAU_CONFIG_DIR", configDir)
	t.Setenv("TAU_AGENT_DIR", "")

	// Write a broken plugin that exits 1 immediately.
	pluginsDir := filepath.Join(configDir, "plugins")
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		t.Fatalf("mkdir plugins: %v", err)
	}
	brokenPath := filepath.Join(pluginsDir, "tau-plugin-broken")
	if err := os.WriteFile(brokenPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write broken plugin: %v", err)
	}

	cwd := t.TempDir()
	tmp, err := os.CreateTemp(cwd, "out")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	old := os.Stdout
	os.Stdout = tmp
	// Suppress stderr so the spawn-error log doesn't clutter test output.
	oldErr := os.Stderr
	nullErr, _ := os.Open(os.DevNull)
	os.Stderr = nullErr
	err = Dispatch(rerunCtx, Args{
		PrintTools: true,
		Model:      "faux",
		Cwd:        cwd,
	})
	os.Stdout = old
	os.Stderr = oldErr
	if nullErr != nil {
		_ = nullErr.Close()
	}
	tmp.Close()
	if err != nil {
		t.Fatalf("Dispatch(print-tools): %v", err)
	}
	b, readErr := os.ReadFile(tmp.Name())
	if readErr != nil {
		t.Fatalf("read tmp: %v", readErr)
	}
	output := string(b)
	// Builtins still present.
	if !strings.Contains(output, "builtin") || !strings.Contains(output, "read") {
		t.Errorf("output missing builtins despite broken plugin; got:\n%s", output)
	}
	// Broken plugin tools must NOT appear.
	if strings.Contains(output, "broken") {
		t.Errorf("output should not contain broken plugin tools; got:\n%s", output)
	}
}
