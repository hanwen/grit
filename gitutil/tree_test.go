// Copyright 2023 Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gitutil

import (
	"fmt"
	"io"
	"reflect"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"
)

func dumpTree(t *object.Tree) ([]object.TreeEntry, error) {
	iter := t.Files()

	var entries []object.TreeEntry
	for {
		fi, err := iter.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		entries = append(entries,
			object.TreeEntry{
				Name: fi.Name,
				Mode: fi.Mode,
				Hash: fi.Hash,
			})
	}
	return entries, nil
}

func checkTree(t *testing.T, eos storer.EncodedObjectStorer, tree *object.Tree, in []object.TreeEntry, want []object.TreeEntry) *object.Tree {
	id, err := PatchTree(eos, tree, in)
	if err != nil {
		t.Fatal(err)
	}
	newTree, err := object.GetTree(eos, id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := dumpTree(newTree)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checkTree(%v): got %v, want %v", in, got, want)
	}

	return newTree
}

func TestPatchTree(t *testing.T) {
	storer := memory.NewStorage()

	var ids []plumbing.Hash
	for i := 0; i < 10; i++ {
		id, err := SaveBlob(storer, []byte(fmt.Sprintf("%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	tree := &object.Tree{}
	tree = checkTree(t, storer, tree, []object.TreeEntry{
		{Name: "a", Mode: filemode.Regular, Hash: ids[0]},
		{Name: "b/c", Mode: filemode.Regular, Hash: ids[1]},
	}, []object.TreeEntry{
		{Name: "a", Mode: filemode.Regular, Hash: ids[0]},
		{Name: "b/c", Mode: filemode.Regular, Hash: ids[1]},
	})
	tree = checkTree(t, storer, tree, []object.TreeEntry{
		{Name: "a/q", Mode: filemode.Regular, Hash: ids[3]},            // file -> dir
		{Name: "b/c", Mode: filemode.Regular, Hash: plumbing.ZeroHash}, // dir becomes empty
	}, []object.TreeEntry{
		{Name: "a/q", Mode: filemode.Regular, Hash: ids[3]},
	})
	tree = checkTree(t, storer, tree, []object.TreeEntry{
		{Name: "a", Mode: filemode.Regular, Hash: ids[4]}, // dir -> file
	}, []object.TreeEntry{
		{Name: "a", Mode: filemode.Regular, Hash: ids[4]},
	})
}
