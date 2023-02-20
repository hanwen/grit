// Copyright 2023 Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/gritfs/gitutil"
	"github.com/hanwen/gritfs/gritfs"
	"github.com/hanwen/gritfs/repo"
)

func newPathFilter(filter []string) func(s string) bool {
	return func(path string) bool {
		for _, f := range filter {
			if path == f || strings.HasPrefix(path, f+"/") {
				return true
			}
		}
		return false
	}
}

func usage(fs *flag.FlagSet) func() {
	return func() {
		fs.PrintDefaults()
	}
}

// findRoot finds the repo root (as opposed to the superproject root
// which is the mountpoint.)
func findRoot(dir string, root *gritfs.RepoNode) (*fs.Inode, string, error) {
	rootInode := root.EmbeddedInode()
	rootIdx := 0

	if dir == "" {
		return rootInode, "", nil
	}

	components := strings.Split(dir, "/")
	current := rootInode
	for i, c := range components {
		ch := current.GetChild(c)
		if ch == nil {
			return nil, "", fmt.Errorf("cannot find child %q at %v", c, current.Path(root.EmbeddedInode()))
		}

		if _, ok := ch.Operations().(*gritfs.RepoNode); ok {
			rootInode = ch
			rootIdx = i
		}

		current = ch
	}

	return rootInode, strings.Join(components[rootIdx+1:], "/"), nil
}

const DateTime = "2006-01-02 15:04:05"

func wsLog(gritRepo *repo.Repository, ioc *IOClient, wsname string, maxEntry int) error {
	ref, err := gritRepo.Reference(plumbing.ReferenceName("refs/grit/"+wsname), true)
	wsID := ref.Hash()
	wsCommit, err := gritRepo.CommitObject(wsID)
	if err != nil {
		return err
	}
	for {
		checkedOut, err := wsCommit.Parent(wsCommit.NumParents() - 1)
		if err != nil {
			return err
		}
		tree, err := wsCommit.Tree()
		if err != nil {
			return err
		}
		f, err := tree.File(checkedOut.Hash.String())
		if err != nil {
			return err
		}
		status := gritfs.WorkspaceState{}
		data, err := f.Contents()
		if err != nil {
			return err
		}

		if err := json.Unmarshal([]byte(data), &status); err != nil {
			return err
		}

		lines := strings.Split(checkedOut.Message, "\n")

		ioc.Printf("%s at commit %s - %s\n", wsCommit.Committer.When.Format(DateTime), checkedOut.Hash, lines[0])
		ioc.Printf("  Reason: %s\n", wsCommit.Message)
		ioc.Printf("  Status: %#v\n", status)

		if wsCommit.NumParents() == 1 {
			break
		}

		wsCommit, err = wsCommit.Parent(0)
		if err != nil {
			return err
		}

		if maxEntry > 0 {
			maxEntry--
			if maxEntry == 0 {
				break
			}
		}
	}

	return nil

}

func WSLogCommand(args []string, dir string, ioc *IOClient, root gritfs.Node) (int, error) {
	fs := flag.NewFlagSet("wslog", flag.ContinueOnError)
	fs.Bool("help", false, "show help")
	maxEntry := fs.Int("n", 0, "maximum number of entries to show")
	fs.SetOutput(ioc)
	fs.Usage = usage(fs)

	if err := fs.Parse(args); err != nil {
		return 2, nil // Parse already prints diagnostics.
	}

	args = fs.Args()

	if len(args) != 0 {
		fs.Usage()
		return 2, nil
	}

	repoNode := root.GetRepoNode()
	repo := repoNode.Repository()
	if err := wsLog(repo, ioc, repoNode.WorkspaceName(), *maxEntry); err != nil {
		ioc.Printf("wsLog: %v", err)
		return 1, nil
	}
	return 0, nil
}

func LogCommand(args []string, dir string, ioc *IOClient, root gritfs.Node) (int, error) {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	patch := fs.Bool("p", false, "show patches")
	fs.Bool("help", false, "show help")
	commitCount := fs.Int("n", 1, "number of commits to show. 0 = unlimited")
	fs.SetOutput(ioc)
	fs.Usage = usage(fs)

	if err := fs.Parse(args); err != nil {
		return 2, nil // Parse already prints diagnostics.
	}

	args = fs.Args()
	var startHash plumbing.Hash
	var err error
	if len(args) > 0 && plumbing.IsHash(args[0]) {
		startHash = plumbing.NewHash(args[0])
		args = args[1:]
	} else {
		startHash, err = root.ID()
		if err != nil {
			ioc.Println("root.gitID: %v", err)
			return 2, nil
		}
	}

	opts := &git.LogOptions{
		From: startHash,
	}

	if len(args) > 0 {
		var filtered []string
		for _, a := range args {
			filtered = append(filtered, filepath.Clean(a))
		}
		opts.PathFilter = newPathFilter(filtered)
	}

	repo := root.GetRepoNode().Repository()
	iter, err := repo.Log(opts)
	if err != nil {
		ioc.Println("Log(%v): %v\n", opts, err)
		return 1, nil
	}

	if err := iter.ForEach(func(c *object.Commit) error {
		_, err := ioc.Println("%v", c)

		if *patch {
			parent := &object.Commit{}
			if c.NumParents() > 0 {
				parent, err = c.Parent(0)
				if err == object.ErrParentNotFound {
					err = nil
					parent = nil
				}
			} else {
				parent.TreeHash, err = gitutil.SaveTree(repo.Storer, nil)
				if err != nil {
					return err
				}
				emptyID, err := gitutil.SaveCommit(repo.Storer, parent)
				if err != nil {
					return err
				}
				parent, err = repo.CommitObject(emptyID)
				if err != nil {
					return err
				}
			}

			if err != nil {
				return err
			}

			p, err := parent.Patch(c)
			if err != nil {
				log.Println("patch", parent, c, err)
				return err
			}

			ioc.Println("")
			// should filter patch by paths as well?
			p.Encode(ioc)
		}

		if *commitCount > 0 {
			*commitCount--
			if *commitCount == 0 {
				return storer.ErrStop
			}
		}

		// propagate err if client went away
		return err
	}); err != nil {
		log.Printf("ForEach: %v", err)
	}

	return 0, nil
}

func amend(ioc *IOClient, root gritfs.Node) error {
	// Update commit
	root.GetRepoNode().ID()

	c := root.GetRepoNode().GetCommit()

	msg := `# provide a new commit message below.
# remove the Grit-Commit footer for further edits to
# create a new commit

` + c.Message
	data, err := ioc.Edit("commit-message", []byte(msg))
	if err != nil {
		return err
	}

	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if len(l) > 0 && l[0] == '#' {
			continue
		}
		lines = append(lines, l)
	}

	c.Message = strings.Join(lines, "\n")

	return root.GetRepoNode().StoreCommit(&c, time.Now(), &gritfs.WorkspaceUpdate{Message: "amend"})
}

func AmendCommand(args []string, dir string, ioc *IOClient, root gritfs.Node) (int, error) {
	if err := amend(ioc, root); err != nil {
		ioc.Println("%v", err)
		return 2, nil
	}
	return 0, nil
}

func commit(args []string, dir string, ioc *IOClient, root gritfs.Node) error {
	repoNode := root.GetRepoNode()

	wsUpdate := gritfs.WorkspaceUpdate{
		Message: "before commit command",
		NewState: gritfs.WorkspaceState{
			AutoSnapshot: true,
		},
	}
	commit, wsState, err := repoNode.Snapshot(&wsUpdate)
	if err != nil {
		return err
	}

	if !wsState.AutoSnapshot {
		ioc.Printf("No pending files; top commit is %s - %s", commit.Hash, gitutil.Subject(commit))
		return nil
	}

	flagSet := flag.NewFlagSet("commit", flag.ContinueOnError)
	msg := flagSet.String("m", "", "commit message")
	flagSet.SetOutput(ioc)
	flagSet.Usage = usage(flagSet)

	if err := flagSet.Parse(args); err != nil {
		return nil // Parse already prints diagnostics.
	}

	parent, err := commit.Parent(0)
	if err != nil {
		return err
	}
	if len(flagSet.Args()) > 0 {
		var changes []object.TreeEntry
		for _, a := range flagSet.Args() {
			p := filepath.Clean(filepath.Join(dir, a))
			c, err := walkPath(repoNode.EmbeddedInode(), p)
			if err != nil {
				return err
			}

			if blob, ok := c.Operations().(*gritfs.BlobNode); !ok {
				return fmt.Errorf("path %q is not a file (%T)", a, c)
			} else {
				id, err := blob.ID()
				if err != nil {
					return err
				}
				changes = append(changes,
					object.TreeEntry{Name: p, Mode: blob.DirMode(), Hash: id})
			}
		}

		prevTree, err := parent.Tree()
		if err != nil {
			return err
		}
		id, err := gitutil.PatchTree(repoNode.Repository().Storer, prevTree, changes)
		commit.TreeHash = id
	}

	if *msg != "" {
		commit.Message = *msg
	} else {
		stats, err := commit.Stats()
		if err != nil {
			return err
		}
		msg := `#
# You are about to commit the following files: 
#
`
		for _, st := range stats {
			msg += fmt.Sprintf("# (+%-4d, -%-4d) %s\n", st.Addition, st.Deletion, st.Name)
		}
		msg += "#\n# Provide a commit message:\n\n"

		before := strings.TrimSpace(commit.Message)
		msg += before

		data, err := ioc.Edit("commit-message", []byte(msg))
		if err != nil {
			return err
		}

		after := strings.TrimSpace(string(data))
		if before == after {
			return fmt.Errorf("must provide a message")
		}
		commit.Message = after
	}

	if err := repoNode.StoreCommit(commit, time.Now(),
		&gritfs.WorkspaceUpdate{Message: "commit"}); err != nil {
		return err
	}

	// If other files were still changed, generate a new snapshot
	wsUpdate = gritfs.WorkspaceUpdate{
		Message: "after commit command",
		NewState: gritfs.WorkspaceState{
			AutoSnapshot: true,
		},
	}
	if _, _, err := repoNode.Snapshot(&wsUpdate); err != nil {
		return err
	}

	return nil
}

func SnapshotCommand(args []string, dir string, ioc *IOClient, root gritfs.Node) (int, error) {
	wsUpdate := gritfs.WorkspaceUpdate{
		Message: "snapshot command",
		NewState: gritfs.WorkspaceState{
			AutoSnapshot: true,
		},
	}
	if _, _, err := root.GetRepoNode().Snapshot(&wsUpdate); err != nil {
		ioc.Printf("Snapshot: %v", err)
		return 1, nil
	}
	return 0, nil
}

func CommitCommand(args []string, dir string, ioc *IOClient, root gritfs.Node) (int, error) {
	if err := commit(args, dir, ioc, root); err != nil {
		ioc.Println("%v", err)
		return 2, nil
	}
	return 0, nil
}

var fileModeNames = map[filemode.FileMode]string{
	filemode.Dir:        "tree",
	filemode.Regular:    "blob",
	filemode.Executable: "blob",
	filemode.Symlink:    "symlink",
	filemode.Submodule:  "commit",
}

// `dir` is the path leading up to `root`.
func lsTree(root *fs.Inode, dir string, recursive bool, ioc *IOClient) error {
	node, ok := root.Operations().(gritfs.Node)
	if !ok {
		return fmt.Errorf("path %q is not a Node", root.Path(nil))
	}

	tree := node.GetTreeNode()
	if tree == nil {
		return fmt.Errorf("path %q is not a git tree", root.Path(nil))
	}

	entries, err := tree.TreeEntries()
	if err != nil {
		return err
	}
	for _, e := range entries {
		fn := filepath.Join(dir, e.Name)
		if recursive && e.Mode == filemode.Dir {
			lsTree(root.GetChild(e.Name), fn, recursive, ioc)
		} else {
			// bug - mode -> string prefixes 0.
			ioc.Println("%s %s %s\t%s", e.Mode.String()[1:], fileModeNames[e.Mode], e.Hash, fn)
		}
	}

	return nil
}

func walkPath(root *fs.Inode, path string) (*fs.Inode, error) {
	var components []string
	if len(path) > 0 {
		path = filepath.Clean(path)
		components = strings.Split(path, "/")
	}

	current := root
	for _, c := range components {
		ch := current.GetChild(c)
		if ch == nil {
			return nil, fmt.Errorf("cannot find child %q at %v", c, current.Path(nil))
		}

		current = ch
	}
	return current, nil
}

func LsTreeCommand(args []string, dir string, ioc *IOClient, root gritfs.Node) (int, error) {
	flagSet := flag.NewFlagSet("ls-tree", flag.ContinueOnError)
	recursive := flagSet.Bool("r", false, "recursive")
	flagSet.SetOutput(ioc)
	flagSet.Usage = usage(flagSet)

	if err := flagSet.Parse(args); err != nil {
		return 2, nil // Parse already prints diagnostics.
	}

	inode := root.(fs.InodeEmbedder).EmbeddedInode()
	current, err := walkPath(inode, dir)
	if err != nil {
		ioc.Println("%s", err)
		return 1, nil
	}
	arg := ""
	if len(flagSet.Args()) > 0 {
		arg = flagSet.Arg(0)
	}

	current, err = walkPath(current, arg)
	if err != nil {
		ioc.Println("%s", err)
		return 1, nil
	}

	if err := lsTree(current, arg, *recursive, ioc); err != nil {
		ioc.Println("lstree: %v", err)
		return 1, nil
	}

	return 0, nil
}

func find(root *fs.Inode, dir string, nameFilter, typeFilter string, ioc *IOClient) error {
	for k, v := range root.Children() {
		match := true
		if nameFilter != "" {
			ok, err := filepath.Match(nameFilter, k)
			if err == nil && !ok {
				match = false
			}
		}

		if typeFilter == "d" && !v.IsDir() {
			match = false
		} else if typeFilter == "f" && v.Mode() != fuse.S_IFREG {
			match = false
		}

		if match {
			ioc.Println("%s", filepath.Join(dir, k))
		}

		if v.IsDir() {
			if err := find(v, filepath.Join(dir, k), nameFilter, typeFilter, ioc); err != nil {
				return err
			}
		}
	}
	return nil
}

func FindCommand(args []string, dir string, ioc *IOClient, root gritfs.Node) (int, error) {
	flagSet := flag.NewFlagSet("find", flag.ContinueOnError)
	nameFilter := flagSet.String("name", "", "name glob")
	typeFilter := flagSet.String("type", "", "filter to type")
	flagSet.SetOutput(ioc)
	flagSet.Usage = usage(flagSet)
	if err := flagSet.Parse(args); err != nil {
		return 2, nil // Parse already prints diagnostics.
	}

	inode := root.(fs.InodeEmbedder).EmbeddedInode()
	current, err := walkPath(inode, dir)
	if err != nil {
		ioc.Println("%s", err)
		return 1, nil
	}
	if err := find(current, "", *nameFilter, *typeFilter, ioc); err != nil {
		ioc.Println("lstree: %v", err)
		return 1, nil
	}

	return 0, nil

}

func CheckoutCommand(args []string, dir string, ioc *IOClient, root gritfs.Node) (int, error) {
	flagSet := flag.NewFlagSet("checkout", flag.ContinueOnError)
	flagSet.SetOutput(ioc)
	flagSet.Usage = usage(flagSet)
	if err := flagSet.Parse(args); err != nil {
		return 2, nil // Parse already prints diagnostics.
	}

	if len(flagSet.Args()) != 1 {
		flagSet.Usage()
		return 2, nil
	}
	h := plumbing.NewHash(flagSet.Arg(0))

	if _, err := root.GetRepoNode().Repository().FetchCommit(h); err != nil {
		ioc.Printf("FetchCommit: %s\n", err)
		return 1, nil
	}

	if err := root.SetID(h, filemode.Dir, time.Now()); err != nil {
		ioc.Printf("%s\n", err)
		return 1, nil
	}
	return 0, nil

}

var dispatch = map[string]func([]string, string, *IOClient, gritfs.Node) (int, error){
	"log":      LogCommand,
	"wslog":    WSLogCommand,
	"amend":    AmendCommand,
	"ls-tree":  LsTreeCommand,
	"commit":   CommitCommand,
	"find":     FindCommand,
	"checkout": CheckoutCommand,
	"snapshot": SnapshotCommand,
}

func Usage(ioc *IOClient) (int, error) {
	ioc.Printf("Usage: grit <subcommand>\n\nAvailable subcommands:\n\n")
	var ks []string
	for k := range dispatch {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		ioc.Printf("  %s\n", k)
	}

	ioc.Println("")
	return 0, nil
}

func RunCommand(args []string, dir string, iocAPI IOClientAPI, root *gritfs.RepoNode) (int, error) {
	ioc := &IOClient{iocAPI}
	if len(args) == 0 {
		return Usage(ioc)
	}

	subcommand := args[0]
	args = args[1:]

	fn := dispatch[subcommand]
	if fn == nil {
		log.Println(ioc)
		ioc.Printf("unknown subcommand %q", subcommand)
		return 2, nil
	}

	rootInode, dir, err := findRoot(dir, root)
	if err != nil {
		ioc.Println("%s", err)
		return 2, nil
	}
	rootGitNode := rootInode.Operations().(gritfs.Node)

	return fn(args, dir, ioc, rootGitNode)
}
