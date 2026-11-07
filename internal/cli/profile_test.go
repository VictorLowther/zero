package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/tui"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// readProfile opens a written profile file and returns its decompressed bytes.
// The pprof format is gzip-compressed protobuf, so a file that gunzips to a
// non-empty payload is a real profile, not a zero-byte placeholder left behind
// by a start that never finished.
func readProfile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read profile %s: %v", path, err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("profile %s is not gzip (%d bytes): %v", path, len(raw), err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decompress profile %s: %v", path, err)
	}
	if len(body) == 0 {
		t.Fatalf("profile %s decompressed to zero bytes", path)
	}
	return body
}

// runCaptureProfileTUI runs the root command with the TUI launch intercepted and
// the process-wide profilers exercised, returning the captured options.
func runCaptureProfileTUI(t *testing.T, args ...string) {
	t.Helper()
	isolateCLIUserState(t)
	var stdout, stderr bytes.Buffer
	workspace := t.TempDir()
	exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return workspace, nil },
		runTUI: func(context.Context, tui.Options) int {
			return exitSuccess
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
}

// THE FLAGS PROFILE THE WHOLE RUN. `zero --cpuprofile cpu.out --memprofile
// heap.out` must produce both artifacts on the way out even though the TUI
// itself is the workload; the session started at the root wraps every
// subcommand, which is why these flags are root-level and never forwarded into
// exec's parser.
func TestRootProfileFlagsWriteBothArtifacts(t *testing.T) {
	dir := t.TempDir()
	cpuPath := filepath.Join(dir, "cpu.out")
	memPath := filepath.Join(dir, "heap.out")
	runCaptureProfileTUI(t, "--cpuprofile", cpuPath, "--memprofile", memPath)
	cpu := readProfile(t, cpuPath)
	if !bytes.Contains(cpu, []byte("cpu")) {
		t.Errorf("cpu profile string table does not name the cpu profile type: %q", cpu[:min(len(cpu), 64)])
	}
	// A heap profile's string table carries its sample units ("bytes" and
	// "objects"), not the word "heap", so match on what the format guarantees.
	mem := readProfile(t, memPath)
	if !bytes.Contains(mem, []byte("bytes")) || !bytes.Contains(mem, []byte("objects")) {
		t.Errorf("heap profile string table lacks bytes/objects units: %q", mem[:min(len(mem), 64)])
	}
}

// EQUALS FORM AND MIXED ORDERING. The flags compose with the other root flags
// in any written order, exactly like --theme and --add-dir, and the = form is
// accepted alongside the space form.
func TestProfileFlagsComposeWithOtherRootFlags(t *testing.T) {
	dir := t.TempDir()
	cpuPath := filepath.Join(dir, "cpu.out")
	memPath := filepath.Join(dir, "heap.out")
	runCaptureProfileTUI(t, "--allow-escalation", "--cpuprofile="+cpuPath, "--theme", "auto", "--memprofile", memPath)
	readProfile(t, cpuPath)
	readProfile(t, memPath)
}

// THE LAST OCCURRENCE WINS ACROSS --skip-permissions-unsafe. Root flags may sit
// on either side of the flag, so the merged target set has to reach the session.
// The heap profile is written only at exit, so the swap is race-free proof the
// session was redirected: the file named after the flag gets the profile and
// the one named before it is never written. The CPU flag exercises the same
// merge path through its reopen.
func TestProfileFlagsAroundSkipPermissionsUnsafe(t *testing.T) {
	dir := t.TempDir()
	memBefore := filepath.Join(dir, "mem-before.out")
	memAfter := filepath.Join(dir, "mem-after.out")
	cpuBefore := filepath.Join(dir, "cpu-before.out")
	cpuAfter := filepath.Join(dir, "cpu-after.out")
	runCaptureProfileTUI(t,
		"--memprofile", memBefore, "--cpuprofile", cpuBefore,
		"--skip-permissions-unsafe",
		"--memprofile", memAfter, "--cpuprofile", cpuAfter,
	)
	readProfile(t, memAfter)
	if _, err := os.Stat(memBefore); !os.IsNotExist(err) {
		t.Errorf("%s was written; the last --memprofile occurrence should win", memBefore)
	}
	readProfile(t, cpuAfter)
}

// THE FLAG WRAPS EXEC TOO, WITHOUT BEING FORWARDED. Because the root consumes
// the profiling flags before dispatch, exec's parser never sees them and cannot
// reject them as unknown; the run still produces its profile.
func TestProfileFlagsWrapExecWithoutReachingItsParser(t *testing.T) {
	isolateCLIUserState(t)
	dir := t.TempDir()
	memPath := filepath.Join(dir, "heap.out")
	var stdout, stderr bytes.Buffer
	workspace := t.TempDir()
	provider := &toolListingProvider{}
	deps := fillAppDeps(appDeps{
		getwd: func() (string, error) { return workspace, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return provider, nil
		},
		newSandboxStore: func() (*sandbox.GrantStore, error) {
			return sandbox.NewGrantStore(sandbox.StoreOptions{FilePath: filepath.Join(dir, "sandbox-grants.json")})
		},
	})
	exitCode := runWithDeps([]string{"--memprofile", memPath, "exec", "say hi"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exit = %d, stderr = %s", exitCode, stderr.String())
	}
	readProfile(t, memPath)
}

// A BAD PATH IS A LOUD ERROR AND NO RUN. A mistyped value (missing or
// flag-like) must fail argument parsing, not silently skip profiling or treat
// the next flag as the path.
func TestProfileFlagsRejectBadValues(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
		want string
	}{
		{"cpu missing value", []string{"--cpuprofile"}, "--cpuprofile requires a file path"},
		{"cpu empty equals form", []string{"--cpuprofile="}, "--cpuprofile requires a file path"},
		{"cpu flag-like value", []string{"--cpuprofile", "--theme", "auto", "version"}, "--cpuprofile requires a file path"},
		{"mem missing value", []string{"--memprofile"}, "--memprofile requires a file path"},
		{"mem flag-like value", []string{"--memprofile", "-p", "hi"}, "--memprofile requires a file path"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exitCode := runWithDeps(testCase.args, &stdout, &stderr, appDeps{
				getwd:  func() (string, error) { return t.TempDir(), nil },
				runTUI: func(context.Context, tui.Options) int { return exitSuccess },
			})
			if exitCode == exitSuccess {
				t.Fatalf("exit = %d: bad value was accepted", exitCode)
			}
			if !strings.Contains(stderr.String(), testCase.want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), testCase.want)
			}
		})
	}
}

// A PROFILE THAT CANNOT BE WRITTEN IS NOT A SUCCESS. The heap file is opened
// only at exit, so an unwritable target must surface as an error and a nonzero
// exit rather than a silent no-profile run.
func TestUnwritableHeapProfileFailsTheRun(t *testing.T) {
	isolateCLIUserState(t)
	missing := filepath.Join(t.TempDir(), "no-such-dir", "heap.out")
	var stdout, stderr bytes.Buffer
	workspace := t.TempDir()
	exitCode := runWithDeps([]string{"--memprofile", missing}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return workspace, nil },
		runTUI: func(context.Context, tui.Options) int {
			return exitSuccess
		},
	})
	if exitCode == exitSuccess {
		t.Fatal("exit = 0: unwritable heap profile was silently ignored")
	}
	if !strings.Contains(stderr.String(), "--memprofile") {
		t.Fatalf("stderr = %q, want it to name --memprofile", stderr.String())
	}
}

// NO FLAGS, NO FILES, NO OVERHEAD. Without the flags the run must not create
// anything or print profile lines.
func TestNoProfileFlagsWriteNothing(t *testing.T) {
	isolateCLIUserState(t)
	var stdout, stderr bytes.Buffer
	workspace := t.TempDir()
	exitCode := runWithDeps(nil, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return workspace, nil },
		runTUI: func(context.Context, tui.Options) int {
			return exitSuccess
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("exit = %d stderr = %s", exitCode, stderr.String())
	}
	if strings.Contains(stderr.String(), "profile") {
		t.Fatalf("stderr mentions profiling without the flags: %s", stderr.String())
	}
}

// THE SPLITTER OWNS ONLY ITS FLAGS. Leading tokens it does not claim stop the
// split untouched, so subcommands and prompts are never eaten as paths.
func TestSplitLeadingProfileFlagsStopsAtForeignTokens(t *testing.T) {
	dir := t.TempDir()
	cpu := filepath.Join(dir, "cpu.out")
	found, rest, err := splitLeadingProfileFlags([]string{
		"--cpuprofile", cpu, "exec", "--cpuprofile", "not-mine",
	})
	if err != nil {
		t.Fatal(err)
	}
	if found.cpuPath != cpu || found.memPath != "" || !found.provided {
		t.Fatalf("found = %+v", found)
	}
	if len(rest) != 3 || rest[0] != "exec" {
		t.Fatalf("rest = %v, want the exec args untouched", rest)
	}
}
