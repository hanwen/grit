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
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/user"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/grit/repo"
)

var emptyBlob = plumbing.NewHash("e69de29bb2d1d6434b8b29ae775ad8c2e48c5391")
var emptyTree = plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904")

type RepoNode struct {
	TreeNode

	cas *CAS

	workspaceName string
	repo          *repo.Repository
	commit        *object.Commit

	// calculation timestamp for commit.Hash
	idTime time.Time

	mu           sync.Mutex
	materialized bool
}

var _ = (Node)((*RepoNode)(nil))

func (n *RepoNode) Repository() *repo.Repository {
	return n.repo

}

func (n *RepoNode) WorkspaceName() string {
	return n.workspaceName

}
func (n *RepoNode) FitsMode(mode filemode.FileMode) bool {
	return mode == filemode.Submodule
}

func (r *RepoNode) DirMode() filemode.FileMode {
	return filemode.Submodule
}

var mySig object.Signature
var sysSig object.Signature

func init() {
	u, _ := user.Current()

	mySig.Name = u.Name
	mySig.Email = fmt.Sprintf("%s@localhost", u.Username)

	hn, err := os.Hostname()
	if err != nil {
		hn = "localhost.localdomain"
	}

	sysSig.Name = "Grit daemon"
	sysSig.Email = "grit+noreply" + hn
}

// Returns the commit currently stored in the repo node; does not
// recompute. Use Snapshot() for that.
func (r *RepoNode) GetCommit() object.Commit {
	return *r.commit
}

func (r *RepoNode) StoreCommit(c *object.Commit, wsUpdate *WorkspaceUpdate) error {
	before := r.commit

	commitID, err := r.repo.SaveCommit(c)
	// decode the object again so it has a Storer reference.
	c, err = r.repo.CommitObject(commitID)
	if err != nil {
		return err
	}

	if before == nil || commitID != before.Hash {
		if r.Path(nil) != "" {
			log.Printf("%s: new commit %v for tree %v", r.Path(nil), c.Hash, c.TreeHash)
		}
		r.commit = c
		r.idTime = wsUpdate.TS

		if err := r.recordWorkspaceChange(c, wsUpdate); err != nil {
			return err
		}
	}

	return nil
}

func (r *RepoNode) recordWorkspaceChange(after *object.Commit, wsUpdate *WorkspaceUpdate) error {
	nowSig := mySig
	nowSig.When = time.Now()
	var before *object.Commit
	refname := r.workspaceRef()
	wsRef, err := r.repo.Reference(refname, true)

	wsTree := &object.Tree{}
	if err == plumbing.ErrReferenceNotFound {
		err = nil
	} else if err != nil {
		return err
	} else {
		wsCommit, err := r.repo.CommitObject(wsRef.Hash())
		if err != nil {
			return err
		}
		wsTree, err = r.repo.TreeObject(wsCommit.TreeHash)
		if err != nil {
			return err
		}

		before, err = wsCommit.Parent(wsCommit.NumParents() - 1)
		if err != nil {
			return err
		}
	}

	if before != nil && before.Hash == after.Hash {
		return nil
	}
	wsBlob, err := json.Marshal(&wsUpdate.NewState)
	if err != nil {
		return err
	}
	wsBlobID, err := r.repo.SaveBytes(wsBlob)
	if err != nil {
		return err
	}

	entries := []object.TreeEntry{{Name: after.Hash.String(), Hash: wsBlobID}}
	if wsUpdate.Amend && before != nil {
		entries = append(entries, object.TreeEntry{
			Name: before.Hash.String(),
		})
	}

	afterTreeID, err := r.repo.PatchTree(wsTree, entries)
	if err != nil {
		return err
	}

	nowSysSig := sysSig
	nowSysSig.When = time.Now()
	wsCommit := &object.Commit{
		Message:   wsUpdate.Message, // TODO - should serialize wsUpdate to json?
		TreeHash:  afterTreeID,
		Author:    nowSysSig,
		Committer: nowSysSig,
	}
	if wsRef != nil {
		wsCommit.ParentHashes = append(wsCommit.ParentHashes, wsRef.Hash())
	}
	wsCommit.ParentHashes = append(wsCommit.ParentHashes, after.Hash)
	wsCommitID, err := r.repo.SaveCommit(wsCommit)
	if err != nil {
		return err
	}
	wsRef = plumbing.NewHashReference(refname, wsCommitID)
	if err := r.repo.SetReference(wsRef); err != nil {
		return err
	}
	return nil
}

func (r *RepoNode) ID() plumbing.Hash {
	return r.commit.Hash
}

func (r *RepoNode) snapshot(wsUpdate *WorkspaceUpdate) (result snapshotResult, err error) {
	_, prevState, err := r.ReadWorkspaceCommit()
	if wsUpdate == nil {
		wsUpdate = &WorkspaceUpdate{
			Message: "Snapshot",
			NewState: WorkspaceState{
				AutoSnapshot: true,
			},
		}

		if prevState.AutoSnapshot {
			wsUpdate.Amend = true
		}
	}

	if submods, err := r.currentSubmods(r.commit, true); err != nil {
		return result, err
	} else {
		type subSnapResult struct {
			result snapshotResult
			err    error
		}

		out := make(chan subSnapResult, len(submods))
		for _, submodState := range submods {
			go func(ss *submoduleState) {
				rs, err := ss.node.snapshot(wsUpdate)
				out <- subSnapResult{
					result: rs,
					err:    err,
				}
			}(submodState)
		}
		for range submods {
			ssr := <-out
			if ssr.err != nil {
				return result, ssr.err
			}

			result.Recomputed += ssr.result.Recomputed
		}
	}

	lastTree := r.commit.TreeHash
	treeUpdate, err := r.TreeNode.snapshot(wsUpdate)
	if err != nil {
		return result, err
	}

	if lastTree != treeUpdate.Hash {
		c := *r.commit

		if wsUpdate.NewState.AutoSnapshot {
			mySig.When = time.Now()
			ts := time.Now().Format(time.RFC822Z)
			c = object.Commit{
				Message: fmt.Sprintf(
					`Snapshot created %v for tree %v`, ts, treeUpdate.Hash),
				Author:    mySig,
				Committer: mySig,
				TreeHash:  treeUpdate.Hash,
			}
		}

		if wsUpdate.Amend {
			c.ParentHashes = r.commit.ParentHashes
		} else {
			c.ParentHashes = []plumbing.Hash{r.commit.Hash}
		}

		if err := r.StoreCommit(&c, wsUpdate); err != nil {
			return result, err
		}
		treeUpdate.Recomputed++
	}

	result = snapshotResult{
		Hash:       r.commit.Hash,
		TS:         r.idTime,
		Recomputed: treeUpdate.Recomputed + result.Recomputed,
	}
	return result, nil
}

func (n *RepoNode) SetID(id plumbing.Hash, ts time.Time) error {
	state := setIDState{ts: ts}
	return n.setID(id, filemode.Submodule, &state)
}

func inodeWalk(n *fs.Inode, p string) *fs.Inode {
	cs := strings.Split(p, "/")
	for _, c := range cs {
		n = n.GetChild(c)
		if n == nil {
			break
		}
	}
	return n
}

func (n *RepoNode) currentSubmods(commit *object.Commit, filter bool) (map[string]*submoduleState, error) {
	cfg, err := n.repo.SubmoduleConfigByCommit(commit)
	if err != nil {
		return nil, err
	}

	result := map[string]*submoduleState{}
	for _, sm := range cfg.Submodules {
		subNode := inodeWalk(n.EmbeddedInode(), sm.Path)
		if subNode == nil {
			if !filter {
				result[sm.Path] = &submoduleState{
					config: sm,
				}
			}
			continue
		}
		repoNode, ok := subNode.Operations().(*RepoNode)
		if !ok {
			result[sm.Path] = &submoduleState{
				config: sm,
			}
			continue
		}
		smState := &submoduleState{
			node:   repoNode,
			hash:   repoNode.commit.Hash,
			config: sm,
		}
		pname, parent := repoNode.Parent()
		smState.name = pname
		smState.parent = parent

		result[sm.Path] = smState
	}
	return result, nil
}

func (n *RepoNode) setID(id plumbing.Hash, mode filemode.FileMode, state *setIDState) error {
	if n.commit != nil && n.commit.Hash == id {
		return nil
	}

	commit, err := n.repo.CommitObject(id)
	if err != nil {
		return err
	}

	state.submodules, err = n.currentSubmods(commit, false)
	if err != nil {
		return err
	}

	if err := n.TreeNode.setID(commit.TreeHash, mode, state); err != nil {
		return err
	}

	var wg sync.WaitGroup
	for _, smState := range state.submodules {
		wg.Add(1)
		go func(ss *submoduleState) {
			defer wg.Done()

			if ss.parent != nil && ss.node == nil {
				subRepo, err := n.repo.OpenSubmodule(ss.config)
				if err != nil {
					ss.err = err
					return
				}

				ops, err := NewRoot(n.cas, subRepo, n.workspaceName)
				if err != nil {
					ss.err = err
					return
				}

				ss.node = ops
				child := n.NewPersistentInode(context.Background(), ops, fs.StableAttr{Mode: fuse.S_IFDIR})

				ss.parent.AddChild(ss.name, child, true)
			}
			if ss.node != nil {
				ss.err = ss.node.SetID(ss.hash, state.ts)
			}
		}(smState)
	}
	wg.Wait()

	for _, smState := range state.submodules {
		if smState.err != nil {
			return fmt.Errorf("module %q: %v", smState.config.Path, smState.err)
		}
	}

	var keys []plumbing.Hash
	for _, blob := range state.missingSizes {
		keys = append(keys, blob.blobID)
	}

	sizes, err := n.repo.ObjectSizes(keys)
	if err != nil {
		return err
	}
	for _, blobNode := range state.missingSizes {
		blobNode.size = sizes[blobNode.blobID]
	}

	if err := n.StoreCommit(commit, &WorkspaceUpdate{
		TS:      state.ts,
		Message: "SetID",
	}); err != nil {
		return err
	}
	return nil
}

func (n *RepoNode) Snapshot(upd *WorkspaceUpdate) (SnapshotResult, error) {
	var zeroTS time.Time
	if upd.TS == zeroTS {
		return SnapshotResult{}, fmt.Errorf("must set TS in WorkspaceUpdate")
	}
	r, err := n.snapshot(upd)
	if err != nil {
		return SnapshotResult{}, err
	}

	coc, wss, err := n.ReadWorkspaceCommit()
	return SnapshotResult{
		Recomputed: r.Recomputed,
		CheckedOut: coc,
		State:      wss,
	}, err
}

func NewRoot(cas *CAS, repo *repo.Repository, workspaceName string) (*RepoNode, error) {
	root := &RepoNode{
		workspaceName: workspaceName,
		repo:          repo,
		cas:           cas,
		idTime:        time.Now(),
	}
	root.root = root

	_, err := repo.Reference(root.workspaceRef(), true)
	if err == plumbing.ErrReferenceNotFound {
		err = nil
		if err := root.initializeWorkspace(); err != nil {
			return nil, err
		}
	}

	return root, err
}

func (r *RepoNode) workspaceRef() plumbing.ReferenceName {
	return plumbing.ReferenceName("refs/grit/" + r.workspaceName)
}

// Reads the workspace state from storage
func (r *RepoNode) ReadWorkspaceCommit() (commit *object.Commit, state *WorkspaceState, err error) {
	ref, err := r.repo.Reference(r.workspaceRef(), true)
	if err != nil {
		return nil, nil, err
	}
	wsCommit, err := r.repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, nil, err
	}

	commit, err = wsCommit.Parent(wsCommit.NumParents() - 1)
	if err != nil {
		return nil, nil, err
	}

	f, err := wsCommit.File(commit.Hash.String())
	if err != nil {
		return nil, nil, err
	}
	state = &WorkspaceState{}
	contents, err := f.Contents()
	if err != nil {
		return nil, nil, err
	}

	if err := json.Unmarshal([]byte(contents), state); err != nil {
		return nil, nil, err
	}

	return commit, state, nil
}

func (r *RepoNode) initializeWorkspace() error {
	treeID, err := r.repo.SaveTree(nil)
	if err != nil {
		return err
	}

	nowSig := mySig
	nowSig.When = time.Now()
	emptyCommit := &object.Commit{
		Message:   "initial commit",
		TreeHash:  treeID,
		Committer: nowSig,
		Author:    nowSig,
	}
	commitID, err := r.repo.SaveCommit(emptyCommit)
	if err != nil {
		return err
	}
	emptyCommit.Hash = commitID
	upd := &WorkspaceUpdate{
		TS:      time.Now(),
		Message: "initialize workspace",
	}
	if err := r.recordWorkspaceChange(emptyCommit, upd); err != nil {
		return err
	}
	return nil
}

func (r *RepoNode) newGitBlobNode(mode filemode.FileMode) (Node, error) {
	bn := &BlobNode{
		root: r,
		mode: mode,
	}
	return bn, nil
}

func (r *RepoNode) newGitTreeNode() (Node, error) {
	ts := time.Now()
	treeNode := &TreeNode{
		root:    r,
		modTime: ts,
	}

	return treeNode, nil
}

func (r *RepoNode) newGitNode(mode filemode.FileMode) (Node, error) {
	switch mode {
	case filemode.Dir:
		return r.newGitTreeNode()
	case filemode.Executable, filemode.Regular, filemode.Symlink:
		return r.newGitBlobNode(mode)
	default:
		return nil, fmt.Errorf("unsupported mode %v", mode)
	}
}

var _ = (fs.NodeLookuper)((*RepoNode)(nil))
var _ = (fs.NodeReaddirer)((*RepoNode)(nil))

func (r *RepoNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.materialize()

	child := r.GetChild(name)
	if child == nil {
		return nil, syscall.ENOENT
	}

	if ga, ok := child.Operations().(fs.NodeGetattrer); ok {
		var a fuse.AttrOut
		errno := ga.Getattr(ctx, nil, &a)
		if errno == 0 {
			out.Attr = a.Attr
		}
	}
	return child, 0
}

func (r *RepoNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.materialize()
	res := []fuse.DirEntry{}
	for k, ch := range r.Children() {
		res = append(res, fuse.DirEntry{Mode: ch.Mode(),
			Name: k,
			Ino:  ch.StableAttr().Ino})
	}
	return fs.NewListDirStream(res), 0
}

func (r *RepoNode) materialize() error {
	if r.materialized {
		return nil
	}
	// todo - should be in OnAdd
	want, _, err := r.ReadWorkspaceCommit()
	if err != nil {
		return err
	}
	err = r.SetID(want.Hash, time.Now())
	r.materialized = true
	return err
}
