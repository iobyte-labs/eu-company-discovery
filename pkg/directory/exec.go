package directory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
	"github.com/iobyte-labs/eu-company-discovery/pkg/normalize"
)

// ExecName is the adapter name for an external adapter run as a child process.
const ExecName = "exec"

// AllowlistEnv names the environment variable holding the commands this engine may run as
// an external adapter.
//
// The allowlist is deliberately *not* in the association YAML. Configuration in this
// repository decides what to read, never what to execute: a source file that could name its
// own interpreter would make "add a directory" and "run a program on the crawler" the same
// act. The operator who runs the engine authorises the binary, in their environment, once.
//
// The value is a list of absolute paths, separated by [os.PathListSeparator]. An entry that
// is a file allows exactly that file; an entry that is a directory allows the executables
// directly inside it.
const AllowlistEnv = "EUCD_EXEC_ALLOW"

// Exec defaults. Each one bounds what a third-party adapter can do to a run.
const (
	// DefaultExecTimeout bounds one external adapter's whole execution.
	DefaultExecTimeout = 5 * time.Minute
	// DefaultExecMaxOutput caps what the engine will read from a child's stdout.
	DefaultExecMaxOutput int64 = 16 << 20 // 16 MiB
	// execMaxStderr is how much of a child's stderr is kept for the error message.
	execMaxStderr int64 = 8 << 10
)

func init() { Register(ExecName, func() Adapter { return ExecAdapter{} }) }

// ExecAdapter runs an external program and reads [model.DirectoryRecord] values from its
// stdout, one JSON object per line.
//
// This is what keeps a Go engine from being a closed shop. A directory that needs a real
// parser — a PDF member list, a paginated JSON API with its own auth, a national registry
// with a bespoke export — can be read by a program written in any language, released in its
// own repository, with its own tests, and plugged in here by configuration.
//
// The protocol is small on purpose:
//
//   - stdin receives one JSON object: {"association": {...}, "limits": {...}}. The
//     association carries its own `settings`, so an external adapter is configured the same
//     way a built-in one is.
//   - stdout is newline-delimited JSON, one [model.DirectoryRecord] per line. Blank lines
//     are ignored. A line that will not parse becomes a warning, not a dead run — unless
//     nothing at all parsed, which is an error.
//   - stderr is for diagnostics. It is read, bounded, and included in an error message.
//   - a non-zero exit status is a failure, unless records were produced, in which case it
//     is reported as a warning alongside them.
//
// Configuration:
//
//	adapter: exec
//	access_method: api          # whatever the external adapter actually does
//	directory_url: https://registry.example.org/members
//	settings:
//	  command: /opt/eucd-adapters/registry-reader   # absolute, and on the allowlist
//	  args: ["--country", "fr"]                     # optional, passed as argv
//	  timeout: 2m                                   # optional
//	  max_output_bytes: 4194304                     # optional
//
// What it will not do: run a shell. The command is executed with
// [exec.CommandContext] and an argv slice, so nothing in configuration is ever interpreted
// as shell syntax, and no quoting mistake can turn an argument into a command. The child
// also gets a scrubbed environment, so a credential in the operator's shell does not leak
// into a third-party program.
type ExecAdapter struct{}

// Name implements [Adapter].
func (ExecAdapter) Name() string { return ExecName }

// ExecInput is the JSON object written to an external adapter's stdin.
type ExecInput struct {
	// Protocol lets a future change be recognised by an adapter that predates it.
	Protocol    string            `json:"protocol"`
	Association model.Association `json:"association"`
	Limits      model.CrawlLimits `json:"limits"`
	Engine      map[string]string `json:"engine,omitempty"`
}

// ExecProtocolVersion is the value of ExecInput.Protocol.
const ExecProtocolVersion = "eucd.exec/1"

// Discover runs the configured program and reads its records.
func (ExecAdapter) Discover(ctx context.Context, req Request) (Result, error) {
	var result Result

	command, args, err := execCommand(req.Association)
	if err != nil {
		return result, err
	}
	timeout := DefaultExecTimeout
	if req.Limits.Timeout > 0 {
		timeout = req.Limits.Timeout
	}
	if configured := SettingString(req.Association.Settings, "timeout", ""); configured != "" {
		parsed, parseErr := time.ParseDuration(configured)
		if parseErr != nil {
			return result, fmt.Errorf("association %q: settings.timeout %q is not a duration "+
				"(try \"2m\")", req.Association.Slug, configured)
		}
		timeout = parsed
	}
	maxOutput := DefaultExecMaxOutput
	if configured := SettingInt(req.Association.Settings, "max_output_bytes", 0); configured > 0 {
		maxOutput = int64(configured)
	}

	input, err := json.Marshal(ExecInput{
		Protocol:    ExecProtocolVersion,
		Association: req.Association,
		Limits:      req.Limits,
		Engine:      map[string]string{"adapter": ExecName},
	})
	if err != nil {
		return result, fmt.Errorf("association %q: cannot encode the adapter input: %w",
			req.Association.Slug, err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// No shell. argv is passed through as given, so nothing here is parsed as shell syntax.
	cmd := exec.CommandContext(runCtx, command, args...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = execEnvironment()
	// Output goes into bounded buffers rather than being streamed out of a pipe we read
	// ourselves. That is what lets WaitDelay do its job: a child that spawns a grandchild
	// holding stdout open cannot hold the run past its timeout, because Wait closes the
	// pipes WaitDelay after the context is done instead of waiting for the last writer.
	stdout := &boundedWriter{limit: maxOutput}
	stderrBuffer := &boundedWriter{limit: execMaxStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderrBuffer
	cmd.WaitDelay = 2 * time.Second

	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("association %q: cannot run %s: %w",
			req.Association.Slug, command, err)
	}
	waitErr := cmd.Wait()

	records, warnings, readErr := readExecRecords(
		bytes.NewReader(stdout.Bytes()), maxOutput, req.Association)
	result.Records = records
	result.Warnings = warnings
	if stdout.overflowed {
		readErr = fmt.Errorf("%w: the adapter wrote more than the %d-byte cap",
			ErrOutputTooLarge, maxOutput)
	}
	stderr := strings.TrimSpace(string(stderrBuffer.Bytes()))

	if req.Limits.MaxRecords > 0 && len(result.Records) > req.Limits.MaxRecords {
		result.Records = result.Records[:req.Limits.MaxRecords]
		result.Truncated = true
	}

	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		if len(result.Records) == 0 {
			return result, fmt.Errorf("association %q: %s did not finish within %s",
				req.Association.Slug, command, timeout)
		}
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"%s did not finish within %s; keeping the %d record(s) it had already written",
			command, timeout, len(result.Records)))
		result.Truncated = true
	case readErr != nil && len(result.Records) == 0:
		return result, fmt.Errorf("association %q: reading %s: %w",
			req.Association.Slug, command, readErr)
	case readErr != nil:
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("stopped reading %s: %v", command, readErr))
		result.Truncated = true
	case waitErr != nil && len(result.Records) == 0:
		return result, fmt.Errorf("association %q: %s failed: %w%s",
			req.Association.Slug, command, waitErr, formatStderr(stderr))
	case waitErr != nil:
		// Some records arrived and then it failed. Partial success is reported, not
		// discarded, and not mistaken for a complete read.
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"%s exited with an error after producing %d record(s): %v%s",
			command, len(result.Records), waitErr, formatStderr(stderr)))
		result.Truncated = true
	}

	if len(result.Records) == 0 && len(result.Warnings) > 0 {
		// Every line was unusable. A run that produced nothing but warnings has not
		// succeeded, whatever the exit status said.
		return result, fmt.Errorf("association %q: %s produced no usable records: %s",
			req.Association.Slug, command, strings.Join(result.Warnings, "; "))
	}
	if stderr != "" && req.Logger != nil {
		req.Logger.Info("external adapter diagnostics",
			"adapter", command, "stderr_bytes", len(stderr))
	}
	return result, nil
}

// ErrOutputTooLarge is reported when an external adapter writes more than its cap.
var ErrOutputTooLarge = errors.New("adapter output is larger than the configured cap")

// boundedWriter keeps the first limit bytes written to it and then refuses the rest.
//
// Refusing rather than discarding is deliberate: the write error stops os/exec's copy, the
// pipe closes, and a runaway child is told to stop instead of being allowed to produce
// output nobody will read until its timeout expires.
type boundedWriter struct {
	limit      int64
	buffer     bytes.Buffer
	overflowed bool
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	room := w.limit - int64(w.buffer.Len())
	if room <= 0 {
		w.overflowed = true
		return 0, ErrOutputTooLarge
	}
	if int64(len(p)) > room {
		written, _ := w.buffer.Write(p[:room])
		w.overflowed = true
		return written, ErrOutputTooLarge
	}
	return w.buffer.Write(p)
}

func (w *boundedWriter) Bytes() []byte { return w.buffer.Bytes() }

// readExecRecords reads newline-delimited JSON records, bounded by maxOutput.
func readExecRecords(
	stdout io.Reader, maxOutput int64, association model.Association,
) ([]model.DirectoryRecord, []string, error) {
	var (
		records  []model.DirectoryRecord
		warnings []string
	)
	// One line of JSON for one company is small; a megabyte-long line is a sign that the
	// output is not what the protocol says it is.
	const maxLine = 1 << 20
	scanner := bufio.NewScanner(io.LimitReader(stdout, maxOutput))
	scanner.Buffer(make([]byte, 0, 64<<10), maxLine)

	now := time.Now().UTC()
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record model.DirectoryRecord
		decoder := json.NewDecoder(strings.NewReader(line))
		// An unknown field means the adapter and the engine disagree about the protocol.
		// Saying so is more useful than silently dropping the field.
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil {
			warnings = append(warnings, fmt.Sprintf("stdout line %d is not a directory record: %v",
				lineNumber, err))
			continue
		}
		if record.Country == "" {
			record.Country = association.Country
		}
		if record.SourceURL == "" {
			record.SourceURL = association.DirectoryURL
		}
		if record.RetrievedAt.IsZero() {
			record.RetrievedAt = now
		}
		record.RawName = normalize.DisplayName(record.RawName)
		if record.Website != "" && normalize.URL(record.Website) == "" {
			warnings = append(warnings, fmt.Sprintf("stdout line %d: website %q is not an http(s) "+
				"url; dropped", lineNumber, record.Website))
			record.Website = ""
		}
		if err := record.Validate(); err != nil {
			warnings = append(warnings, fmt.Sprintf("stdout line %d: %v", lineNumber, err))
			continue
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return records, warnings, err
	}
	return records, warnings, nil
}

// execCommand resolves and authorises the program an association names.
func execCommand(association model.Association) (string, []string, error) {
	command := strings.TrimSpace(SettingString(association.Settings, "command", ""))
	if command == "" {
		return "", nil, fmt.Errorf("association %q: the %s adapter needs settings.command",
			association.Slug, ExecName)
	}
	args, err := execArgs(association)
	if err != nil {
		return "", nil, err
	}

	allowed := allowlist()
	if len(allowed) == 0 {
		return "", nil, fmt.Errorf(
			"association %q wants to run %q, but no external adapter is authorised: set %s to a "+
				"%c-separated list of absolute paths to the programs (or directories of programs) "+
				"this engine may run",
			association.Slug, command, AllowlistEnv, os.PathListSeparator)
	}
	if !filepath.IsAbs(command) {
		resolved, ok := resolveInAllowlist(command, allowed)
		if !ok {
			return "", nil, fmt.Errorf(
				"association %q: settings.command %q is not an absolute path and is not a program "+
					"in %s; a relative command would resolve against whatever directory the engine "+
					"happens to be run from",
				association.Slug, command, AllowlistEnv)
		}
		command = resolved
	}

	real, err := filepath.EvalSymlinks(command)
	if err != nil {
		return "", nil, fmt.Errorf("association %q: cannot resolve %q: %w",
			association.Slug, command, err)
	}
	// The comparison is between fully resolved paths on both sides. Resolving only one side
	// would refuse ordinary setups where a parent directory is a symlink (/var on macOS,
	// /home on many Linux installs). Resolving both keeps the property that matters: a
	// symlink *inside* an allowed directory is not a way out of it, because what it points
	// at resolves somewhere that is not allowed.
	if !isAllowed(real, resolveAll(allowed)) {
		return "", nil, fmt.Errorf(
			"association %q: %q is not in %s, so it will not be run. Add it deliberately, with "+
				"the same care as installing it",
			association.Slug, command, AllowlistEnv)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", nil, fmt.Errorf("association %q: cannot stat %q: %w",
			association.Slug, command, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", nil, fmt.Errorf("association %q: %q is not an executable file",
			association.Slug, command)
	}
	return command, args, nil
}

func execArgs(association model.Association) ([]string, error) {
	raw, present := association.Settings["args"]
	if !present {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("association %q: settings.args must be a list of strings",
			association.Slug)
	}
	args := make([]string, 0, len(list))
	for index, item := range list {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("association %q: settings.args[%d] is not a string",
				association.Slug, index)
		}
		args = append(args, text)
	}
	return args, nil
}

// allowlist reads the authorised commands from the environment.
func allowlist() []string {
	var out []string
	for _, entry := range filepath.SplitList(os.Getenv(AllowlistEnv)) {
		entry = strings.TrimSpace(entry)
		if entry == "" || !filepath.IsAbs(entry) {
			continue // a relative allowlist entry authorises nothing in particular
		}
		out = append(out, filepath.Clean(entry))
	}
	return out
}

// resolveAll resolves symlinks in every allowlist entry, keeping an entry that cannot be
// resolved as it was written so that a missing entry is never silently widened.
func resolveAll(allowed []string) []string {
	out := make([]string, 0, len(allowed))
	for _, entry := range allowed {
		if resolved, err := filepath.EvalSymlinks(entry); err == nil {
			out = append(out, resolved)
			continue
		}
		out = append(out, entry)
	}
	return out
}

// isAllowed reports whether path is an allowed file, or sits directly inside an allowed
// directory. "Directly inside" rather than "underneath", so allowing one directory of
// adapters does not allow everything in the tree below it.
func isAllowed(path string, allowed []string) bool {
	path = filepath.Clean(path)
	for _, entry := range allowed {
		if path == entry {
			return true
		}
		if filepath.Dir(path) == entry {
			if info, err := os.Stat(entry); err == nil && info.IsDir() {
				return true
			}
		}
	}
	return false
}

// resolveInAllowlist finds a bare command name among the allowed entries, so an operator
// can authorise /opt/eucd-adapters and a source can then name just "registry-reader".
func resolveInAllowlist(name string, allowed []string) (string, bool) {
	if strings.ContainsRune(name, filepath.Separator) {
		return "", false
	}
	for _, entry := range allowed {
		info, err := os.Stat(entry)
		if err != nil {
			continue
		}
		if info.IsDir() {
			candidate := filepath.Join(entry, name)
			if stat, err := os.Stat(candidate); err == nil && !stat.IsDir() {
				return candidate, true
			}
			continue
		}
		if filepath.Base(entry) == name {
			return entry, true
		}
	}
	return "", false
}

// execEnvironment is the child's environment: enough to run, and nothing of the operator's.
//
// A third-party adapter has no business reading the engine's database path, an API key that
// happens to be exported, or a cloud credential. PATH and the locale are kept because a
// program that cannot find its own interpreter or write UTF-8 is not useful.
func execEnvironment() []string {
	keep := []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "SSL_CERT_FILE", "SSL_CERT_DIR"}
	env := make([]string, 0, len(keep)+2)
	for _, name := range keep {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	env = append(env, "EUCD_EXEC_PROTOCOL="+ExecProtocolVersion)
	return env
}

func formatStderr(stderr string) string {
	if stderr == "" {
		return ""
	}
	const limit = 500
	if len(stderr) > limit {
		stderr = stderr[:limit] + "…"
	}
	return "; stderr: " + stderr
}
