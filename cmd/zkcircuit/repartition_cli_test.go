package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end regression for moving the public/private boundary of a draft
// that already has an imported definition. It drives the real CLI against
// independent data directories and pins:
//
//   - circuit-update after constraint-import succeeds when the constraint
//     count and every referenced wire stay valid, and the query then shows
//     the two new group sizes,
//   - the compile hash reflects the partition (two circuits identical but
//     for the split compile to different hashes in separate directories),
//   - input-check evaluates the SAME numbered values by their NEW ownership
//     (wire 2 becomes the second public input; wire 3 the first private),
//     reporting satisfied + its artifact hash or unsatisfied + the 1-based
//     first failure, without trusting the old boundary,
//   - a hash from the other partition is an explicit "artifact mismatch"
//     with no satisfaction verdict,
//   - arrays still grouped at the old 1/2 boundary (right grand total) are an
//     "input format error",
//   - a group of size 0 at one end still checks by wire number,
//   - a frozen version can no longer be repartitioned.
//
// Definition (mod 13), highest wire 3:
//
//	#1  wire2 · wire1 = 6
//	#2  wire3 · 1     = 4   (wire 0 = constant 1)
//
// Satisfying numbered values: wire1=2, wire2=3, wire3=4.
const cliRepartitionDef = `{"modulus":"13","constraints":[
	{"a":[{"wire":2,"coeff":"1"}],"b":[{"wire":1,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]},
	{"a":[{"wire":3,"coeff":"1"}],"b":[{"wire":0,"coeff":"1"}],"c":[{"wire":0,"coeff":"4"}]}]}`

// extractArtifactHash pulls the hash=<64-hex> value out of a compile line.
func extractArtifactHash(t *testing.T, out string) string {
	t.Helper()
	const marker = "hash="
	i := strings.Index(out, marker)
	if i < 0 {
		t.Fatalf("no hash in compile output: %q", out)
	}
	rest := out[i+len(marker):]
	if end := strings.IndexAny(rest, " \n"); end >= 0 {
		rest = rest[:end]
	}
	if len(rest) != 64 {
		t.Fatalf("bad artifact hash %q", rest)
	}
	return rest
}

type cliRes struct {
	code     int
	out, err string
}

func newCLICall(t *testing.T) func(args ...string) cliRes {
	t.Helper()
	return func(args ...string) cliRes {
		c, o, e := runCapture(t, args...)
		return cliRes{c, o, e}
	}
}

// TestCLIRepartitionAfterImportFollowsNewBoundary is the headline
// end-to-end flow: import under 1/2, move the split to 2/1 as a draft, then
// freeze/compile/check against the new grouping.
func TestCLIRepartitionAfterImportFollowsNewBoundary(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "bench")
	defPath := writeFile(t, work, "def.json", cliRepartitionDef)
	goodPath := writeFile(t, work, "good.json", `{"public":["2","3"],"private":["4"]}`)
	swappedPath := writeFile(t, work, "swapped.json", `{"public":["2","4"],"private":["3"]}`)
	badWire3Path := writeFile(t, work, "badwire3.json", `{"public":["2","3"],"private":["5"]}`)
	oldLayoutPath := writeFile(t, work, "old.json", `{"public":["2"],"private":["3","4"]}`)
	call := newCLICall(t)

	must := func(r cliRes, step string) {
		t.Helper()
		if r.code != 0 {
			t.Fatalf("%s: code=%d out=%q err=%q", step, r.code, r.out, r.err)
		}
	}

	must(call("circuit-create", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "2", "--public-inputs", "1", "--private-inputs", "2"), "create 1/2")
	must(call("constraint-import", "--dir", dir, "--name", "c", "--version", "1",
		"--file", defPath), "import")

	// Move the boundary to 2 public / 1 private with the definition present:
	// count and all wires stay in range, so this succeeds as one change.
	r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--public-inputs", "2", "--private-inputs", "1")
	must(r, "repartition 2/1")
	if !strings.Contains(r.out, "public_inputs=2") || !strings.Contains(r.out, "private_inputs=1") ||
		!strings.Contains(r.out, "constraints=2") {
		t.Fatalf("update must show the two new group sizes: %q", r.out)
	}
	// The query confirms the new partition and that it is still a draft.
	if r := call("circuit-get", "--dir", dir, "--name", "c", "--version", "1"); r.code != 0 ||
		!strings.Contains(r.out, "public_inputs=2") || !strings.Contains(r.out, "private_inputs=1") ||
		strings.Contains(r.out, "frozen=true") {
		t.Fatalf("query after repartition: %+v", r)
	}

	must(call("circuit-freeze", "--dir", dir, "--name", "c", "--version", "1"), "freeze")
	rc := call("circuit-compile", "--dir", dir, "--name", "c", "--version", "1")
	must(rc, "compile")
	hash := extractArtifactHash(t, rc.out)
	if !strings.Contains(rc.out, "modulus=13") || !strings.Contains(rc.out, "constraints=2") {
		t.Fatalf("compile output lost modulus/count: %q", rc.out)
	}

	// Satisfied under the new ownership (wire2 public, wire3 private).
	if r := call("input-check", "--dir", dir, "--name", "c", "--version", "1",
		"--hash", hash, "--file", goodPath); r.code != 0 ||
		!strings.Contains(r.out, "satisfied=true") || !strings.Contains(r.out, "hash="+hash) ||
		strings.Contains(r.out, "private") {
		t.Fatalf("satisfied check: %+v", r)
	}
	// Swapped moved positions flip the verdict: first failure is constraint 1.
	if r := call("input-check", "--dir", dir, "--name", "c", "--version", "1",
		"--hash", hash, "--file", swappedPath); r.code != 0 ||
		!strings.Contains(r.out, "satisfied=false") || !strings.Contains(r.out, "first_failure=1") {
		t.Fatalf("swapped-position check: %+v", r)
	}
	// Wrong value on wire 3 (the lone private) reaches constraint 2 first.
	if r := call("input-check", "--dir", dir, "--name", "c", "--version", "1",
		"--hash", hash, "--file", badWire3Path); r.code != 0 ||
		!strings.Contains(r.out, "first_failure=2") {
		t.Fatalf("bad-wire3 check: %+v", r)
	}
	// Old-boundary arrays (1/2), right grand total, must be a format error.
	if r := call("input-check", "--dir", dir, "--name", "c", "--version", "1",
		"--hash", hash, "--file", oldLayoutPath); r.code != 1 ||
		!strings.Contains(r.err, "input format error") {
		t.Fatalf("old-layout arrays: %+v", r)
	}

	// Once frozen the partition can never move again.
	if r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--public-inputs", "3", "--private-inputs", "0"); r.code != 1 ||
		!strings.Contains(r.err, "frozen") {
		t.Fatalf("frozen repartition: %+v", r)
	}
}

// TestCLIRepartitionCompileHashDistinguishesPartitions compiles the same
// circuit content under both splits in two separate data directories and
// shows the hashes differ and cross-partition checks are an explicit
// artifact mismatch with no verdict.
func TestCLIRepartitionCompileHashDistinguishesPartitions(t *testing.T) {
	work := t.TempDir()
	dirOld := filepath.Join(work, "old")
	dirNew := filepath.Join(work, "new")
	defPath := writeFile(t, work, "def.json", cliRepartitionDef)
	newGood := writeFile(t, work, "newgood.json", `{"public":["2","3"],"private":["4"]}`)
	oldGood := writeFile(t, work, "oldgood.json", `{"public":["2"],"private":["3","4"]}`)
	call := newCLICall(t)

	compile := func(dir, pub, priv string) string {
		t.Helper()
		mk := func(step string, args ...string) cliRes {
			t.Helper()
			r := call(args...)
			if r.code != 0 {
				t.Fatalf("%s: code=%d out=%q err=%q", step, r.code, r.out, r.err)
			}
			return r
		}
		mk("create", "circuit-create", "--dir", dir, "--name", "c", "--version", "1",
			"--constraints", "2", "--public-inputs", pub, "--private-inputs", priv)
		mk("import", "constraint-import", "--dir", dir, "--name", "c", "--version", "1",
			"--file", defPath)
		mk("freeze", "circuit-freeze", "--dir", dir, "--name", "c", "--version", "1")
		return extractArtifactHash(t, mk("compile", "circuit-compile",
			"--dir", dir, "--name", "c", "--version", "1").out)
	}

	hashOld := compile(dirOld, "1", "2")
	hashNew := compile(dirNew, "2", "1")
	if hashOld == hashNew {
		t.Fatalf("partitions 1/2 and 2/1 must compile to different hashes (both %q)", hashOld)
	}

	// The 2/1 circuit accepts its own hash and rejects the 1/2 hash outright.
	if r := call("input-check", "--dir", dirNew, "--name", "c", "--version", "1",
		"--hash", hashNew, "--file", newGood); r.code != 0 ||
		!strings.Contains(r.out, "satisfied=true") {
		t.Fatalf("own-hash check: %+v", r)
	}
	if r := call("input-check", "--dir", dirNew, "--name", "c", "--version", "1",
		"--hash", hashOld, "--file", newGood); r.code != 1 ||
		!strings.Contains(r.err, "artifact mismatch") || strings.Contains(r.out, "satisfied") {
		t.Fatalf("cross-partition hash must mismatch with no verdict: %+v", r)
	}
	// The 1/2 circuit accepts its own hash with its own grouping.
	if r := call("input-check", "--dir", dirOld, "--name", "c", "--version", "1",
		"--hash", hashOld, "--file", oldGood); r.code != 0 ||
		!strings.Contains(r.out, "satisfied=true") {
		t.Fatalf("1/2 own-hash check: %+v", r)
	}
	// A 2/1 document has the right total but wrong group lengths for the 1/2
	// artifact, so it is an input format error — the old split is not guessed.
	if r := call("input-check", "--dir", dirOld, "--name", "c", "--version", "1",
		"--hash", hashOld, "--file", newGood); r.code != 1 ||
		!strings.Contains(r.err, "input format error") {
		t.Fatalf("2/1 document against 1/2 artifact: %+v", r)
	}
}

// TestCLIRepartitionEmptyGroupAtBoundary covers the split moved to an end:
// 3 public / 0 private still checks by wire number with an empty private
// group, and an omitted/short group is a format error.
func TestCLIRepartitionEmptyGroupAtBoundary(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "bench")
	defPath := writeFile(t, work, "def.json", cliRepartitionDef)
	allPubGood := writeFile(t, work, "allpub.json", `{"public":["2","3","4"],"private":[]}`)
	allPubBad := writeFile(t, work, "allpubbad.json", `{"public":["2","3","5"],"private":[]}`)
	groupedWrong := writeFile(t, work, "grouped.json", `{"public":["2","3"],"private":["4"]}`)
	call := newCLICall(t)

	mk := func(step string, args ...string) cliRes {
		t.Helper()
		r := call(args...)
		if r.code != 0 {
			t.Fatalf("%s: code=%d out=%q err=%q", step, r.code, r.out, r.err)
		}
		return r
	}
	mk("create", "circuit-create", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "2", "--public-inputs", "1", "--private-inputs", "2")
	mk("import", "constraint-import", "--dir", dir, "--name", "c", "--version", "1",
		"--file", defPath)
	mk("repartition 3/0", "circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--public-inputs", "3", "--private-inputs", "0")
	mk("freeze", "circuit-freeze", "--dir", dir, "--name", "c", "--version", "1")
	hash := extractArtifactHash(t, mk("compile", "circuit-compile",
		"--dir", dir, "--name", "c", "--version", "1").out)

	// Empty private group, values taken by wire number -> satisfied.
	if r := call("input-check", "--dir", dir, "--name", "c", "--version", "1",
		"--hash", hash, "--file", allPubGood); r.code != 0 ||
		!strings.Contains(r.out, "satisfied=true") {
		t.Fatalf("3/0 satisfied: %+v", r)
	}
	// Same layout, a value that breaks constraint #2 (wire3=5) reports it.
	if r := call("input-check", "--dir", dir, "--name", "c", "--version", "1",
		"--hash", hash, "--file", allPubBad); r.code != 0 ||
		!strings.Contains(r.out, "satisfied=false") || !strings.Contains(r.out, "first_failure=2") {
		t.Fatalf("3/0 unsatisfied: %+v", r)
	}
	// A 2/1 document is wrong group lengths even though total is 3.
	if r := call("input-check", "--dir", dir, "--name", "c", "--version", "1",
		"--hash", hash, "--file", groupedWrong); r.code != 1 ||
		!strings.Contains(r.err, "input format error") {
		t.Fatalf("2/1 document against 3/0 artifact: %+v", r)
	}
}
