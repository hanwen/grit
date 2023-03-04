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

package gritfs

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/gritfs/repo"
)

type WorkspacesNode struct {
	fs.Inode

	repo *repo.Repository
	cas  *CAS
}

func (n *WorkspacesNode) Repository() *repo.Repository {
	return n.repo
}

func NewWorkspacesNode(cas *CAS, repo *repo.Repository) (*WorkspacesNode, error) {
	n := &WorkspacesNode{
		repo: repo,
		cas:  cas,
	}

	return n, nil
}

func (r *WorkspacesNode) OnAdd(ctx context.Context) {
	if err := r.onAdd(ctx); err != nil {
		log.Printf("OnAdd: %v", err)
	}
}

const workspaceRefPrefix = "refs/grit/"

func WorkspaceNames(r *repo.Repository) ([]string, error) {
	it, err := r.References()
	if err != nil {
		return nil, err
	}
	defer it.Close()
	var names []string
	for {
		ref, err := it.Next()
		if err == io.EOF {
			break
		}

		wsName := string(ref.Name())
		if !strings.HasPrefix(wsName, workspaceRefPrefix) {
			continue
		}

		wsName = wsName[len(workspaceRefPrefix):]
		names = append(names, wsName)
	}
	return names, nil
}

func (r *WorkspacesNode) AddWorkspace(wsName string) error {
	if ch := r.GetChild(wsName); ch != nil {
		return fmt.Errorf("already have workspace %q", wsName)
	}

	wsRoot, err := NewRoot(r.cas, r.repo, wsName)
	if err != nil {
		return err
	}

	child := r.NewPersistentInode(context.Background(), wsRoot, fs.StableAttr{Mode: fuse.S_IFDIR})
	r.AddChild(wsName, child, true)
	return nil
}

func (r *WorkspacesNode) DelWorkspace(wsName string) error {
	if ch := r.GetChild(wsName); ch == nil {
		return fmt.Errorf("workspace %q does not exist", wsName)
	}
	r.RmChild(wsName)
	return r.repo.RemoveReference(plumbing.ReferenceName(workspaceRefPrefix + wsName))
}

func (n *WorkspacesNode) onAdd(ctx context.Context) error {
	names, err := WorkspaceNames(n.repo)
	if err != nil {
		return err
	}
	for _, nm := range names {
		if err := n.AddWorkspace(nm); err != nil {
			log.Printf("workspace %q: %v", nm, err)
		}
	}

	return nil
}
