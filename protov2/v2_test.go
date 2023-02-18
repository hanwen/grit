package protov2

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/hanwen/gritfs/gitutil"
)

func TestObjectInfo(t *testing.T) {
	tmp := t.TempDir()

	repoDir := tmp + "/repo"

	content := "hello world"
	tr, err := gitutil.SetupTestRepo(repoDir,
		map[string]string{
			"file": content,
			"g":    strings.Repeat("x", 1025),
			"h":    strings.Repeat("x", 1023),
		})

	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}

	gitutil.ServeGit(tmp, l)

	addr := fmt.Sprintf("http://%s/repo", l.Addr())

	cl, err := NewClient(addr)
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
		Want: []plumbing.Hash{tr.CommitID},
		// ? doesnt work?
		//		Filter: "blob:limit=1024",
	}
	if err := cl.Fetch(destRepo.Storer, &opts); err != nil {
		t.Fatal(err)
	}

	medBlobID := tr.FileIDs["h"]
	if _, err := destRepo.BlobObject(medBlobID); err != nil {
		t.Fatal(err)
	}
	if _, err := destRepo.TreeObject(tr.TreeID); err != nil {
		t.Fatal(err)
	}
}
