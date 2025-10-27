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

package protov2

import (
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/hanwen/grit/gitutil"
)

func TestObjectInfo(t *testing.T) {
	tmp := t.TempDir()

	content := "hello world"
	tr, err := gitutil.SetupTestRepo(tmp, "repo",
		map[string]string{
			"file": content,
			"g":    strings.Repeat("x", 1025),
			"h":    strings.Repeat("x", 1023),
		})
	tr.Serve(tmp)
	t.Cleanup(tr.Close)

	cl, err := NewClient(tr.RepoURL)
	if err != nil {
		t.Fatal(err)
	}
	blobID := tr.FileIDs["file"]
	res, err := cl.ObjectInfo([]plumbing.Hash{blobID})
	if err != nil {
		t.Fatal(err)
	}

	if got, want := res[blobID], len(content); got != uint64(want) {
		t.Errorf("got %d want %d", got, want)
	}

	destDir := tmp + "/dest"
	destRepo, err := git.PlainInit(destDir, true)
	if err != nil {
		t.Fatal(err)
	}

	opts := FetchOptions{
		Want:   []plumbing.Hash{tr.CommitID},
		Filter: "blob:limit=1024",
	}
	if err := cl.Fetch(destRepo.Storer, &opts); err != nil {
		t.Fatal(err)
	}

	medBlobID := tr.FileIDs["h"]
	if _, err := destRepo.BlobObject(medBlobID); err != nil {
		t.Fatal(err)
	}
	largeBlobID := tr.FileIDs["g"]
	if _, err := destRepo.BlobObject(largeBlobID); err == nil {
		t.Fatalf("found %v, want ErrNotFound", largeBlobID)
	}
	if _, err := destRepo.TreeObject(tr.TreeID); err != nil {
		t.Fatal(err)
	}
}
