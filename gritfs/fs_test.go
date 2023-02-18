// Copyright 2023 Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gritfs

import (
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
