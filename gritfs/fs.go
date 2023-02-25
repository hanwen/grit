// Copyright 2023 Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gritfs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/gritfs/gitutil"
	"github.com/hanwen/gritfs/repo"
)

var emptyBlob = plumbing.NewHash("e69de29bb2d1d6434b8b29ae775ad8c2e48c5391")
var emptyTree = plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904")

// snapshotResult is the result for the internal snapshot method, computing a SHA1.
type snapshotResult struct {
	// Number of hashes recomputed. Used for verifying incremental updates
	Recomputed int

	// TS for the hash computed below.
	TS   time.Time
	Hash plumbing.Hash
}

// SnapshotResult is the result for the public RepoNode.Snapshot method.
type SnapshotResult struct {
	Recomputed int
	CheckedOut *object.Commit
	State      *WorkspaceState
}

// Properties of a checked out commit
type WorkspaceState struct {
	AutoSnapshot bool
}

// Input to calculating a Snapshot
type WorkspaceUpdate struct {
	// Must be acquired as early as possible to avoid loosing
	// updates in a next run of Snapshot
	TS       time.Time
	Message  string
	Amend    bool
	NewState WorkspaceState
}

type Node interface {
	fs.InodeEmbedder

	snapshot(*WorkspaceUpdate) (snapshotResult, error)

	// Return the last calculated Hash. Does not trigger a new snapshot.
	ID() plumbing.Hash
	DirMode() filemode.FileMode
	GetRepoNode() *RepoNode
	GetTreeNode() *TreeNode
	FitsMode(filemode.FileMode) bool
	setID(plumbing.Hash, filemode.FileMode, *setIDState) error
}

////////////////////////////////////////////////////////////////

type BlobNode struct {
	fs.Inode

	root *RepoNode

	// mutable metadata
	mu      sync.Mutex
	mode    filemode.FileMode
	size    uint64
	blobID  plumbing.Hash
	modTime time.Time

	// If opened, filedesc for the open file. Also protected by mu
	backingFile string
	backingFd   int
	openCount   int
}

var _ = (Node)((*BlobNode)(nil))

func (n *BlobNode) GetRepoNode() *RepoNode {
	return n.root
}

func (n *BlobNode) snapshot(*WorkspaceUpdate) (snapshotResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	return snapshotResult{
		Hash: n.blobID,
		TS:   n.modTime,
	}, nil
}

func (n *BlobNode) ID() plumbing.Hash {
	return n.blobID
}

func (n *BlobNode) setID(id plumbing.Hash, mode filemode.FileMode, state *setIDState) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.blobID = id
	n.modTime = state.ts
	n.mode = mode

	ok := true
	n.size, ok = n.root.repo.CachedBlobSize(id)
	if !ok {
		state.missingSizes = append(state.missingSizes, n)
	}
	return nil
}

func (n *BlobNode) DirMode() filemode.FileMode {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.mode
}

func (n *BlobNode) GetTreeNode() *TreeNode {
	return nil
}

func (n *BlobNode) FitsMode(mode filemode.FileMode) bool {
	return mode == filemode.Regular || mode == filemode.Executable || mode == filemode.Symlink
}

var _ = (fs.NodeOpener)((*BlobNode)(nil))

func (n *BlobNode) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	// We have to always expand the file, because of {open(rdwr), setsize(sz=0)}.
	if err := n.materialize(); err != nil {
		return nil, 0, syscall.EIO
	}

	fd, err := syscall.Open(n.backingFile, int(flags), 0777)
	if err != nil {
		return nil, 0, err.(syscall.Errno)
	}

	fh = &openBlob{
		fileAllOps: fs.NewLoopbackFile(fd).(fileAllOps),
		flags:      flags,
	}

	return fh, 0, 0
}

func (n *BlobNode) setSize(sz uint64) error {
	if err := n.materialize(); err != nil {
		return err
	}
	defer n.unmaterialize()

	if err := os.Truncate(n.backingFile, int64(sz)); err != nil {
		return err
	}

	if err := n.saveToGit(); err != nil {
		return err
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.modTime = time.Now()

	return nil
}

var _ = (fs.NodeSetattrer)((*BlobNode)(nil))

func (n *BlobNode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if in.Valid&fuse.FATTR_SIZE != 0 {
		if err := n.setSize(in.Size); err != nil {
			return syscall.EIO
		}
	}
	return n.Getattr(ctx, f, out)
}

// expandBlob reads the blob from Git and saves into CAS.
func (n *BlobNode) expandBlob() error {
	obj, err := n.root.repo.BlobObject(n.blobID)
	if err != nil {
		return err
	}
	rc, err := obj.Reader()
	if err != nil {
		return err
	}
	defer rc.Close()
	data, err := ioutil.ReadAll(rc)
	if err != nil {
		return err
	}

	return n.root.cas.Write(n.blobID, data)
}

var _ = (fs.NodeGetattrer)((*BlobNode)(nil))

func (n *BlobNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.mu.Lock()
	defer n.mu.Unlock()

	out.Size = n.size
	out.Mode = uint32(n.mode)
	out.SetTimes(nil, &n.modTime, nil)
	return 0
}

var _ = (fs.NodeReadlinker)((*BlobNode)(nil))

func (n *BlobNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	obj, err := n.root.repo.BlobObject(n.blobID)
	if err != nil {
		return nil, syscall.EIO
	}

	r, err := obj.Reader()
	if err != nil {
		return nil, syscall.EIO
	}
	defer r.Close()

	content, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, syscall.EIO
	}

	return content, 0
}

type fileAllOps interface {
	Release(ctx context.Context) syscall.Errno
	Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno
	Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno)
	Write(ctx context.Context, data []byte, off int64) (written uint32, errno syscall.Errno)
	Getlk(ctx context.Context, owner uint64, lk *fuse.FileLock, flags uint32, out *fuse.FileLock) syscall.Errno
	Setlk(ctx context.Context, owner uint64, lk *fuse.FileLock, flags uint32) syscall.Errno
	Setlkw(ctx context.Context, owner uint64, lk *fuse.FileLock, flags uint32) syscall.Errno
	Lseek(ctx context.Context, off uint64, whence uint32) (uint64, syscall.Errno)
	Flush(ctx context.Context) syscall.Errno
	Fsync(ctx context.Context, flags uint32) syscall.Errno
	Setattr(ctx context.Context, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno
	Allocate(ctx context.Context, off uint64, size uint64, mode uint32) syscall.Errno
}

type openBlob struct {
	fileAllOps
	flags uint32
}

// save takes the file and saves it back into Git storage updating
// n.id and n.size
func (n *BlobNode) saveToGit() error {
	f, err := os.Open(n.backingFile)
	if err != nil {
		return err
	}
	defer f.Close()

	id, err := n.root.repo.SaveBlob(f)

	n.mu.Lock()
	defer n.mu.Unlock()
	n.blobID = id
	n.size, _ = n.root.repo.CachedBlobSize(id)
	n.modTime = time.Now()

	return nil
}

var _ = (fs.NodeFlusher)((*BlobNode)(nil))

func (n *BlobNode) Flush(ctx context.Context, fh fs.FileHandle) syscall.Errno {
	errno := fh.(fs.FileFlusher).Flush(ctx)
	if errno != 0 {
		return errno
	}

	of := fh.(*openBlob)
	if of.flags&(syscall.O_WRONLY|syscall.O_APPEND|syscall.O_RDWR) != 0 {
		if err := n.saveToGit(); err != nil {
			log.Printf("saveToGit: %v", err)
			return syscall.EIO
		}
	}
	return 0
}

var _ = (fs.NodeReleaser)((*BlobNode)(nil))

func (n *BlobNode) Release(ctx context.Context, fh fs.FileHandle) syscall.Errno {
	fh.(fs.FileReleaser).Release(ctx)
	n.unmaterialize()
	return 0
}

func (n *BlobNode) unmaterialize() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.openCount--
	if n.openCount < 0 {
		log.Fatal("underflow")
	}
	if n.openCount == 0 {
		syscall.Close(n.backingFd)
		syscall.Unlink(n.backingFile)
		n.backingFd = 0
		n.backingFile = ""
	}
}

// materialize creates a private fd for this inode. We cannot use the
// CAS file for this. If the file is opened read-only, the same file
// can be opened R/W and changes should reflect in the R/O file too.
func (n *BlobNode) materialize() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.backingFd > 0 {
		n.openCount++
		return nil
	}
	t, err := ioutil.TempFile("", "")
	if err != nil {
		return err
	}
	defer t.Close()

	var zero plumbing.Hash
	if n.blobID != zero {
		f, ok := n.root.cas.Open(n.blobID)
		if !ok {
			if err := n.expandBlob(); err != nil {
				log.Printf("load: %v", err)
			} else {
				f, ok = n.root.cas.Open(n.blobID)
			}
		}
		if !ok {
			return fmt.Errorf("can't materialize %s", n.blobID)
		}
		defer f.Close()

		if _, err := io.Copy(t, f); err != nil {
			return err
		}
		if err := t.Sync(); err != nil { //
			return err
		}

		if _, err := t.Seek(0, 0); err != nil {
			return err
		}
	}
	fd, err := syscall.Dup(int(t.Fd()))
	if err != nil {
		return err
	}

	n.backingFile = t.Name()
	n.backingFd = fd
	n.openCount = 1
	return nil
}

////////////////////////////////////////////////////////////////

type TreeNode struct {
	fs.Inode

	root *RepoNode

	mu      sync.Mutex
	modTime time.Time
	treeID  plumbing.Hash
	idTime  time.Time
}

func (n *TreeNode) FitsMode(mode filemode.FileMode) bool {
	return mode == filemode.Dir
}

func (n *TreeNode) setID(id plumbing.Hash, mode filemode.FileMode, state *setIDState) error {
	n.mu.Lock()
	tid := n.treeID
	n.mu.Unlock()
	if tid == id {
		return nil
	}

	tree, err := n.root.repo.TreeObject(id)
	if err != nil {
		return err
	}

	var remove []string
	for nm, child := range n.Children() {
		if _, ok := child.Operations().(Node); !ok {
			continue
		}
		entry, _ := tree.FindEntry(nm)
		if entry == nil {
			remove = append(remove, nm)
		}
	}
	n.RmChild(remove...)

	nodePath := n.Path(n.root.EmbeddedInode())

	loadEntry := func(name string, mode filemode.FileMode, hash plumbing.Hash) error {
		childOps, err := n.root.newGitNode(mode, filepath.Join(nodePath, name))
		if err != nil {
			return err
		}
		fsMode := uint32(mode)
		child := n.NewPersistentInode(context.Background(), childOps, fs.StableAttr{Mode: fsMode})
		n.AddChild(name, child, true)
		return childOps.setID(hash, mode, state)
	}

	for _, e := range tree.Entries {
		if e.Mode == filemode.Submodule {
			path := filepath.Join(nodePath, e.Name)
			smState := state.submodules[path]
			smState.hash = e.Hash
			smState.parent = n.EmbeddedInode()
			smState.name = e.Name
			continue
		}
		child := n.GetChild(e.Name)
		if child != nil {
			node, ok := child.Operations().(Node)
			if ok && node.FitsMode(e.Mode) {
				if err := node.setID(e.Hash, e.Mode, state); err != nil {
					return err
				}
				continue
			}
		}

		if err := loadEntry(e.Name, e.Mode, e.Hash); err != nil {
			return err
		}
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.modTime = state.ts
	n.idTime = state.ts
	n.treeID = id
	return nil
}

func (n *TreeNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.mu.Lock()
	defer n.mu.Unlock()

	out.Mode = fuse.S_IFDIR
	out.SetTimes(nil, &n.modTime, nil)
	return 0
}

var _ = (Node)((*TreeNode)(nil))

func (n *TreeNode) GetRepoNode() *RepoNode {
	return n.root
}

func (n *TreeNode) GetTreeNode() *TreeNode {
	return n
}

// c&p from go-git

func (n *TreeNode) DirMode() filemode.FileMode {
	return filemode.Dir
}

func (n *TreeNode) TreeEntries() ([]object.TreeEntry, error) {
	children := n.Children()
	se := make([]object.TreeEntry, 0, len(children))
	for k, v := range children {
		ops, ok := v.Operations().(Node)
		if !ok {
			continue
		}

		id := ops.ID()
		e := object.TreeEntry{
			Name: k,
			Mode: ops.DirMode(),
			Hash: id,
		}
		se = append(se, e)
	}
	gitutil.SortTreeEntries(se)

	return se, nil
}

func (n *TreeNode) ID() plumbing.Hash {
	return n.treeID
}

func (n *TreeNode) snapshot(wsUpdate *WorkspaceUpdate) (result snapshotResult, err error) {
	children := n.Children()

	uptodate := !n.modTime.After(n.idTime) && n.treeID != plumbing.ZeroHash
	se := make([]object.TreeEntry, 0, len(children))
	for nm, node := range children {
		ops, ok := node.Operations().(Node)
		if !ok {
			continue
		}

		r, err := ops.snapshot(wsUpdate)
		if err != nil {
			return result, err
		}
		result.Recomputed += r.Recomputed
		e := object.TreeEntry{
			Name: nm,
			Mode: ops.DirMode(),
			Hash: r.Hash,
		}
		se = append(se, e)

		if r.TS.After(n.idTime) {
			uptodate = false
		}
	}

	if uptodate {
		return snapshotResult{
			Hash: n.treeID,
			TS:   n.idTime,
		}, nil
	}

	n.treeID, err = gitutil.SaveTree(n.root.repo.Storer, se)
	n.idTime = wsUpdate.TS

	result.Recomputed++
	result.Hash = n.treeID
	result.TS = wsUpdate.TS
	return result, err
}

var _ = (fs.NodeUnlinker)((*TreeNode)(nil))

func (n *TreeNode) Unlink(ctx context.Context, name string) syscall.Errno {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.modTime = time.Now()
	return 0
}

var _ = (fs.NodeRmdirer)((*TreeNode)(nil))

func (n *TreeNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.modTime = time.Now()
	return 0
}

var _ = (fs.NodeCreater)((*TreeNode)(nil))

func (n *TreeNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (node *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	if mode&0111 != 0 {
		mode = 0755 | fuse.S_IFREG
	} else {
		mode = 0644 | fuse.S_IFREG
	}
	bn := &BlobNode{
		root:    n.root,
		mode:    filemode.FileMode(mode),
		modTime: time.Now(),
	}

	if err := bn.materialize(); err != nil {
		errno = syscall.EIO
		return
	}

	fd, err := syscall.Open(bn.backingFile, int(flags), 0777)
	if err != nil {
		errno = err.(syscall.Errno)
		return
	}

	child := n.NewPersistentInode(ctx, bn, fs.StableAttr{})
	n.AddChild(name, child, true)
	fh = &openBlob{
		fileAllOps: fs.NewLoopbackFile(fd).(fileAllOps),
		flags:      flags,
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.modTime = time.Now()

	return child, fh, 0, 0
}

////////////////////////////////////////////////////////////////

type submoduleState struct {
	node   *RepoNode
	parent *fs.Inode
	name   string
	hash   plumbing.Hash

	config *config.Submodule
	err    error
}

type setIDState struct {
	// submodules
	submodules map[string]*submoduleState

	missingSizes []*BlobNode
	ts           time.Time
}

type RepoNode struct {
	TreeNode

	cas *CAS

	workspaceName string
	repo          *repo.Repository
	commit        *object.Commit

	// calculation timestamp for commit.Hash
	idTime time.Time
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

	commitID, err := gitutil.SaveCommit(r.repo.Storer, c)
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

		if wsCommit.NumParents() > 1 {
			before, err = wsCommit.Parent(1)
			if err != nil {
				return err
			}
		}
	}

	wsBlob, err := json.Marshal(&wsUpdate.NewState)
	if err != nil {
		return err
	}
	wsBlobID, err := gitutil.SaveBlob(r.repo.Storer, wsBlob)
	if err != nil {
		return err
	}

	entries := []object.TreeEntry{{Name: after.Hash.String(), Hash: wsBlobID}}
	if wsUpdate.Amend && before != nil {
		entries = append(entries, object.TreeEntry{
			Name: before.Hash.String(),
		})
	}

	afterTreeID, err := gitutil.PatchTree(r.repo.Storer, wsTree, entries)
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
	wsCommitID, err := gitutil.SaveCommit(r.repo.Storer, wsCommit)
	if err != nil {
		return err
	}
	wsRef = plumbing.NewHashReference(refname, wsCommitID)
	if err := r.repo.Storer.SetReference(wsRef); err != nil {
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
		Recomputed: treeUpdate.Recomputed,
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

func (n *RepoNode) currentSubmods(commit *object.Commit) (map[string]*submoduleState, error) {
	cfg, err := n.repo.SubmoduleConfig(commit)
	if err != nil {
		return nil, err
	}

	result := map[string]*submoduleState{}
	for _, sm := range cfg.Submodules {
		subNode := inodeWalk(n.EmbeddedInode(), sm.Path)
		if subNode == nil {
			result[sm.Path] = &submoduleState{
				config: sm,
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
	if n.commit.Hash == id {
		return nil
	}

	commit, err := n.repo.CommitObject(id)
	if err != nil {
		return err
	}

	state.submodules, err = n.currentSubmods(commit)
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
		if err := root.initializeWorkspace(); err != nil {
			return nil, err
		}
	}

	// todo - should be in OnAdd
	root.commit, _, err = root.ReadWorkspaceCommit()
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
	treeID, err := gitutil.SaveTree(r.repo.Storer, nil)
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
	commitID, err := gitutil.SaveCommit(r.repo.Storer, emptyCommit)
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

func (r *RepoNode) newGitTreeNode(nodePath string) (Node, error) {
	ts := time.Now()
	treeNode := &TreeNode{
		root:    r,
		modTime: ts,
	}

	return treeNode, nil
}

func (r *RepoNode) newGitNode(mode filemode.FileMode, nodePath string) (Node, error) {
	switch mode {
	case filemode.Dir:
		return r.newGitTreeNode(nodePath)
	case filemode.Executable, filemode.Regular, filemode.Symlink:
		return r.newGitBlobNode(mode)
	default:
		return nil, fmt.Errorf("unsupported mode %v %q", mode, nodePath)
	}
}

var _ = (fs.NodeOnAdder)((*RepoNode)(nil))

func (r *RepoNode) OnAdd(ctx context.Context) {
	if err := r.onAdd(ctx); err != nil {
		log.Printf("OnAdd: %v", err)
	}
}

func (r *RepoNode) onAdd(ctx context.Context) error {
	want := r.commit.Hash
	r.commit.Hash = plumbing.ZeroHash
	return r.SetID(want, time.Now())
}
