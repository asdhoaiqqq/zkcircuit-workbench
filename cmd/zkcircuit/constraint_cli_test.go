package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile is a small helper for seeding JSON input files.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const cliDef = `{"modulus":"7","constraints":[
	{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`

const cliGoodInput = `{"public":["2"],"private":["3"]}`

// 100003 ≡ 1 mod 7, so 2·1 = 2 ≠ 6: this fails the one constraint. The
// distinctive value also lets the test assert the private input is never
// echoed in the verdict.
const cliBadInput = `{"public":["2"],"private":["100003"]}`

func TestCLIConstraintLifecycle(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "bench")
	defPath := writeFile(t, work, "def.json", cliDef)
	goodPath := writeFile(t, work, "good.json", cliGoodInput)
	badPath := writeFile(t, work, "bad.json", cliBadInput)

	type res struct {
		code int
		out  string
		err  string
	}
	call := func(args ...string) res {
		c, o, e := runCapture(t, args...)
		return res{c, o, e}
	}

	// Create a draft whose counts match the definition, then import.
	if r := call("circuit-create", "--dir", dir, "--name", "mul", "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1",
		"--description", "smoke"); r.code != 0 {
		t.Fatalf("create: %+v", r)
	}
	if r := call("constraint-import", "--dir", dir, "--name", "mul", "--version", "1",
		"--file", defPath); r.code != 0 || !strings.Contains(r.out, "modulus=7") {
		t.Fatalf("import: %+v", r)
	}
	// Compile is refused before freeze.
	if r := call("circuit-compile", "--dir", dir, "--name", "mul", "--version", "1"); r.code != 1 || !strings.Contains(r.err, "not frozen") {
		t.Fatalf("compile draft: %+v", r)
	}
	// Illegal JSON is a rule failure and leaves no definition.
	badDef := writeFile(t, work, "bad.json2", `{"modulus":"9","constraints":[]}`)
	if r := call("constraint-import", "--dir", dir, "--name", "mul", "--version", "1",
		"--file", badDef); r.code != 1 || !strings.Contains(r.err, "prime") {
		t.Fatalf("composite modulus: %+v", r)
	}
	// Freeze, then compile to a stable hash.
	if r := call("circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"); r.code != 0 {
		t.Fatalf("freeze: %+v", r)
	}
	r1 := call("circuit-compile", "--dir", dir, "--name", "mul", "--version", "1")
	if r1.code != 0 || !strings.Contains(r1.out, "hash=") {
		t.Fatalf("compile: %+v", r1)
	}
	hash := strings.TrimSpace(strings.SplitN(strings.SplitAfter(r1.out, "hash=")[1], "\n", 2)[0])
	if len(hash) != 64 {
		t.Fatalf("bad hash %q", hash)
	}
	// Recompiling returns the same artifact.
	r2 := call("circuit-compile", "--dir", dir, "--name", "mul", "--version", "1")
	if r2.code != 0 || !strings.Contains(r2.out, "hash="+hash) {
		t.Fatalf("recompile differs: %+v", r2)
	}
	// A frozen definition cannot be replaced.
	if r := call("constraint-import", "--dir", dir, "--name", "mul", "--version", "1",
		"--file", defPath); r.code != 1 || !strings.Contains(r.err, "frozen") {
		t.Fatalf("frozen replace: %+v", r)
	}

	// Satisfied input.
	if r := call("input-check", "--dir", dir, "--name", "mul", "--version", "1",
		"--hash", hash, "--file", goodPath); r.code != 0 ||
		!strings.Contains(r.out, "satisfied=true") || strings.Contains(r.out, "private") {
		t.Fatalf("satisfied check: %+v", r)
	}
	// Unsatisfied input: verdict on stdout, exit 0, first failure reported,
	// no private value echoed.
	if r := call("input-check", "--dir", dir, "--name", "mul", "--version", "1",
		"--hash", hash, "--file", badPath); r.code != 0 ||
		!strings.Contains(r.out, "satisfied=false") || !strings.Contains(r.out, "first_failure=1") ||
		strings.Contains(r.out, "100003") || strings.Contains(r.err, "100003") {
		t.Fatalf("unsatisfied check: %+v", r)
	}

	// Missing --hash / --file are usage errors.
	if r := call("input-check", "--dir", dir, "--name", "mul", "--version", "1",
		"--file", goodPath); r.code != 2 {
		t.Fatalf("missing hash: %+v", r)
	}
	if r := call("input-check", "--dir", dir, "--name", "mul", "--version", "1",
		"--hash", hash); r.code != 2 {
		t.Fatalf("missing file: %+v", r)
	}
	// Wrong hash -> artifact mismatch (exit 1).
	if r := call("input-check", "--dir", dir, "--name", "mul", "--version", "1",
		"--hash", strings.Repeat("0", 64), "--file", goodPath); r.code != 1 ||
		!strings.Contains(r.err, "artifact mismatch") {
		t.Fatalf("wrong hash: %+v", r)
	}
	// Malformed witness -> input format error (exit 1).
	badWitness := writeFile(t, work, "badwitness.json", `{"public":["2"]}`)
	if r := call("input-check", "--dir", dir, "--name", "mul", "--version", "1",
		"--hash", hash, "--file", badWitness); r.code != 1 ||
		!strings.Contains(r.err, "input format error") {
		t.Fatalf("bad witness: %+v", r)
	}

	// Reopen in a fresh process invocation: artifact still usable.
	if r := call("input-check", "--dir", dir, "--name", "mul", "--version", "1",
		"--hash", hash, "--file", goodPath); r.code != 0 || !strings.Contains(r.out, "satisfied=true") {
		t.Fatalf("check after reopen: %+v", r)
	}
}

// TestCLIPrivateInputDiagnosticsDoNotLeak drives input-check through the
// command line and asserts the privacy contract end to end: a malformed
// private input yields exit code 1 with an "input format error" reason, no
// check conclusion on stdout, and no private value, character, fragment or
// object key on either output stream.
func TestCLIPrivateInputDiagnosticsDoNotLeak(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "bench")
	defPath := writeFile(t, work, "def.json", cliDef)

	type res struct {
		code int
		out  string
		err  string
	}
	call := func(args ...string) res {
		c, o, e := runCapture(t, args...)
		return res{c, o, e}
	}

	if r := call("circuit-create", "--dir", dir, "--name", "mul", "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "2"); r.code != 0 {
		t.Fatalf("create: %+v", r)
	}
	if r := call("constraint-import", "--dir", dir, "--name", "mul", "--version", "1",
		"--file", defPath); r.code != 0 {
		t.Fatalf("import: %+v", r)
	}
	if r := call("circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"); r.code != 0 {
		t.Fatalf("freeze: %+v", r)
	}
	r1 := call("circuit-compile", "--dir", dir, "--name", "mul", "--version", "1")
	if r1.code != 0 {
		t.Fatalf("compile: %+v", r1)
	}
	hash := strings.TrimSpace(strings.SplitN(strings.SplitAfter(r1.out, "hash=")[1], "\n", 2)[0])

	// Secret tokens planted inside the private section must never surface.
	const secret = "SECRETK"
	cases := []struct {
		name    string
		content string
		want    string // actionable detail that must be present
	}{
		{"illegal char", `{"public":["2"],"private":["3","123` + secret + `"]}`, "private input #2"},
		{"empty", `{"public":["2"],"private":["3",""]}`, "the string is empty"},
		{"minus only", `{"public":["2"],"private":["3","-"]}`, "minus sign without digits"},
		{"number element", `{"public":["2"],"private":["3",824173]}`, "private input #2"},
		{"object with dup key", `{"public":["2"],"private":["3",{"` + secret + `":1,"` + secret + `":2}]}`, "private input #2"},
		{"syntax in private", `{"public":["2"],"private":["3",` + secret + `]}`, "private input group"},
		{"private length", `{"public":["2"],"private":["3"]}`, "private input length mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, work, "input-"+tc.name+".json", tc.content)
			r := call("input-check", "--dir", dir, "--name", "mul", "--version", "1",
				"--hash", hash, "--file", path)
			if r.code != 1 {
				t.Fatalf("want exit 1, got %d (out=%q err=%q)", r.code, r.out, r.err)
			}
			if strings.Contains(r.out, "satisfied") {
				t.Fatalf("a rejected input must not print a check conclusion: %q", r.out)
			}
			if !strings.Contains(r.err, "input format error") {
				t.Fatalf("must keep the input-format category: %q", r.err)
			}
			if !strings.Contains(r.err, tc.want) {
				t.Fatalf("reason must name %q, got %q", tc.want, r.err)
			}
			for _, stream := range []string{r.out, r.err} {
				if strings.Contains(stream, secret) {
					t.Fatalf("private token leaked on output: %q", stream)
				}
			}
			if tc.name == "number element" && strings.Contains(r.out+r.err, "824173") {
				t.Fatalf("private numeric element leaked: %q %q", r.out, r.err)
			}
		})
	}

	// A valid input still evaluates normally (exit 0, conclusion printed).
	good := writeFile(t, work, "good-cli.json", `{"public":["2"],"private":["3","1"]}`)
	if r := call("input-check", "--dir", dir, "--name", "mul", "--version", "1",
		"--hash", hash, "--file", good); r.code != 0 || !strings.Contains(r.out, "satisfied=true") {
		t.Fatalf("valid input should check: %+v", r)
	}
}

func TestCLICountsOnlyVersionCannotCompile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	type res struct {
		code int
		out  string
		err  string
	}
	call := func(args ...string) res {
		c, o, e := runCapture(t, args...)
		return res{c, o, e}
	}

	if r := call("circuit-create", "--dir", dir, "--name", "bare", "--version", "1",
		"--constraints", "1"); r.code != 0 {
		t.Fatalf("create: %+v", r)
	}
	if r := call("circuit-freeze", "--dir", dir, "--name", "bare", "--version", "1"); r.code != 0 {
		t.Fatalf("freeze: %+v", r)
	}
	if r := call("setup-record", "--dir", dir, "--name", "bare", "--version", "1"); r.code != 0 {
		t.Fatalf("setup: %+v", r)
	}
	if r := call("job-submit", "--dir", dir, "--id", "j1", "--name", "bare",
		"--version", "1"); r.code != 0 {
		t.Fatalf("job on counts-only version should succeed: %+v", r)
	}
	// Compilation must explicitly report the missing definition.
	if r := call("circuit-compile", "--dir", dir, "--name", "bare", "--version", "1"); r.code != 1 ||
		!strings.Contains(r.err, "constraint definition missing") {
		t.Fatalf("counts-only compile: %+v", r)
	}
}
