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

func newTestRoot(t *testing.T, repoURL string) *Root {
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

	root, err := NewCommandServer(cas, gritRepo, "ws")
	if err != nil {
		t.Fatal(err)
	}
	return root
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

	ioc := &testIOC{}
	exit, err := RunCommand([]string{"checkout", tr.CommitID.String()}, "", ioc, root.RepoNode)
	if exit != 0 || err != nil {
		t.Errorf("exit %d, %v", exit, err)
	}

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
		ioc = &testIOC{}
		exit, err = RunCommand([]string{"log", "-n", "1"}, "", ioc, root.RepoNode)
		if exit != 0 || err != nil {
			t.Errorf("exit %d, %v", exit, err)
		}

		if got, want := ioc.String(), "commit "+tr.CommitID.String(); !strings.Contains(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	}

	// try a write, hopefully triggering parallelism.
	if err := ioutil.WriteFile(mntDir+"/file.txt", []byte("blabla"), 0644); err != nil {
		t.Fatal(err)
	}

	ioc = &testIOC{}
	exit, err = RunCommand([]string{"log", "-n", "1"}, "", ioc, root.RepoNode)
	if exit != 0 || err != nil {
		t.Errorf("exit %d, %v", exit, err)
	}

	update := &gritfs.WorkspaceUpdate{
		Message:  "bla",
		NewState: gritfs.WorkspaceState{AutoSnapshot: true},
	}
	savedCommit, _, err := root.RepoNode.Snapshot(update)
	if err != nil {
		t.Fatal(err)
	}

	commit, readState, err := root.RepoNode.ReadWorkspaceCommit()
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

	{
		ioc := &testIOC{}
		exit, err := RunCommand([]string{"checkout", tr.CommitID.String()}, "", ioc, root.RepoNode)
		if exit != 0 || err != nil {
			t.Errorf("exit %d, %v", exit, err)
		}
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
		NewState: gritfs.WorkspaceState{AutoSnapshot: true},
	}
	savedCommit, _, err := root.RepoNode.Snapshot(update)
	if err != nil {
		t.Fatal(err)
	}

	if savedCommit.Hash == tr.CommitID {
		t.Errorf("writing file should have changed commit ID")
	}

	{
		ioc := &testIOC{}
		exit, err := RunCommand([]string{"checkout", tr.CommitID.String()}, "", ioc, root.RepoNode)
		if exit != 0 || err != nil {
			t.Errorf("exit %d, %v", exit, err)
		}
	}
}
