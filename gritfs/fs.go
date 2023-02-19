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

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/gritfs/gitutil"
	"github.com/hanwen/gritfs/repo"
)

type Node interface {
	fs.InodeEmbedder

	idTS(*WorkspaceUpdate) (plumbing.Hash, time.Time, error)
	ID() (plumbing.Hash, error)
	DirMode() filemode.FileMode
	GetRepoNode() *RepoNode
	GetTreeNode() *TreeNode
	FitsMode(filemode.FileMode) bool
	SetID(plumbing.Hash, filemode.FileMode, time.Time) error
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

func (n *BlobNode) idTS(*WorkspaceUpdate) (plumbing.Hash, time.Time, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.blobID, n.modTime, nil
}

func (n *BlobNode) ID() (plumbing.Hash, error) {
	upd := &WorkspaceUpdate{
		Message: "ID call",
	}
	id, _, err := n.idTS(upd)
	return id, err
}

func (n *BlobNode) SetID(id plumbing.Hash, mode filemode.FileMode, ts time.Time) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.blobID = id
	n.modTime = ts
	n.mode = mode

	ok := true
	n.size, ok = n.root.repo.CachedBlobSize(id)
	if !ok {
		return fmt.Errorf("size missing: %v", id)
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
	enc := n.root.repo.Storer.NewEncodedObject()
	enc.SetType(plumbing.BlobObject)
	w, err := enc.Writer()
	if err != nil {
		return err
	}

	f, err := os.Open(n.backingFile)
	if err != nil {
		return err
	}
	defer f.Close()
	sz, err := io.Copy(w, f)
	if err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	id, err := n.root.repo.Storer.SetEncodedObject(enc)
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.blobID = id
	n.size = uint64(sz)
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

func (n *TreeNode) SetID(id plumbing.Hash, mode filemode.FileMode, ts time.Time) error {
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
	for _, e := range tree.Entries {
		child := n.GetChild(e.Name)
		if child != nil {
			node, ok := child.Operations().(Node)
			if ok && node.FitsMode(e.Mode) {
				if err := node.SetID(e.Hash, e.Mode, ts); err != nil {
					return err
				}
				continue
			}
		}

		childOps, err := n.root.newGitNode(e.Mode, e.Hash, filepath.Join(nodePath, e.Name))
		if err != nil {
			return err
		}

		fsMode := uint32(e.Mode)
		if fsMode == uint32(filemode.Submodule) {
			fsMode = fuse.S_IFDIR
		}
		child = n.NewPersistentInode(context.Background(), childOps, fs.StableAttr{Mode: fsMode})
		n.AddChild(e.Name, child, true)
		if err := childOps.SetID(e.Hash, e.Mode, ts); err != nil {

			return err
		}
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.modTime = ts
	n.idTime = ts
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

		id, err := ops.ID()
		if err != nil {
			return nil, err
		}

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

func (n *TreeNode) ID() (id plumbing.Hash, err error) {
	upd := &WorkspaceUpdate{
		Message: "ID",
	}
	id, _, err = n.idTS(upd)
	return id, err
}

func (n *TreeNode) idTS(wsUpdate *WorkspaceUpdate) (id plumbing.Hash, idTime time.Time, err error) {
	startTS := time.Now()
	children := n.Children()

	uptodate := n.modTime.Before(n.idTime) && n.treeID != plumbing.ZeroHash

	se := make([]object.TreeEntry, 0, len(children))
	for nm, node := range children {
		ops, ok := node.Operations().(Node)
		if !ok {
			continue
		}

		id, idTS, err := ops.idTS(wsUpdate)
		if err != nil {
			return id, idTime, err
		}

		e := object.TreeEntry{
			Name: nm,
			Mode: ops.DirMode(),
			Hash: id,
		}
		se = append(se, e)

		if idTS.After(n.idTime) {
			uptodate = false
		}
	}

	if uptodate {
		return n.treeID, n.idTime, nil
	}

	n.treeID, err = gitutil.SaveTree(n.root.repo.Storer, se)
	n.idTime = startTS

	return n.treeID, n.idTime, err
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

type RepoNode struct {
	TreeNode

	cas *CAS

	repo   *repo.Repository
	commit *object.Commit

	// calculation timestamp for commit.Hash
	idTime time.Time
}

var _ = (Node)((*RepoNode)(nil))

func (n *RepoNode) Repository() *repo.Repository {
	return n.repo
}

func (n *RepoNode) FitsMode(mode filemode.FileMode) bool {
	return mode == filemode.Submodule
}

func IsGritCommit(c *object.Commit) bool {
	idx := strings.LastIndex(c.Message, "\n\n")
	if idx == -1 {
		return false
	}

	return strings.Contains(c.Message[idx:], "\nGrit-Amends: ")
}

func SetGritCommit(msg string, h plumbing.Hash) string {
	footerIdx := strings.LastIndex(msg, "\n\n")
	var lines []string
	body := msg
	if footerIdx > 0 {
		lines = strings.Split(msg[footerIdx+2:], "\n")
		body = msg[:footerIdx]
	}
	newLine := "Grit-Amends: " + h.String()
	if h == plumbing.ZeroHash {
		newLine = ""
	}
	seen := false

	var newLines []string
	for _, line := range lines {
		if strings.HasPrefix(line, "Grit-Amends: ") {
			line = newLine
			seen = true
		}
		if line == "" {
			continue
		}
		newLines = append(newLines, line)
	}
	if !seen {
		newLines = append(newLines, newLine)
	}
	return body + "\n\n" + strings.Join(newLines, "\n") + "\n"
}

func (r *RepoNode) DirMode() filemode.FileMode {
	return filemode.Submodule
}

var mySig object.Signature

func init() {
	u, _ := user.Current()

	mySig.Name = u.Name
	mySig.Email = fmt.Sprintf("%s@localhost", u.Username)
}

// Returns the commit currently stored in the repo node; does not
// recompute. Use ID() for that.
func (r *RepoNode) GetCommit() object.Commit {
	return *r.commit
}

func (r *RepoNode) StoreCommit(c *object.Commit, startTS time.Time, wsUpdate *WorkspaceUpdate) error {
	before := r.commit

	commitID, err := gitutil.SaveCommit(r.repo.Storer, c)
	// decode the object again so it has a Storer reference.
	c, err = r.repo.CommitObject(commitID)
	if err != nil {
		return err
	}

	log.Printf("%s: new commit %v for tree %v", r.Path(nil), c.Hash, c.TreeHash)
	if before == nil || commitID != before.Hash {
		r.commit = c
		r.idTime = startTS

		if err := r.recordWorkspaceChange(before, c, wsUpdate); err != nil {
			return err
		}
	}

	return nil
}

type WorkspaceState struct {
	Conflict bool
}

type WorkspaceUpdate struct {
	Message string
	Command []string
	Amend   bool

	NewState WorkspaceState
}

func (r *RepoNode) recordWorkspaceChange(before, after *object.Commit, wsUpdate *WorkspaceUpdate) error {
	nowSig := mySig
	nowSig.When = time.Now()

	refname := plumbing.ReferenceName("refs/grit/ws")
	wsRef, err := r.repo.Reference(refname, true)
	if err == plumbing.ErrReferenceNotFound {
		treeID, err := gitutil.SaveTree(r.repo.Storer, nil)
		if err != nil {
			return err
		}

		wsCommit := &object.Commit{
			Message:      "initial workspace",
			TreeHash:     treeID,
			Committer:    nowSig,
			Author:       nowSig,
			ParentHashes: []plumbing.Hash{}, // before.Hash
		}
		commitID, err := gitutil.SaveCommit(r.repo.Storer, wsCommit)
		if err != nil {
			return err
		}

		wsRef = plumbing.NewHashReference(refname, commitID)
		if err := r.repo.Storer.SetReference(wsRef); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	wsCommit, err := r.repo.CommitObject(wsRef.Hash())
	if err != nil {
		return err
	}
	wsTree, err := r.repo.TreeObject(wsCommit.TreeHash)
	if err != nil {
		return err
	}

	state := &WorkspaceState{}
	wsBlob, err := json.Marshal(state)
	if err != nil {
		return err
	}
	wsBlobID, err := gitutil.SaveBlob(r.repo.Storer, wsBlob)
	if err != nil {
		return err
	}

	entries := []object.TreeEntry{{Name: after.Hash.String(), Hash: wsBlobID}}
	if wsUpdate.Amend {
		entries = append(entries, object.TreeEntry{
			Name: before.Hash.String(),
		})
	}

	afterTreeID, err := gitutil.PatchTree(r.repo.Storer, wsTree, entries)
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("update: %s", wsUpdate.Command)
	wsCommit = &object.Commit{
		Message:      msg,
		TreeHash:     afterTreeID,
		ParentHashes: []plumbing.Hash{wsRef.Hash(), after.Hash},
		Author:       nowSig,
		Committer:    nowSig,
	}
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

func (r *RepoNode) ID() (plumbing.Hash, error) {
	id, _, err := r.idTS(&WorkspaceUpdate{Message: "ID call"})
	return id, err
}

func (r *RepoNode) idTS(wsUpdate *WorkspaceUpdate) (plumbing.Hash, time.Time, error) {
	startTS := time.Now()

	lastTree := r.commit.TreeHash
	treeID, err := r.TreeNode.ID()
	var zeroTS time.Time
	if err != nil {
		return plumbing.ZeroHash, zeroTS, err
	}

	if lastTree != treeID {
		c := *r.commit
		if IsGritCommit(r.commit) {
			// amend commit
			c.TreeHash = treeID
			c.Message = SetGritCommit(r.commit.Message, r.commit.Hash)
		} else {
			mySig.When = time.Now()
			ts := time.Now().Format(time.RFC822Z)
			c = object.Commit{
				Message: SetGritCommit(fmt.Sprintf(
					`Snapshot originally created %v for tree %v`, ts, treeID), r.commit.Hash),
				Author:       mySig,
				Committer:    mySig,
				TreeHash:     treeID,
				ParentHashes: []plumbing.Hash{r.commit.Hash},
			}
		}
		if err := r.StoreCommit(&c, startTS, wsUpdate); err != nil {
			return plumbing.ZeroHash, time.Time{}, err
		}
	}

	return r.commit.Hash, r.idTime, nil
}

func (n *RepoNode) SetID(id plumbing.Hash, mode filemode.FileMode, ts time.Time) error {
	commit, err := n.repo.CommitObject(id)
	if err != nil {
		return err
	}

	err = n.TreeNode.SetID(commit.TreeHash, mode, ts)
	n.StoreCommit(commit, ts, &WorkspaceUpdate{
		Message: "SetID",
	})
	return err
}

func NewRoot(cas *CAS, repo *repo.Repository, commit *object.Commit) (*RepoNode, error) {
	root := &RepoNode{
		repo:   repo,
		cas:    cas,
		commit: commit,
		idTime: time.Now(),
	}
	root.root = root
	return root, nil
}

func (r *RepoNode) newGitBlobNode(mode filemode.FileMode, id plumbing.Hash) (Node, error) {
	bn := &BlobNode{
		root:    r,
		blobID:  id,
		mode:    mode,
		modTime: time.Now(),
	}
	if sz, ok := r.repo.CachedBlobSize(id); !ok {
		return nil, fmt.Errorf("%s: blob %s has no size", r.repo, id)
	} else {
		bn.size = sz
	}
	return bn, nil
}

func (r *RepoNode) newGitTreeNode(id plumbing.Hash, nodePath string) (Node, error) {
	ts := time.Now()
	treeNode := &TreeNode{
		root:    r,
		modTime: ts,
		treeID:  id,
		idTime:  ts,
	}

	return treeNode, nil
}

func (r *RepoNode) newSubmoduleNode(id plumbing.Hash, path string) (Node, error) {
	subRepo, err := r.repo.SubmoduleByPath(r.commit, path)
	if err != nil {
		return nil, err
	}
	subCommit, err := subRepo.CommitObject(id)
	if err != nil {
		return nil, err
	}
	ops, err := NewRoot(r.cas, subRepo, subCommit)
	if err != nil {
		return nil, err
	}
	return ops, nil
}

func (r *RepoNode) newGitNode(mode filemode.FileMode, id plumbing.Hash, nodePath string) (Node, error) {
	switch mode {
	case filemode.Dir:
		return r.newGitTreeNode(id, nodePath)
	case filemode.Submodule:
		return r.newSubmoduleNode(id, nodePath)
	case filemode.Executable, filemode.Regular, filemode.Symlink:
		return r.newGitBlobNode(mode, id)
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
	return r.TreeNode.SetID(r.commit.TreeHash, filemode.Dir, time.Now())
}
