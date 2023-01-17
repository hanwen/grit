// Copyright 2023 Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package glitfs

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

func TestSetGlitCommit(t *testing.T) {
	h := plumbing.NewHash("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	for in, want := range map[string]string{`abc

Change-Id: 123`: `abc

Change-Id: 123
Glit-Amends: deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
`, `abc

Change-Id: 123
Glit-Amends: 1eadbeefdeadbeefdeadbeefdeadbeefdeadbeef
Bug: 123
`: `abc

Change-Id: 123
Glit-Amends: deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
Bug: 123
`} {
		got := setGlitCommit(in, h)
		if got != want {
			t.Errorf("setGlitCommit(%s): got:\n%swant:\n%s", in, got, want)
		}
	}
}
