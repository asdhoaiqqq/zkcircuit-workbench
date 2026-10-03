package main

import (
	"strings"
	"testing"
)

// TestCLIPrivateInputErrorsDoNotLeak pins the command-line privacy contract:
// a rejected check whose cause lies in the PRIVATE input exits 1, prints no
// satisfaction conclusion on stdout, and on stderr names the cause (private
// group, 1-based index, required shape / "input format error") without quoting
// the private value, any character or fragment taken from it, any object key
// found inside the private array, or any raw bytes the parser met there.
func TestCLIPrivateInputErrorsDoNotLeak(t *testing.T) {
	work := t.TempDir()
	dir := work + "/bench"
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

	// One public, one private input; freeze, setup and compile to a hash.
	if r := call("circuit-create", "--dir", dir, "--name", "mul", "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1",
		"--description", "smoke"); r.code != 0 {
		t.Fatalf("create: %+v", r)
	}
	if r := call("constraint-import", "--dir", dir, "--name", "mul", "--version", "1",
		"--file", defPath); r.code != 0 {
		t.Fatalf("import: %+v", r)
	}
	if r := call("circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"); r.code != 0 {
		t.Fatalf("freeze: %+v", r)
	}
	compiled := call("circuit-compile", "--dir", dir, "--name", "mul", "--version", "1")
	if compiled.code != 0 {
		t.Fatalf("compile: %+v", compiled)
	}
	hash := strings.TrimSpace(strings.SplitN(strings.SplitAfter(compiled.out, "hash=")[1], "\n", 2)[0])

	cases := []struct {
		name    string
		content string
		marker  string // private bytes that must never appear on stdout or stderr
		want    string // required stderr content
	}{
		{"illegal character", `{"public":["2"],"private":["123ZZCLISECQ"]}`, "ZZCLISECQ",
			"private input #1 is not a decimal integer"},
		{"empty string", `{"public":["2"],"private":[""]}`, "",
			"private input #1 is not a decimal integer: empty string"},
		{"sign only", `{"public":["2"],"private":["-"]}`, "",
			"private input #1 is not a decimal integer: sign without digits"},
		{"duplicate key in object element", `{"public":["2"],"private":[{"ZZCLIDUP":1,"ZZCLIDUP":2}]}`, "ZZCLIDUP",
			"private input #1 must be a decimal string, not an object"},
		{"number element", `{"public":["2"],"private":[424242]}`, "424242",
			"private input #1 must be a decimal string, not a number"},
		{"boolean element", `{"public":["2"],"private":[false]}`, "false",
			"private input #1 must be a decimal string, not a boolean"},
		{"null element", `{"public":["2"],"private":[null]}`, "",
			"private input #1 must be a decimal string, not null"},
		{"nested array element", `{"public":["2"],"private":[["ZZCLINEST"]]}`, "ZZCLINEST",
			"private input #1 must be a decimal string, not an array"},
		{"syntax error inside private", `{"public":["2"],"private":["ZZCLISYN" "x"]}`, "ZZCLISYN",
			`input field "private" is not valid JSON`},
		{"private group not an array", `{"public":["2"],"private":7}`, "",
			`input field "private" must be an array of decimal strings`},
		{"private length mismatch", `{"public":["2"],"private":["3","4"]}`, "",
			"private input array has 2 values but the version requires 1 decimal strings"},
		{"damaged document", `{"public":["2"],"private":["3"]} ZZCLIBROKEN`, "ZZCLIBROKEN",
			"input is not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, work, "in.json", tc.content)
			r := call("input-check", "--dir", dir, "--name", "mul", "--version", "1",
				"--hash", hash, "--file", path)
			if r.code != 1 {
				t.Fatalf("want exit 1, got %d (out=%q err=%q)", r.code, r.out, r.err)
			}
			if r.out != "" {
				t.Fatalf("a rejected check must print no conclusion on stdout, got %q", r.out)
			}
			if !strings.Contains(r.err, "input format error") {
				t.Fatalf("stderr must carry the input-format category: %q", r.err)
			}
			if !strings.Contains(r.err, tc.want) {
				t.Fatalf("stderr %q\n must contain %q", r.err, tc.want)
			}
			if tc.marker != "" {
				if strings.Contains(r.out, tc.marker) || strings.Contains(r.err, tc.marker) {
					t.Fatalf("private bytes %q leaked (out=%q err=%q)", tc.marker, r.out, r.err)
				}
			}
		})
	}

	// A legal check still exits 0 with the normal verdict on stdout.
	good := writeFile(t, work, "good.json", cliGoodInput)
	if r := call("input-check", "--dir", dir, "--name", "mul", "--version", "1",
		"--hash", hash, "--file", good); r.code != 0 ||
		!strings.Contains(r.out, "satisfied=true") || r.err != "" {
		t.Fatalf("legal check behavior changed: %+v", r)
	}
}
