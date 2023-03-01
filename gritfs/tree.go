package gritfs

import (
	"context"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/gritfs/gitutil"
)

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

		var r snapshotResult
		if repo, ok := ops.(*RepoNode); ok {
			// RepoNode.snapshot did this in parallel already
			r.Hash = repo.ID()
			r.TS = repo.idTime // todo - locking
		} else {
			r, err = ops.snapshot(wsUpdate)
			if err != nil {
				return result, err
			}
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

	n.treeID, err = n.root.repo.SaveTree(se)
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
