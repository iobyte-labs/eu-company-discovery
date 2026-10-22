package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// run executes the CLI in-process and returns its exit code and streams.
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Execute(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// Exercises the path that writes the data contract to a file.
//
// It exists because `export -o` used to drop the error from closing that file. Closing a
// written file is where its last bytes are flushed, so an export cut short — a full disk, a
// quota, a network filesystem — was reported as a success, and a private consumer would
// read a file it had no way of knowing was meant to be longer.
func TestExportWritesAFileAndSaysWhereItWent(t *testing.T) {
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "companies.json")

	code, _, stderr := run(t,
		"--db", filepath.Join(dir, "test.db"), "export", "--format", "json", "-o", outputPath)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, outputPath) {
		t.Errorf("stderr = %q, want it to name the file written", stderr)
	}

	raw, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("the export was not written: %v", err)
	}
	// The whole document has to be there: a half-written envelope is exactly the failure
	// this command must not report as success.
	var envelope struct {
		SchemaVersion string `json:"schema_version"`
		Count         int    `json:"count"`
		Companies     []any  `json:"companies"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("the export is not complete JSON: %v\n%s", err, raw)
	}
	if envelope.SchemaVersion == "" {
		t.Error("the export carries no schema version, so a consumer cannot check it")
	}
	if envelope.Count != len(envelope.Companies) {
		t.Errorf("count %d disagrees with %d companies", envelope.Count, len(envelope.Companies))
	}
}

func TestExportReportsAFileItCouldNotCreate(t *testing.T) {
	dir := t.TempDir()
	// A directory that does not exist: os.Create fails before anything is written.
	outputPath := filepath.Join(dir, "no-such-directory", "companies.json")

	code, stdout, stderr := run(t,
		"--db", filepath.Join(dir, "test.db"), "export", "--format", "json", "-o", outputPath)
	if code == 0 {
		t.Fatal("exit 0 after failing to create the output file")
	}
	if !strings.Contains(stderr, "no-such-directory") {
		t.Errorf("stderr = %q, want it to name the path that could not be created", stderr)
	}
	// The envelope must not have gone to stdout as a consolation prize: a caller that
	// redirected stdout would end up with a file it did not ask for.
	if strings.Contains(stdout, "schema_version") {
		t.Errorf("stdout = %q, want no export when the file could not be created", stdout)
	}
}

func TestExportRefusesAFormatItCannotWrite(t *testing.T) {
	dir := t.TempDir()
	code, _, stderr := run(t, "--db", filepath.Join(dir, "test.db"), "export", "--format", "xml")
	if code != 64 {
		t.Errorf("exit %d, want 64 (bad invocation); stderr: %s", code, stderr)
	}
}
