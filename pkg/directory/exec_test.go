package directory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

// writeAdapter puts a shell script on disk and authorises it, which is what an operator
// does for a real external adapter. Nothing in configuration can authorise itself.
func writeAdapter(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the external-adapter tests run a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "adapter.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatalf("write the test adapter: %v", err)
	}
	t.Setenv(AllowlistEnv, path)
	return path
}

func execAssociation(settings map[string]any) model.Association {
	return model.Association{
		Slug:         "fr_registry_exec",
		Name:         "A registry read by an external adapter",
		Country:      "fr",
		Sectors:      []string{"pharma"},
		DirectoryURL: "https://registry.example.org/members",
		Adapter:      ExecName,
		AccessMethod: model.AccessAPI,
		Settings:     settings,
	}
}

const twoGoodRecords = `cat > /dev/null
echo '{"raw_name":"Exemple Externe SAS","website":"https://exemple-externe.example.org",` +
	`"source_url":"https://registry.example.org/members","retrieved_at":"2026-03-01T09:00:00Z"}'
echo ''
echo '{"raw_name":"Exemple Externe Deux SA","source_url":"https://registry.example.org/members"}'
`

func TestExecReadsRecordsFromStdout(t *testing.T) {
	command := writeAdapter(t, twoGoodRecords)
	result, err := ExecAdapter{}.Discover(context.Background(), Request{
		Association: execAssociation(map[string]any{"command": command}),
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Records) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(result.Records), result)
	}
	if result.Records[0].RawName != "Exemple Externe SAS" {
		t.Errorf("name = %q", result.Records[0].RawName)
	}
	// The engine fills in what the adapter left out, so a minimal external adapter is
	// still a well-formed source.
	second := result.Records[1]
	if second.Country != "fr" {
		t.Errorf("country = %q, want the association's", second.Country)
	}
	if second.RetrievedAt.IsZero() {
		t.Error("RetrievedAt is zero; a record with no time has no provenance")
	}
	if len(result.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", result.Warnings)
	}
}

func TestExecPassesTheAssociationOnStdin(t *testing.T) {
	// An external adapter is configured exactly as a built-in one is: it is handed the
	// association, settings included.
	// The adapter writes what it was given to the path it was passed as an argument, which
	// also exercises argv being handed through untouched.
	command := writeAdapter(t, `
cat > "$1"
echo '{"raw_name":"Exemple Externe SAS","source_url":"https://registry.example.org/members"}'
`)
	inputPath := filepath.Join(t.TempDir(), "input.json")

	result, err := ExecAdapter{}.Discover(context.Background(), Request{
		Association: execAssociation(map[string]any{
			"command": command,
			"args":    []any{inputPath},
		}),
		Limits: model.CrawlLimits{MaxRecords: 10},
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(result.Records))
	}

	raw, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatalf("the adapter was not given its input: %v", err)
	}
	var input ExecInput
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatalf("stdin was not the documented JSON: %v (%s)", err, raw)
	}
	if input.Protocol != ExecProtocolVersion {
		t.Errorf("protocol = %q, want %q", input.Protocol, ExecProtocolVersion)
	}
	if input.Association.Slug != "fr_registry_exec" {
		t.Errorf("association = %+v", input.Association)
	}
	if input.Limits.MaxRecords != 10 {
		t.Errorf("limits = %+v, want the run's limits", input.Limits)
	}
}

func TestExecReportsMalformedJSONAsWarningsAndKeepsTheRest(t *testing.T) {
	command := writeAdapter(t, `cat > /dev/null
echo 'this is not json at all'
echo '{"raw_name":"Exemple Externe SAS","source_url":"https://registry.example.org/members"}'
echo '{"raw_name":"Exemple Tronqué SAS","source_url":'
echo '{"unexpected_field":"x","raw_name":"Exemple Inconnu SAS","source_url":"https://registry.example.org/members"}'
echo '{"website":"https://no-name.example.org","source_url":"https://registry.example.org/members"}'
`)
	result, err := ExecAdapter{}.Discover(context.Background(), Request{
		Association: execAssociation(map[string]any{"command": command}),
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("got %d records, want the one good line: %+v", len(result.Records), names(result))
	}
	// Four bad lines: junk, truncated JSON, an unknown field (the adapter and the engine
	// disagree about the protocol), and a record with no name.
	if len(result.Warnings) != 4 {
		t.Errorf("warnings = %v, want four", result.Warnings)
	}
	for _, warning := range result.Warnings {
		if !strings.Contains(warning, "line") {
			t.Errorf("warning %q does not say which line was unusable", warning)
		}
	}
}

func TestExecFailsWhenNothingParses(t *testing.T) {
	// A run that produced nothing but warnings has not succeeded, whatever the exit status
	// said. Reporting success here is how a broken external adapter goes unnoticed.
	command := writeAdapter(t, `cat > /dev/null
echo 'Traceback (most recent call last):'
echo '  File "adapter.py", line 1'
exit 0
`)
	_, err := ExecAdapter{}.Discover(context.Background(), Request{
		Association: execAssociation(map[string]any{"command": command}),
	})
	if err == nil {
		t.Fatal("an adapter that wrote no usable record reported success")
	}
	if !strings.Contains(err.Error(), "no usable records") {
		t.Errorf("err = %v", err)
	}
}

func TestExecReportsAFailureAfterPartialOutput(t *testing.T) {
	command := writeAdapter(t, `cat > /dev/null
echo '{"raw_name":"Exemple Externe SAS","source_url":"https://registry.example.org/members"}'
echo 'the upstream API rejected page 2' >&2
exit 3
`)
	result, err := ExecAdapter{}.Discover(context.Background(), Request{
		Association: execAssociation(map[string]any{"command": command}),
	})
	if err != nil {
		t.Fatalf("Discover discarded a partial result: %v", err)
	}
	if len(result.Records) != 1 {
		t.Errorf("got %d records, want the one written before the failure", len(result.Records))
	}
	if !result.Truncated {
		t.Error("a partial read was not reported as partial")
	}
	joined := strings.Join(result.Warnings, " ")
	if !strings.Contains(joined, "exit status 3") {
		t.Errorf("warnings = %v, want the exit status", result.Warnings)
	}
	if !strings.Contains(joined, "rejected page 2") {
		t.Errorf("warnings = %v, want the adapter's own diagnostics", result.Warnings)
	}
}

func TestExecWillNotRunAnythingThatIsNotAuthorised(t *testing.T) {
	authorised := writeAdapter(t, "cat > /dev/null\n")
	elsewhere := filepath.Join(t.TempDir(), "other.sh")
	if err := os.WriteFile(elsewhere, []byte("#!/bin/sh\ntrue\n"), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}

	cases := map[string]struct {
		command string
		allow   string
		want    string
	}{
		"nothing authorised": {
			command: authorised,
			allow:   "",
			want:    "no external adapter is authorised",
		},
		"not on the allowlist": {
			command: elsewhere,
			allow:   authorised,
			want:    "is not in " + AllowlistEnv,
		},
		"relative path": {
			command: "./adapter.sh",
			allow:   authorised,
			want:    "not an absolute path",
		},
		"a shell with an argument": {
			// The shape of the attack this guards against: naming an interpreter and
			// passing it a script. It fails because /bin/sh is not authorised, not
			// because the arguments were inspected.
			command: "/bin/sh",
			allow:   authorised,
			want:    "is not in " + AllowlistEnv,
		},
	}
	for name, testCase := range cases {
		t.Setenv(AllowlistEnv, testCase.allow)
		_, err := ExecAdapter{}.Discover(context.Background(), Request{
			Association: execAssociation(map[string]any{
				"command": testCase.command,
				"args":    []any{"-c", "echo hostile"},
			}),
		})
		if err == nil {
			t.Errorf("%s: the command was run", name)
			continue
		}
		if !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, testCase.want)
		}
	}
}

func TestExecResolvesANameInsideAnAuthorisedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the external-adapter tests run a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "registry-reader")
	script := "#!/bin/sh\ncat > /dev/null\n" +
		`echo '{"raw_name":"Exemple Externe SAS","source_url":"https://registry.example.org/m"}'` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv(AllowlistEnv, dir)

	result, err := ExecAdapter{}.Discover(context.Background(), Request{
		Association: execAssociation(map[string]any{"command": "registry-reader"}),
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Records) != 1 {
		t.Errorf("got %d records, want 1", len(result.Records))
	}
}

func TestExecDoesNotPassTheOperatorsEnvironmentToTheChild(t *testing.T) {
	// A third-party adapter has no business reading an API key that happens to be
	// exported, or the engine's own configuration.
	command := writeAdapter(t, `cat > /dev/null
echo "{\"raw_name\":\"leak:${EUCD_SECRET_FOR_TEST}x\",\"source_url\":\"https://registry.example.org/m\"}"
`)
	t.Setenv("EUCD_SECRET_FOR_TEST", "topsecret")

	result, err := ExecAdapter{}.Discover(context.Background(), Request{
		Association: execAssociation(map[string]any{"command": command}),
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got := result.Records[0].RawName; strings.Contains(got, "topsecret") {
		t.Errorf("the child saw the operator's environment: %q", got)
	}
}

func TestExecStopsAChildThatOverrunsItsTimeout(t *testing.T) {
	command := writeAdapter(t, `cat > /dev/null
sleep 30
`)
	_, err := ExecAdapter{}.Discover(context.Background(), Request{
		Association: execAssociation(map[string]any{
			"command": command,
			"timeout": "100ms",
		}),
	})
	if err == nil {
		t.Fatal("a child that never finished was waited for")
	}
	if !strings.Contains(err.Error(), "did not finish") {
		t.Errorf("err = %v, want it to say the adapter timed out", err)
	}
}

func TestExecCapsTheOutputItWillRead(t *testing.T) {
	// An adapter that streams forever must not be able to fill the engine's memory, and a
	// run cut off by the cap must say it was cut off.
	command := writeAdapter(t, `cat > /dev/null
echo '{"raw_name":"Exemple Externe SAS","source_url":"https://registry.example.org/m"}'
while true; do
  echo '{"raw_name":"Exemple Flood SAS","source_url":"https://registry.example.org/m"}'
done
`)
	result, err := ExecAdapter{}.Discover(context.Background(), Request{
		Association: execAssociation(map[string]any{
			"command":          command,
			"max_output_bytes": 4096,
			"timeout":          "20s",
		}),
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !result.Truncated {
		t.Error("output stopped at the cap was not reported as partial")
	}
	if len(result.Records) == 0 {
		t.Error("the records written before the cap were discarded")
	}
	joined := strings.Join(result.Warnings, " ")
	if !strings.Contains(joined, "cap") {
		t.Errorf("warnings = %v, want one naming the cap", result.Warnings)
	}
}

func TestExecRejectsUnusableSettings(t *testing.T) {
	command := writeAdapter(t, "true\n")
	cases := map[string]struct {
		settings map[string]any
		want     string
	}{
		"no command": {
			settings: map[string]any{},
			want:     "settings.command",
		},
		"args not a list": {
			settings: map[string]any{"command": command, "args": "--all"},
			want:     "settings.args must be a list",
		},
		"timeout not a duration": {
			settings: map[string]any{"command": command, "timeout": "soon"},
			want:     "not a duration",
		},
	}
	for name, testCase := range cases {
		_, err := ExecAdapter{}.Discover(context.Background(), Request{
			Association: execAssociation(testCase.settings),
		})
		if err == nil {
			t.Errorf("%s: settings were accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, testCase.want)
		}
	}
}

func TestExecIsRegistered(t *testing.T) {
	adapter, err := Get(ExecName)
	if err != nil {
		t.Fatalf("Get(%q): %v", ExecName, err)
	}
	if adapter.Name() != ExecName {
		t.Errorf("Name() = %q", adapter.Name())
	}
}
