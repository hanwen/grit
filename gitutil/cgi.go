package gitutil

import (
	"log"
	"net"
	"net/http"
	"net/http/cgi"
	"os/exec"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

var gitExecDir string

func init() {
	cmd := exec.Command("git", "--exec-path")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Fatalf("git --exec-path: %v", err)
	}
	gitExecDir = strings.TrimSpace(string(out))
}

// from go-git common_test.go
func ServeGit(root string, l net.Listener) {
	server := &http.Server{
		Handler: &cgi.Handler{
			Path: gitExecDir + "/git-http-backend",
			Dir:  root,
			Env: []string{
				"GIT_HTTP_EXPORT_ALL=true",
				"GIT_PROJECT_ROOT=" + root,
			},
		},
	}
	go server.Serve(l)
}

type TestRepo struct {
	Repo     *git.Repository
	FileIDs  map[string]plumbing.Hash
	TreeID   plumbing.Hash
	CommitID plumbing.Hash
}

func SetupTestRepo(dir string, fileContents map[string]string) (*TestRepo, error) {
	tr := &TestRepo{
		FileIDs: map[string]plumbing.Hash{},
	}

	var err error
	tr.Repo, err = git.PlainInit(dir, true)
	if err != nil {
		return nil, err
	}

	var es []object.TreeEntry
	for k, v := range fileContents {
		id, err := SaveBlob(tr.Repo.Storer, []byte(v))
		if err != nil {
			return nil, err
		}
		mode := filemode.Regular
		if strings.HasSuffix(k, "*") {
			k = k[:len(k)-1]
			mode = filemode.Executable
		} else if strings.HasSuffix(k, "@") {
			k = k[:len(k)-1]
			mode = filemode.Symlink
		}
		tr.FileIDs[k] = id
		es = append(es, object.TreeEntry{Name: k, Hash: id, Mode: mode})
	}

	t := &object.Tree{}
	tr.TreeID, err = PatchTree(tr.Repo.Storer, t, es)
	if err != nil {
		return nil, err
	}
	c := object.Commit{
		Message:  "msg",
		TreeHash: tr.TreeID,
	}

	tr.CommitID, err = SaveCommit(tr.Repo.Storer, &c)
	if err != nil {
		return nil, err
	}

	ref := plumbing.NewHashReference("refs/heads/main", tr.CommitID)
	if err := tr.Repo.Storer.SetReference(ref); err != nil {
		return nil, err
	}

	return tr, nil
}
