// Copyright 2023 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package repo

import (
	"os"
	"reflect"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

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
