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
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/cgi"
	"os/exec"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
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
	Listener net.Listener
	Name     string
	RepoURL  string

	FileIDs  map[string]plumbing.Hash
	TreeID   plumbing.Hash
	CommitID plumbing.Hash
}

func (tr *TestRepo) Serve(root string) error {
	var err error
	tr.Listener, err = net.Listen("tcp", "localhost:0")
	if err != nil {
		return err
	}

	ServeGit(root, tr.Listener)
	tr.RepoURL = fmt.Sprintf("http://%s/%s", tr.Listener.Addr(), tr.Name)
	return nil
}

func (tr *TestRepo) Close() {
	tr.Listener.Close()
}

// TestMapToEntries provides input to PatchTree. keys are filenames, with
// suffixes:
// * '!' = delete
// * '*' = executable
// * '@' = symlink
// * '#' = submodule.
func TestMapToEntries(st storer.EncodedObjectStorer, in map[string]string) ([]object.TreeEntry, error) {
	var es []object.TreeEntry
	for k, v := range in {
		id, err := SaveBlob(st, []byte(v))
		if err != nil {
			return nil, err
		}
		mode := filemode.Regular

		last := k[len(k)-1]
		trim := true
		switch last {
		case '*':
			mode = filemode.Executable
		case '@':
			mode = filemode.Symlink
		case '!':
			id = plumbing.ZeroHash
		case '#':
			mode = filemode.Submodule
			id = plumbing.NewHash(v)
		default:
			trim = false
		}
		if trim {
			k = k[:len(k)-1]
		}

		es = append(es, object.TreeEntry{Name: k, Hash: id, Mode: mode})
	}
	return es, nil
}

func SetupTestRepo(root, name string, fileContents map[string]string) (*TestRepo, error) {
	tr := &TestRepo{
		FileIDs: map[string]plumbing.Hash{},
		Name:    name,
	}

	var err error
	tr.Repo, err = git.PlainInit(filepath.Join(root, name), true)
	if err != nil {
		return nil, err
	}

	cfg, err := tr.Repo.Config()
	if err != nil {
		return nil, err
	}
	cfg.Raw.AddOption("uploadpack", "", "allowfilter", "1")
	cfg.Raw.AddOption("uploadpack", "", "allowanysha1inwant", "1")
	tr.Repo.SetConfig(cfg)
	if err != nil {
		return nil, err
	}

	es, err := TestMapToEntries(tr.Repo.Storer, fileContents)
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		tr.FileIDs[e.Name] = e.Hash
	}
	t := &object.Tree{}
	tr.TreeID, err = PatchTree(tr.Repo.Storer, t, es)
	if err != nil {
		return nil, err
	}
	c := object.Commit{
		Message:  "SetupTestRepo",
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
