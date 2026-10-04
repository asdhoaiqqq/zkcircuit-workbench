package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These tests pin the CLI contract for the input-layout representability rule:
// an unrepresentable create/update is a rule failure (exit 1) with a clear
// message and no success record; the representable boundary (M-1 public, 0
// private) still creates and queries; and a committed record beyond the bound
// makes every data command fail at open with exit 3.

func TestCLICreateInputLayoutOverflow(t *testing.T) {
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

	// public = M, private = 1 must be refused, not accepted through wraparound.
	r := call("circuit-create", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "1",
		"--public-inputs", strconv.Itoa(math.MaxInt), "--private-inputs", "1")
	if r.code != 1 {
		t.Fatalf("overflow create: want exit 1, got %d (out=%q err=%q)", r.code, r.out, r.err)
	}
	if r.out != "" {
		t.Fatalf("overflow create printed a success record: %q", r.out)
	}
	if !strings.Contains(r.err, "invalid argument") || !strings.Contains(r.err, "not representable") {
		t.Fatalf("overflow create error must explain the layout limit: %q", r.err)
	}

	// public = M on its own already leaves no room for the constant wire.
	if r := call("circuit-create", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "1",
		"--public-inputs", strconv.Itoa(math.MaxInt)); r.code != 1 ||
		!strings.Contains(r.err, "not representable") {
		t.Fatalf("public=M only: code=%d err=%q", r.code, r.err)
	}

	// The refused create left no record behind.
	if r := call("circuit-list", "--dir", dir); r.code != 0 || strings.Contains(r.out, "name=c") {
		t.Fatalf("refused create left a record: code=%d out=%q", r.code, r.out)
	}

	// The exact representable boundary still creates and queries normally.
	if r := call("circuit-create", "--dir", dir, "--name", "edge", "--version", "1",
		"--constraints", "1",
		"--public-inputs", strconv.Itoa(math.MaxInt-1), "--private-inputs", "0"); r.code != 0 {
		t.Fatalf("boundary create: code=%d err=%q", r.code, r.err)
	}
	if r := call("circuit-get", "--dir", dir, "--name", "edge", "--version", "1"); r.code != 0 ||
		!strings.Contains(r.out, "public_inputs="+strconv.Itoa(math.MaxInt-1)) ||
		!strings.Contains(r.out, "private_inputs=0") {
		t.Fatalf("boundary query: code=%d out=%q err=%q", r.code, r.out, r.err)
	}

	// Zero public and zero private inputs still keep the constant wire: a
	// 0/0 draft creates cleanly.
	if r := call("circuit-create", "--dir", dir, "--name", "zero", "--version", "1",
		"--constraints", "1",
		"--public-inputs", "0", "--private-inputs", "0"); r.code != 0 {
		t.Fatalf("zero/zero create: code=%d err=%q", r.code, r.err)
	}
}

func TestCLIUpdateInputLayoutOverflow(t *testing.T) {
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

	if r := call("circuit-create", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "2", "--public-inputs", "1", "--private-inputs", "3",
		"--description", "初稿"); r.code != 0 {
		t.Fatalf("create: %+v", r)
	}

	// Change only the public count; the retained 3 private inputs push M past
	// the bound. A description supplied in the same change must be refused too.
	if r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--public-inputs", strconv.Itoa(math.MaxInt), "--description", "x"); r.code != 1 ||
		!strings.Contains(r.err, "not representable") {
		t.Fatalf("public=M with retained private=3: code=%d err=%q", r.code, r.err)
	}
	// Change only the private count; the retained 1 public input pushes.
	if r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--private-inputs", strconv.Itoa(math.MaxInt)); r.code != 1 ||
		!strings.Contains(r.err, "not representable") {
		t.Fatalf("private=M with retained public=1: code=%d err=%q", r.code, r.err)
	}

	// Counts and description all stay as they were.
	r := call("circuit-get", "--dir", dir, "--name", "c", "--version", "1")
	if r.code != 0 ||
		!strings.Contains(r.out, "constraints=2") || !strings.Contains(r.out, "public_inputs=1") ||
		!strings.Contains(r.out, "private_inputs=3") || !strings.Contains(r.out, `description="初稿"`) {
		t.Fatalf("rejected update leaked: %+v", r)
	}

	// Reaching the exact boundary through partial edits stays possible.
	if r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--private-inputs", "0"); r.code != 0 {
		t.Fatalf("zero private: %+v", r)
	}
	if r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--public-inputs", strconv.Itoa(math.MaxInt-1)); r.code != 0 ||
		!strings.Contains(r.out, "public_inputs="+strconv.Itoa(math.MaxInt-1)) {
		t.Fatalf("public=M-1 boundary: %+v", r)
	}
}

// readNumberedEnv decodes data.json preserving every integer as an exact
// json.Number, so a tampered MaxInt-sized value is not degraded through
// float64 before the strict int decoder sees it.
func readNumberedEnv(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var env map[string]any
	if err := dec.Decode(&env); err != nil {
		t.Fatal(err)
	}
	return env
}

func writeNumberedEnv(t *testing.T, path string, env map[string]any) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCLICommittedOverflowLayoutExitThree(t *testing.T) {
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

	// Commit the representable boundary, then tamper one private input into
	// the file: 1 + (M-1) + 1 = M+1.
	if r := call("circuit-create", "--dir", dir, "--name", "edge", "--version", "1",
		"--constraints", "1",
		"--public-inputs", strconv.Itoa(math.MaxInt-1), "--private-inputs", "0"); r.code != 0 {
		t.Fatalf("create boundary: %+v", r)
	}
	path := filepath.Join(dir, "data.json")
	env := readNumberedEnv(t, path)
	rec := env["circuits"].([]any)[0].(map[string]any)
	rec["private_inputs"] = json.Number("1")
	bad := writeNumberedEnv(t, path, env)

	for _, args := range [][]string{
		{"circuit-list", "--dir", dir},
		{"circuit-get", "--dir", dir, "--name", "edge", "--version", "1"},
	} {
		r := call(args...)
		if r.code != 3 {
			t.Fatalf("%s: want exit 3, got %d (out=%q err=%q)", args[0], r.code, r.out, r.err)
		}
		if r.out != "" {
			t.Fatalf("%s printed partial results despite corruption: %q", args[0], r.out)
		}
		if !strings.Contains(r.err, "read failed") || !strings.Contains(r.err, "data corrupt") ||
			!strings.Contains(r.err, "not representable") {
			t.Fatalf("%s: unexpected error %q", args[0], r.err)
		}
	}

	left, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(bad) {
		t.Fatal("refused read modified data.json")
	}
}
