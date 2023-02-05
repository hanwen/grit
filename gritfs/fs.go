// Copyright 2023 Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gritfs

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type BlobNode struct {
	fs.Inode

	root *RepoNode

	// mutable metadata
	mu         sync.Mutex
	mode       filemode.FileMode
	size       uint64
	id         plumbing.Hash
	linkTarget []byte

	// If opened, filedesc for the open file. Also protected by mu
	backingFile string
	backingFd   int
	openCount   int
}

var _ = (Node)((*BlobNode)(nil))

func (n *BlobNode) GetRepoNode() *RepoNode {
	return n.root
}

func (n *BlobNode) ID() (plumbing.Hash, error) {
	return n.id, nil
}

func (n *BlobNode) DirMode() filemode.FileMode {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.mode
}

func (n *BlobNode) GetTreeNode() *TreeNode {
	return nil
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
	obj, err := n.root.repo.BlobObject(n.id)
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

	return n.root.cas.Write(n.id, data)
}

var _ = (fs.NodeGetattrer)((*BlobNode)(nil))

func (n *BlobNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.mu.Lock()
	defer n.mu.Unlock()
	out.Size = n.size
	out.Mode = uint32(n.mode)

	return 0
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
	n.id = id
	n.size = uint64(sz)
	log.Println("new hash is", id)
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
			log.Printf("mf.save: %v", err)
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
	if n.id != zero {
		f, ok := n.root.cas.Open(n.id)
		if !ok {
			if err := n.expandBlob(); err != nil {
				log.Printf("load: %v", err)
			} else {
				f, ok = n.root.cas.Open(n.id)
			}
		}
		if !ok {
			return fmt.Errorf("can't materialize %s", n.id)
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

type Node interface {
	ID() (plumbing.Hash, error)
	DirMode() filemode.FileMode
	GetRepoNode() *RepoNode
	GetTreeNode() *TreeNode
}

type TreeNode struct {
	fs.Inode

	root *RepoNode
}

var _ = (Node)((*TreeNode)(nil))

func (n *TreeNode) GetRepoNode() *RepoNode {
	return n.root
}

func (n *TreeNode) GetTreeNode() *TreeNode {
	return n
}

// c&p from go-git

type sortableEntries []object.TreeEntry

func (sortableEntries) sortName(te object.TreeEntry) string {
	if te.Mode == filemode.Dir {
		return te.Name + "/"
	}
	return te.Name
}
func (se sortableEntries) Len() int               { return len(se) }
func (se sortableEntries) Less(i int, j int) bool { return se.sortName(se[i]) < se.sortName(se[j]) }
func (se sortableEntries) Swap(i int, j int)      { se[i], se[j] = se[j], se[i] }

func SortTreeEntries(es []object.TreeEntry) {
	se := sortableEntries(es)
	sort.Sort(se)
}

func (n *TreeNode) DirMode() filemode.FileMode {
	return filemode.Dir
}

func (n *TreeNode) TreeEntries() ([]object.TreeEntry, error) {
	var se []object.TreeEntry
	for k, v := range n.Children() {
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
	SortTreeEntries(se)

	return se, nil
}

// ID computes the hash of the tree on the fly. We could cache this,
// but invalidating looks hard.
func (n *TreeNode) ID() (id plumbing.Hash, err error) {
	entries, err := n.TreeEntries()
	if err != nil {
		return id, err
	}
	t := object.Tree{Entries: entries}

	enc := n.root.repo.Storer.NewEncodedObject()
	enc.SetType(plumbing.TreeObject)
	if err := t.Encode(enc); err != nil {
		return id, err
	}

	id, err = n.root.repo.Storer.SetEncodedObject(enc)
	return id, err
}

var _ = (fs.NodeCreater)((*TreeNode)(nil))

func (n *TreeNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (node *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	if mode&0111 != 0 {
		mode = 0755 | fuse.S_IFREG
	} else {
		mode = 0644 | fuse.S_IFREG
	}
	bn := &BlobNode{
		root: n.root,
		mode: filemode.FileMode(mode),
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
	return child, fh, 0, 0
}

////////////////////////////////////////////////////////////////

type RepoNode struct {
	TreeNode

	repo     *git.Repository
	cas      *CAS
	id       plumbing.Hash
	repoPath string
	repoURL  *url.URL

	commit *object.Commit
}

var _ = (Node)((*RepoNode)(nil))

func (n *RepoNode) Repository() *git.Repository {
	return n.repo
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

func (r *RepoNode) StoreCommit(c *object.Commit) error {
	enc := r.repo.Storer.NewEncodedObject()
	enc.SetType(plumbing.CommitObject)
	if err := c.Encode(enc); err != nil {
		return err
	}

	_, err := r.repo.Storer.SetEncodedObject(enc)
	if err != nil {
		return err
	}

	// decode the object again so it has a Storer reference.
	c, err = object.DecodeCommit(r.repo.Storer, enc)
	if err != nil {
		return err
	}

	log.Printf("new commit %v for tree %v", c.Hash, c.TreeHash)
	r.commit = c
	return nil
}

func (r *RepoNode) ID() (plumbing.Hash, error) {
	lastTree := r.commit.TreeHash
	treeID, err := r.TreeNode.ID()
	if err != nil {
		return plumbing.ZeroHash, err
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
		r.StoreCommit(&c)
	}

	return r.commit.Hash, nil
}

func (r *RepoNode) modules() (*config.Modules, error) {
	ch := r.GetChild(".gitmodules")
	if ch == nil {
		return nil, nil
	}

	blob, ok := ch.Operations().(*BlobNode)
	if !ok {
		return nil, fmt.Errorf(".gitmodules is not a blob")
	}

	if err := blob.materialize(); err != nil {
		return nil, err
	}

	defer blob.unmaterialize()

	data, err := ioutil.ReadFile(blob.backingFile)
	if err != nil {
		return nil, err
	}

	mods := config.NewModules()
	if err := mods.Unmarshal(data); err != nil {
		return nil, err
	}

	return mods, nil
}

func maybeFetchCommit(repo *git.Repository, id plumbing.Hash, repoURL *url.URL) (*object.Commit, error) {
	commit, err := repo.CommitObject(id)
	if commit != nil {
		return commit, nil
	}

	if err == plumbing.ErrObjectNotFound {
		if _, err := repo.Remote("grit-origin"); err == git.ErrRemoteNotFound {
			_, err := repo.CreateRemote(&config.RemoteConfig{
				Name: "grit-origin",
				URLs: []string{repoURL.String()},
			})
			if err != nil {
				return nil, err
			}
		}
		opt := git.FetchOptions{
			RemoteName: "grit-origin",
			Depth:      1,
			Progress:   os.Stderr,
			Tags:       git.NoTags,
			RefSpecs:   []config.RefSpec{config.RefSpec(fmt.Sprintf("+%s:refs/fetch", id))},
		}
		if err = repo.Fetch(&opt); err != nil {
			return nil, err
		}
	}
	return repo.CommitObject(id)
}

func NewRoot(cas *CAS, repo *git.Repository,
	repoPath string,
	id plumbing.Hash, repoURL *url.URL) (fs.InodeEmbedder, error) {

	commit, err := maybeFetchCommit(repo, id, repoURL)
	if err != nil {
		return nil, fmt.Errorf("CommitObject(%v): %v", id, err)
	}
	repoPath, err = filepath.Abs(repoPath)
	if err != nil {
		return nil, err
	}

	root := &RepoNode{
		repo:     repo,
		cas:      cas,
		id:       id,
		repoPath: repoPath,
		repoURL:  repoURL,
		commit:   commit,
	}
	root.root = root

	return root, nil
}

func (r *RepoNode) newGitBlobNode(ctx context.Context, mode filemode.FileMode, obj *object.Blob) (*fs.Inode, error) {
	bn := &BlobNode{
		root: r,
		id:   obj.ID(),
		mode: mode,
		size: uint64(obj.Size),
	}
	if mode == filemode.Symlink {
		rc, err := obj.Reader()
		if err != nil {
			return nil, err
		}

		defer rc.Close()
		data, err := ioutil.ReadAll(rc)
		if err != nil {
			return nil, err
		}
		bn.linkTarget = data
	}
	return r.NewPersistentInode(ctx, bn, fs.StableAttr{Mode: uint32(mode)}), nil
}

func (r *RepoNode) newGitTreeNode(ctx context.Context, tree *object.Tree, nodePath string) (*fs.Inode, error) {
	treeNode := &TreeNode{
		root: r,
	}

	node := r.NewPersistentInode(ctx, treeNode, fs.StableAttr{Mode: fuse.S_IFDIR})
	return node, r.addGitTree(ctx, node, nodePath, tree)
}

func (r *RepoNode) newSubmoduleNode(ctx context.Context, mods *config.Modules, path string, id plumbing.Hash) (*fs.Inode, error) {
	var submod *config.Submodule
	for _, m := range mods.Submodules {
		if m.Path == path {
			submod = m
			break
		}
	}
	if submod == nil {
		return nil, fmt.Errorf("submodule %q unknown", path)
	}

	repoPath := filepath.Join(r.repoPath, "modules", submod.Name)
	subRepo, err := git.PlainOpen(repoPath)
	if err != nil {
		repoPath = filepath.Join(r.repoPath, "modules", strings.Replace(submod.Name, "/", "%2f", -1))
		subRepo, err = git.PlainOpen(repoPath)
	}
	if err == git.ErrRepositoryNotExists {
		subRepo, err = git.PlainInit(repoPath, true)
	}
	if err != nil {
		log.Printf("opening/creating %q: %v", repoPath, err)
		return nil, err
	}

	subURL, err := r.repoURL.Parse(submod.URL)
	if err != nil {
		return nil, err
	}

	ops, err := NewRoot(r.cas, subRepo, repoPath, id, subURL)
	if err != nil {
		return nil, err
	}

	return r.NewPersistentInode(ctx, ops, fs.StableAttr{Mode: fuse.S_IFDIR}), nil
}

// addGitTree adds entries under tree to node.
func (r *RepoNode) addGitTree(ctx context.Context, node *fs.Inode, nodePath string, tree *object.Tree) error {
	var mods *config.Modules
	for _, e := range tree.Entries {
		path := filepath.Join(nodePath, e.Name)
		var child *fs.Inode
		var err error
		if e.Mode == filemode.Submodule {
			if mods == nil {
				mods, err = r.modules()
				if err != nil {
					log.Printf(".gitmodules error %s", err)
					continue
				}
			}
			child, err = r.newSubmoduleNode(ctx, mods, path, e.Hash)
			if err != nil {
				log.Printf("submodule %q: %v", path, err)
				continue
			}
		} else {
			child, err = r.newGitNode(ctx, e.Mode, e.Hash, filepath.Join(nodePath, e.Name))
			if err != nil {
				return err
			}
		}

		node.AddChild(e.Name, child, true)

		if e.Mode == filemode.Submodule {
			child.AddChild(".grit",
				r.NewPersistentInode(ctx,
					&fs.MemSymlink{Data: []byte(strings.Repeat("../", strings.Count(path, "/")+1) + ".grit")},
					fs.StableAttr{Mode: fuse.S_IFLNK}), true)
		}
	}

	return nil
}

func (r *RepoNode) newGitNode(ctx context.Context, mode filemode.FileMode, id plumbing.Hash, nodePath string) (*fs.Inode, error) {
	obj, err := r.repo.Object(plumbing.AnyObject, id)
	if err != nil {
		return nil, err
	}

	switch o := obj.(type) {
	case *object.Tree:
		return r.newGitTreeNode(ctx, o, nodePath)
	case *object.Blob:
		return r.newGitBlobNode(ctx, mode, o)
	default:
		return nil, fmt.Errorf("unsupported")
	}
}

var _ = (fs.NodeOnAdder)((*RepoNode)(nil))

func (r *RepoNode) OnAdd(ctx context.Context) {
	c := r.commit
	tree, err := r.repo.TreeObject(c.TreeHash)
	if err != nil {
		log.Fatalf("TreeObject %s: %v", c.TreeHash, err)
	}
	if err := r.addGitTree(ctx, &r.Inode, "", tree); err != nil {
		log.Fatalf("addGitTree: %v", err)
	}
}
