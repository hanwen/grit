package protov2

import (
	"bytes"
	"fmt"
	"net"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/hanwen/gritfs/gitutil"
)

func TestObjectInfo(t *testing.T) {
	tmp := t.TempDir()

	repoDir := tmp + "/repo"
	repo, err := git.PlainInit(repoDir, true)
	if err != nil {
		t.Fatal(err)
	}

	content := []byte("blablabla")
	blobID, err := gitutil.SaveBlob(repo.Storer, content)
	if err != nil {
		t.Fatal(err)
	}

	longBlobID, err := gitutil.SaveBlob(repo.Storer,
		bytes.Repeat([]byte("x"), 1025))
	medBlobID, err := gitutil.SaveBlob(repo.Storer,
		bytes.Repeat([]byte("x"), 1023))
	if err != nil {
		t.Fatal(err)
	}

	treeID, err := gitutil.SaveTree(repo.Storer,
		[]object.TreeEntry{
			{Name: "file", Mode: filemode.Executable, Hash: blobID},
			{Name: "g", Mode: filemode.Executable, Hash: longBlobID},
			{Name: "h", Mode: filemode.Executable, Hash: medBlobID},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	c := object.Commit{
		Message:  "msg",
		TreeHash: treeID,
	}
	id, err := gitutil.SaveCommit(repo.Storer, &c)
	if err != nil {
		t.Fatal(err)
	}

	ref := plumbing.NewHashReference("refs/heads/main", id)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatal(err)
	}

	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}

	serveGit(tmp, l)

	addr := fmt.Sprintf("http://%s/repo", l.Addr())

	cl, err := NewClient(addr)
	if err != nil {
		t.Fatal(err)
	}

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
		Want: []plumbing.Hash{id},
		// ? doesnt work?
		//		Filter: "blob:limit=1024",
	}
	if err := cl.Fetch(destRepo.Storer, &opts); err != nil {
		t.Fatal(err)
	}

	if _, err := destRepo.BlobObject(medBlobID); err != nil {
		t.Fatal(err)
	}
	if _, err := destRepo.TreeObject(treeID); err != nil {
		t.Fatal(err)
	}
}
