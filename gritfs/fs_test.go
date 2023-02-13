// Copyright 2023 Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gritfs

import (
	"os"
	"reflect"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

func TestSetGritCommit(t *testing.T) {
	h := plumbing.NewHash("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	for in, want := range map[string]string{`abc

Change-Id: 123`: `abc

Change-Id: 123
Grit-Amends: deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
`, `abc

Change-Id: 123
Grit-Amends: 1eadbeefdeadbeefdeadbeefdeadbeefdeadbeef
Bug: 123
`: `abc

Change-Id: 123
Grit-Amends: deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
Bug: 123
`} {
		got := SetGritCommit(in, h)
		if got != want {
			t.Errorf("setGritCommit(%s): got:\n%swant:\n%s", in, got, want)
		}
	}
}

func TestSaveSizes(t *testing.T) {
	tmp, err := os.CreateTemp("", "")
	if err != nil {
		t.Fatal(err)
	}
	in := map[plumbing.Hash]uint64{
		plumbing.NewHash("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"): 12341234,
		plumbing.NewHash("1eadbeefdeadbeefdeadbeefdeadbeefdeadbeef"): 22,
	}

	fn := tmp.Name()
	if err := saveSizes(fn, in); err != nil {
		t.Fatal(err)
	}
	out, err := readSizes(fn)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("got %v want %v", out, in)
	}

	h := plumbing.NewHash("2eadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	sz := uint64(42)

	if err := saveSizes(fn, map[plumbing.Hash]uint64{h: sz}); err != nil {
		t.Fatal(err)
	}

	in[h] = sz
	out, err = readSizes(fn)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("got %v want %v", out, in)
	}
}
