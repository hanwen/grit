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

package server

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/gritfs/gitutil"
	"github.com/hanwen/gritfs/gritfs"
	"github.com/hanwen/gritfs/repo"
)

type testIOC struct {
	bytes.Buffer
}

func (t *testIOC) Edit(name string, data []byte) ([]byte, error) {
	return nil, fmt.Errorf("can't edit in test")
}

func newTestRoot(t *testing.T, repoURL string) *gritfs.RepoNode {
	casDir := t.TempDir()
	cas, err := gritfs.NewCAS(casDir)
	if err != nil {
		t.Fatal(err)
	}

	repoDir := t.TempDir()
	gitRepo, err := git.PlainInit(repoDir, true)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(repoURL)
	if err != nil {
		t.Fatal(err)
	}
	gritRepo, err := repo.NewRepo(gitRepo, repoDir, u)
	if err != nil {
		t.Fatal(err)
	}

	repoNode, err := gritfs.NewRoot(cas, gritRepo, "ws")
	if err != nil {
		t.Fatal(err)
	}
	return repoNode
}

func mountTest(t *testing.T, root fs.InodeEmbedder) string {
	mntDir := t.TempDir()
	server, err := fs.Mount(mntDir, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			//			Debug: true,
		},
		UID: uint32(os.Getuid()),
		GID: uint32(os.Getgid()),
	})
	if err != nil {
		log.Fatal("Mount", err)
	}
	t.Cleanup(func() { server.Unmount() })
	return mntDir
}

func TestFS(t *testing.T) {
	srcRoot := t.TempDir()

	input := map[string]string{"a": "hello world",
		"b/c":   "xyz",
		"b/d":   "pqr",
		"large": strings.Repeat("x", 20000),
	}
	tr, err := gitutil.SetupTestRepo(srcRoot, "repo", input)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Serve(srcRoot); err != nil {
		t.Fatal(err)
	}

	root := newTestRoot(t, tr.RepoURL)

	mntDir := mountTest(t, root)

	testCommand(t, []string{"checkout", tr.CommitID.String()}, "", root)

	for k, want := range input {
		fn := filepath.Join(mntDir, k)
		content, err := ioutil.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != want {
			t.Errorf("got %q want %q", content, want)
		}
	}

	for i := 0; i < 2; i++ {
		out := testCommand(t, []string{"log", "-n", "1"}, "", root)
		if got, want := string(out), "commit "+tr.CommitID.String(); !strings.Contains(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	}

	// try a write, hopefully triggering parallelism.
	if err := ioutil.WriteFile(mntDir+"/file.txt", []byte("blabla"), 0644); err != nil {
		t.Fatal(err)
	}

	out := testCommand(t, []string{"log", "-n", "1", "-p"}, "", root)

	if got, want := string(out), "+blabla\n"; !strings.Contains(got, want) {
		t.Errorf("got %s, want substring %q", got, want)
	}
	update := &gritfs.WorkspaceUpdate{
		Message:  "bla",
		TS:       time.Now(),
		NewState: gritfs.WorkspaceState{AutoSnapshot: true},
	}
	res, err := root.Snapshot(update)
	savedCommit := res.CheckedOut
	if err != nil {
		t.Fatal(err)
	}

	commit, readState, err := root.ReadWorkspaceCommit()
	if err != nil {
		t.Fatal(err)
	}
	if commit.Hash != savedCommit.Hash {
		t.Errorf("got %v want %v", commit, savedCommit)
	}

	parent, err := commit.Parent(0)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := parent.Hash, tr.CommitID; got != want {
		t.Fatalf("got %s want %s", parent, want)
	}

	if !reflect.DeepEqual(&update.NewState, readState) {
		t.Fatalf("got %v want %v", readState, &update.NewState)
	}
}

func TestSubmodules(t *testing.T) {
	srcRoot := t.TempDir()
	sub1files := map[string]string{
		"f1":    "f1content",
		"d1/f1": "f1content'",
	}

	sub1, err := gitutil.SetupTestRepo(srcRoot, "sub1", sub1files)

	if err != nil {
		t.Fatal(err)
	}
	sub2files := map[string]string{
		"f2":    "f2content",
		"d2/f2": "f2content'",
	}
	sub2, err := gitutil.SetupTestRepo(srcRoot, "sub2", sub2files)
	if err != nil {
		t.Fatal(err)
	}

	superProject := map[string]string{
		".gitmodules": `[submodule "sub1"]
  path = sub1
  url = ../sub1
  branch = .

[submodule "sub2"]
  path = sub2
  url = ../sub2
  branch = .

[submodule "sub3"]
  path = sub3
  url = ../sub2
  branch = .
`,
		"a":     "xyz",
		"b/c":   "xyz",
		"b/d":   "pqr",
		"sub1#": sub1.CommitID.String(),
		"sub2#": sub2.CommitID.String(),
	}
	tr, err := gitutil.SetupTestRepo(srcRoot, "repo", superProject)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Serve(srcRoot); err != nil {
		t.Fatal(err)
	}

	root := newTestRoot(t, tr.RepoURL)
	mntDir := mountTest(t, root)

	testCommand(t, []string{"checkout", tr.CommitID.String()}, "", root)

	if fi, err := os.Stat(filepath.Join(mntDir, "sub1")); err != nil {
		t.Fatalf("stat: %v", err)
	} else if !fi.IsDir() {
		t.Fatalf("not a dir: %v", fi)
	}

	for k, v := range sub1files {
		c, err := ioutil.ReadFile(filepath.Join(mntDir, "sub1", k))
		if err != nil {
			t.Fatal(err)
		}

		if string(c) != v {
			t.Errorf("got %q want %q", c, v)
		}
	}

	if err := ioutil.WriteFile(mntDir+"/sub1/file.txt", []byte("blabla"), 0644); err != nil {
		t.Fatal(err)
	}

	update := &gritfs.WorkspaceUpdate{
		Message:  "bla",
		TS:       time.Now(),
		NewState: gritfs.WorkspaceState{AutoSnapshot: true},
	}
	res, err := root.Snapshot(update)
	if err != nil {
		t.Fatal(err)
	}
	savedCommit := res.CheckedOut

	if savedCommit.Hash == tr.CommitID {
		t.Errorf("writing file should have changed commit ID")
	}

	out := testCommand(t, []string{"log", "-p"}, "", root)
	if got, want := string(out), "+blabla\n"; !strings.Contains(got, want) {
		t.Errorf("got %s, want substr %q", got, want)
	}
	testCommand(t, []string{"checkout", tr.CommitID.String()}, "", root)
}

func TestSnapshot(t *testing.T) {
	srcRoot := t.TempDir()

	input := map[string]string{"a": "hello world",
		"b/c/d": "xyz",
		"b/c/e": "abc",
		"b/c/f": "pqr",
	}
	tr, err := gitutil.SetupTestRepo(srcRoot, "repo", input)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Serve(srcRoot); err != nil {
		t.Fatal(err)
	}

	root := newTestRoot(t, tr.RepoURL)
	mntDir := mountTest(t, root)

	testCommand(t, []string{"checkout", tr.CommitID.String()}, "", root)

	time.Sleep(time.Microsecond) // ensure clock advances
	res, err := root.Snapshot(&gritfs.WorkspaceUpdate{
		Message:  "snapshot call",
		NewState: gritfs.WorkspaceState{AutoSnapshot: true},
	})
	if res.Recomputed != 0 {
		t.Fatalf("got %d want %d", res.Recomputed, 0)
	}

	if err := ioutil.WriteFile(mntDir+"/b/c/d", []byte("different"), 0644); err != nil {
		t.Fatal(err)
	}

	if res, err := root.Snapshot(&gritfs.WorkspaceUpdate{
		Message:  "snapshot call",
		TS:       time.Now(),
		NewState: gritfs.WorkspaceState{AutoSnapshot: true},
	}); err != nil {
		t.Fatal(err)
	} else if res.Recomputed != 4 {
		// 4 = "b/c", "b/", "", commit
		t.Fatalf("got %d want %d", res.Recomputed, 4)
	}
}

func testCommand(t *testing.T, args []string, dir string, root *gritfs.RepoNode) (out []byte) {
	tc := &testIOC{}
	call := Call{
		IOClientAPI: tc,
		Args:        args,
		Dir:         dir,
	}
	if err := InvokeRepoNode(&call, root.EmbeddedInode()); err != nil {
		t.Log(tc.String())
		t.Fatalf("%s; %v", args, err)
	}

	return tc.Bytes()
}

func TestRemount(t *testing.T) {
	srcRoot := t.TempDir()

	input := map[string]string{"a": "hello world",
		"b/c":   "xyz",
		"b/d":   "pqr",
		"large": strings.Repeat("x", 20000),
	}
	tr, err := gitutil.SetupTestRepo(srcRoot, "repo", input)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Serve(srcRoot); err != nil {
		t.Fatal(err)
	}

	casDir := t.TempDir()
	cas, err := gritfs.NewCAS(casDir)
	if err != nil {
		t.Fatal(err)
	}

	repoDir := t.TempDir()
	gitRepo, err := git.PlainInit(repoDir, true)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(tr.RepoURL)
	if err != nil {
		t.Fatal(err)
	}
	gritRepo, err := repo.NewRepo(gitRepo, repoDir, u)
	if err != nil {
		t.Fatal(err)
	}

	root, err := gritfs.NewRoot(cas, gritRepo, "ws")
	if err != nil {
		t.Fatal(err)
	}

	mntDir := t.TempDir()
	server, err := fs.Mount(mntDir, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			//			Debug: true,
		},
		UID: uint32(os.Getuid()),
		GID: uint32(os.Getgid()),
	})
	if err != nil {
		t.Fatal("Mount", err)
	}
	defer server.Unmount()

	out := testCommand(t, []string{"wslog"}, "", root)
	if strings.Count(string(out), "initial commit") != 1 {
		t.Errorf("got %q, want 'initial commit' once", out)
	}
	testCommand(t, []string{"checkout", tr.CommitID.String()}, "", root)
	content := []byte("different")
	if err := ioutil.WriteFile(mntDir+"/file.txt", content, 0644); err != nil {
		t.Fatal(err)
	}
	testCommand(t, []string{"snapshot"}, "", root)

	server.Unmount()
	root, err = gritfs.NewRoot(cas, gritRepo, "ws")
	if err != nil {
		t.Fatal(err)
	}
	server, err = fs.Mount(mntDir, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			//			Debug: true,
		},
		UID: uint32(os.Getuid()),
		GID: uint32(os.Getgid()),
	})
	if err != nil {
		t.Fatal("Mount", err)
	}
	defer server.Unmount()

	loopback, err := ioutil.ReadFile(mntDir + "/file.txt")
	if err != nil {
		t.Fatal("Mount", err)
	}

	if !bytes.Equal(loopback, content) {
		t.Fatalf("got %q want %q", loopback, content)
	}
}
