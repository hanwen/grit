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

package gitutil

import (
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

func SaveBlob(st storer.EncodedObjectStorer, data []byte) (id plumbing.Hash, err error) {
	enc := st.NewEncodedObject()
	enc.SetType(plumbing.BlobObject)
	w, err := enc.Writer()
	if err != nil {
		return
	}
	if _, err := w.Write(data); err != nil {
		return id, err
	}
	if err := w.Close(); err != nil {
		return id, err
	}
	return st.SetEncodedObject(enc)
}

func SaveTree(st storer.EncodedObjectStorer, entries []object.TreeEntry) (id plumbing.Hash, err error) {
	SortTreeEntries(entries)

	enc := st.NewEncodedObject()
	enc.SetType(plumbing.TreeObject)

	t := object.Tree{Entries: entries}
	if err := t.Encode(enc); err != nil {
		return id, err
	}

	return st.SetEncodedObject(enc)
}

func SaveCommit(st storer.EncodedObjectStorer, c *object.Commit) (id plumbing.Hash, err error) {
	enc := st.NewEncodedObject()
	enc.SetType(plumbing.CommitObject)
	if err := c.Encode(enc); err != nil {
		return id, err
	}
	return st.SetEncodedObject(enc)
}

func ModifyCommit(st storer.EncodedObjectStorer, c *object.Commit, newContent map[string]string, message string) (id plumbing.Hash, err error) {
	tree, err := object.GetTree(st, c.TreeHash)

	es, err := TestMapToEntries(st, newContent)
	if err != nil {
		return id, err
	}

	treeID, err := PatchTree(st, tree, es)
	if err != nil {
		return id, err
	}

	newCommit := object.Commit{
		Message:      message,
		TreeHash:     treeID,
		ParentHashes: []plumbing.Hash{c.Hash},
	}

	return SaveCommit(st, &newCommit)
}
