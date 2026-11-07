package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
)

// profilers is the set of leading root profiling flags (currently cpu and heap).
type profilers struct {
	cpuPath  string
	memPath  string
	provided bool
}

// mergeProfilers folds a later occurrence of the profiling flags over an
// earlier one: the last path written for each flag wins, and a flag the later
// group did not mention keeps the earlier path.
func mergeProfilers(earlier, later profilers) profilers {
	merged := profilers{provided: earlier.provided || later.provided}
	merged.cpuPath = later.cpuPath
	if merged.cpuPath == "" {
		merged.cpuPath = earlier.cpuPath
	}
	merged.memPath = later.memPath
	if merged.memPath == "" {
		merged.memPath = earlier.memPath
	}
	return merged
}

// splitLeadingProfileFlags strips leading --cpuprofile <path> and
// --memprofile <path> (space or = form) from the root argument list. The last
// occurrence of each wins, as it does for --theme.
//
// These flags are root-only: they wrap the whole process (or the exec run when
// written before the subcommand), so unlike --add-dir they are consumed here
// and never re-synthesised into exec's own parser.
func splitLeadingProfileFlags(args []string) (profilers, []string, error) {
	var found profilers
	for len(args) > 0 {
		switch {
		case args[0] == "--cpuprofile":
			if len(args) < 2 {
				return profilers{}, nil, errors.New("--cpuprofile requires a file path")
			}
			value, err := profilePathValue("--cpuprofile", args[1])
			if err != nil {
				return profilers{}, nil, err
			}
			found.cpuPath = value
			found.provided = true
			args = args[2:]
		case strings.HasPrefix(args[0], "--cpuprofile="):
			value, err := profilePathValue("--cpuprofile", strings.TrimPrefix(args[0], "--cpuprofile="))
			if err != nil {
				return profilers{}, nil, err
			}
			found.cpuPath = value
			found.provided = true
			args = args[1:]
		case args[0] == "--memprofile":
			if len(args) < 2 {
				return profilers{}, nil, errors.New("--memprofile requires a file path")
			}
			value, err := profilePathValue("--memprofile", args[1])
			if err != nil {
				return profilers{}, nil, err
			}
			found.memPath = value
			found.provided = true
			args = args[2:]
		case strings.HasPrefix(args[0], "--memprofile="):
			value, err := profilePathValue("--memprofile", strings.TrimPrefix(args[0], "--memprofile="))
			if err != nil {
				return profilers{}, nil, err
			}
			found.memPath = value
			found.provided = true
			args = args[1:]
		default:
			return found, args, nil
		}
	}
	return found, args, nil
}

// profilePathValue validates one profile flag value the way --add-dir does:
// empty or flag-like values are a loud error, so a mistyped option is never
// taken as a path.
func profilePathValue(flag string, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" || strings.HasPrefix(value, "-") {
		return "", fmt.Errorf("%s requires a file path", flag)
	}
	return value, nil
}

// profileSession owns the running CPU profile (if any) and the paths to write
// when the run finishes.
type profileSession struct {
	cpuPath    string
	cpuFile    *os.File
	cpuStarted bool
	memPath    string
}

// startProfilers opens the profile files and begins CPU collection. Heap
// collection happens at finish, when the run has allocated its working set.
// An error here aborts the run: the operator asked for a profile, and silently
// proceeding without one would make every later "profiled" measurement a lie.
func startProfilers(p profilers, stderr io.Writer) (*profileSession, error) {
	session := &profileSession{cpuPath: p.cpuPath, memPath: p.memPath}
	if session.cpuPath == "" && session.memPath == "" {
		return session, nil
	}
	if session.cpuPath != "" {
		f, err := os.Create(session.cpuPath)
		if err != nil {
			return nil, fmt.Errorf("--cpuprofile: %w", err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			f.Close()
			return nil, fmt.Errorf("--cpuprofile: %w", err)
		}
		session.cpuFile = f
		session.cpuStarted = true
		_, _ = fmt.Fprintf(stderr, "[zero] cpu profile: %s\n", session.cpuPath)
	}
	if session.memPath != "" {
		_, _ = fmt.Fprintf(stderr, "[zero] heap profile will be written to %s at exit\n", session.memPath)
	}
	return session, nil
}

// finish stops CPU collection and writes the heap profile, reporting the first
// failure encountered. Like the crash-recovery defer it pairs with, it reports
// failures rather than pretending the requested artifact exists.
func (s *profileSession) finish() error {
	if s == nil || (s.cpuPath == "" && s.memPath == "") {
		return nil
	}
	var firstErr error
	if s.cpuStarted {
		pprof.StopCPUProfile()
		s.cpuStarted = false
		if err := s.cpuFile.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("--cpuprofile: close %s: %w", s.cpuPath, err)
		}
	}
	if s.memPath != "" {
		// Flush pending finalizers so the heap snapshot reflects reachable
		// memory the way `go test -memprofile` does.
		runtime.GC()
		f, err := os.Create(s.memPath)
		if err != nil {
			return joinProfileError(firstErr, fmt.Errorf("--memprofile: %w", err))
		}
		writeErr := pprof.WriteHeapProfile(f)
		closeErr := f.Close()
		if writeErr != nil {
			return joinProfileError(firstErr, fmt.Errorf("--memprofile: write %s: %w", s.memPath, writeErr))
		}
		if closeErr != nil {
			return joinProfileError(firstErr, fmt.Errorf("--memprofile: close %s: %w", s.memPath, closeErr))
		}
	}
	return firstErr
}

// reconfigure points the session at the merged flag set from a second root
// split (the --skip-permissions-unsafe path, where the flags may appear on
// either side of the subcommand flag). A changed CPU target is closed and
// reopened; only the flag-splitting preamble has been recorded into the
// abandoned file at this point.
func (s *profileSession) reconfigure(p profilers) error {
	if s == nil || p == (profilers{}) ||
		(p.cpuPath == s.cpuPath && p.memPath == s.memPath) {
		return nil
	}
	if p.cpuPath != s.cpuPath {
		if s.cpuStarted {
			pprof.StopCPUProfile()
			s.cpuStarted = false
			if err := s.cpuFile.Close(); err != nil {
				return fmt.Errorf("--cpuprofile: close %s: %w", s.cpuPath, err)
			}
			s.cpuFile = nil
		}
		s.cpuPath = ""
		if p.cpuPath != "" {
			f, err := os.Create(p.cpuPath)
			if err != nil {
				return fmt.Errorf("--cpuprofile: %w", err)
			}
			if err := pprof.StartCPUProfile(f); err != nil {
				f.Close()
				return fmt.Errorf("--cpuprofile: %w", err)
			}
			s.cpuPath = p.cpuPath
			s.cpuFile = f
			s.cpuStarted = true
		}
	}
	s.memPath = p.memPath
	return nil
}

func joinProfileError(existing, next error) error {
	if existing == nil {
		return next
	}
	return fmt.Errorf("%v; %v", existing, next)
}
