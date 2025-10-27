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
	"github.com/hanwen/grit/gitutil"
	"github.com/hanwen/grit/gritfs"
	"github.com/hanwen/grit/repo"
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
func findRoot(dir string, current *fs.Inode) (*gritfs.RepoNode, string, error) {
	repoIdx := 0
	start := current
	if dir == "" {
		if rn, ok := current.Operations().(*gritfs.RepoNode); ok {
			return rn, "", nil
		}
		return nil, "", nil
	}

	var result *gritfs.RepoNode
	components := strings.Split(dir, "/")
	for i, c := range components {
		ch := current.GetChild(c)
		if ch == nil {
			return nil, "", fmt.Errorf("cannot find child %q at %v", c, current.Path(start))
		}

		if rn, ok := ch.Operations().(*gritfs.RepoNode); ok {
			result = rn
			repoIdx = i
		}

		current = ch
	}

	return result, strings.Join(components[repoIdx+1:], "/"), nil
}

const DateTime = "2006-01-02 15:04:05"

func wsLog(gritRepo *repo.Repository, call *Call, wsname string, maxEntry int) error {
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

		call.Printf("%s at commit %s - %s\n", wsCommit.Committer.When.Format(DateTime), checkedOut.Hash, lines[0])
		call.Printf("  Reason: %s\n", wsCommit.Message)
		call.Printf("  Metadata ID: %x\n", wsCommit.Hash[:8])
		call.Printf("  Status: %#v\n", status)

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

func WSLogCommand(call *Call) error {
	fs := flag.NewFlagSet("wslog", flag.ContinueOnError)
	fs.Bool("help", false, "show help")
	maxEntry := fs.Int("n", 0, "maximum number of entries to show")
	fs.SetOutput(call)
	fs.Usage = usage(fs)

	if err := fs.Parse(call.Args); err != nil {
		return err
	}

	args := fs.Args()

	if len(args) != 0 {
		fs.Usage()
		return fmt.Errorf("need argument")
	}

	repoNode := call.RepoNode
	repo := repoNode.Repository()
	if err := wsLog(repo, call, repoNode.WorkspaceName(), *maxEntry); err != nil {
		return err
	}
	return nil
}

func LogCommand(call *Call) error {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	patch := fs.Bool("p", false, "show patches")
	fs.Bool("help", false, "show help")
	commitCount := fs.Int("n", 1, "number of commits to show. 0 = unlimited")
	fs.SetOutput(call)
	fs.Usage = usage(fs)

	if err := fs.Parse(call.Args); err != nil {
		return fmt.Errorf("flag parse")
	}

	args := fs.Args()
	var startHash plumbing.Hash
	var err error
	if len(args) > 0 && plumbing.IsHash(args[0]) {
		startHash = plumbing.NewHash(args[0])
		args = args[1:]
	} else {
		_, err := call.RepoNode.Snapshot(&gritfs.WorkspaceUpdate{
			Message: "log call",
			TS:      time.Now(),
			NewState: gritfs.WorkspaceState{
				AutoSnapshot: true,
			},
		})

		if err != nil {
			return err
		}

		startHash = call.RepoNode.ID()
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

	repo := call.RepoNode.Repository()
	iter, err := repo.Log(opts)
	if err != nil {
		return err
	}

	if err := iter.ForEach(func(c *object.Commit) error {
		_, err := call.Println("%v", c)

		if *patch {
			parent := &object.Commit{}
			if c.NumParents() > 0 {
				parent, err = c.Parent(0)
				if err == object.ErrParentNotFound {
					err = nil
					parent = nil
				}
			} else {
				parent.TreeHash, err = repo.SaveTree(nil)

				if err != nil {
					return err
				}
				emptyID, err := repo.SaveCommit(parent)
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

			chs, err := call.RepoNode.Repository().DiffRecursiveByCommit(parent, c)
			if err != nil {
				log.Println("patch", parent, c, err)
				return err
			}
			call.Println("")
			p, err := chs.Patch()
			if err != nil {
				log.Println("patch", parent, c, err)
				return err
			}
			// should filter patch by paths as well?
			p.Encode(call)
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

	return nil
}

func AmendCommand(call *Call) error {
	// Update commit
	wsUpdate := gritfs.WorkspaceUpdate{
		Message: "before amend command",
		NewState: gritfs.WorkspaceState{
			AutoSnapshot: true,
		},
		TS: time.Now(),
	}
	if _, err := call.RepoNode.Snapshot(&wsUpdate); err != nil {
		return err
	}

	c := call.RepoNode.GetCommit()

	msg := `# provide a new commit message below.
# remove the Grit-Commit footer for further edits to
# create a new commit

` + c.Message
	data, err := call.Edit("commit-message", []byte(msg))
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

	return call.RepoNode.StoreCommit(&c, &gritfs.WorkspaceUpdate{
		TS:      time.Now(),
		Message: "amend",
	})
}

func CommitCommand(call *Call) error {
	repoNode := call.RepoNode

	wsUpdate := gritfs.WorkspaceUpdate{
		Message: "before commit command",
		NewState: gritfs.WorkspaceState{
			AutoSnapshot: true,
		},
		TS: time.Now(),
	}
	snapResult, err := repoNode.Snapshot(&wsUpdate)
	if err != nil {
		return err
	}
	if !snapResult.State.AutoSnapshot {
		call.Printf("No pending files; top commit is %s - %s", snapResult.CheckedOut.Hash, gitutil.Subject(snapResult.CheckedOut))
		return nil
	}

	flagSet := flag.NewFlagSet("commit", flag.ContinueOnError)
	msg := flagSet.String("m", "", "commit message")
	flagSet.SetOutput(call)
	flagSet.Usage = usage(flagSet)

	if err := flagSet.Parse(call.Args); err != nil {
		return nil // Parse already prints diagnostics.
	}

	commit := snapResult.CheckedOut
	parent, err := commit.Parent(0)
	if err != nil {
		return err
	}
	if len(flagSet.Args()) > 0 {
		var changes []object.TreeEntry
		for _, a := range flagSet.Args() {
			p := filepath.Clean(filepath.Join(call.Dir, a))
			c, err := walkPath(repoNode.EmbeddedInode(), p)
			if err != nil {
				return err
			}

			if blob, ok := c.Operations().(*gritfs.BlobNode); !ok {
				return fmt.Errorf("path %q is not a file (%T)", a, c)
			} else {
				id := blob.ID()
				changes = append(changes,
					object.TreeEntry{Name: p, Mode: blob.DirMode(), Hash: id})
			}
		}

		prevTree, err := parent.Tree()
		if err != nil {
			return err
		}
		id, err := repoNode.Repository().PatchTree(prevTree, changes)
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

		data, err := call.Edit("commit-message", []byte(msg))
		if err != nil {
			return err
		}

		after := strings.TrimSpace(string(data))
		if before == after {
			return fmt.Errorf("must provide a message")
		}
		commit.Message = after
	}

	if err := repoNode.StoreCommit(commit,
		&gritfs.WorkspaceUpdate{
			TS:      time.Now(),
			Message: "commit",
		}); err != nil {
		return err
	}

	// If other files were still changed, generate a new snapshot
	wsUpdate = gritfs.WorkspaceUpdate{
		TS:      time.Now(),
		Message: "after commit command",
		NewState: gritfs.WorkspaceState{
			AutoSnapshot: true,
		},
	}
	if _, err := repoNode.Snapshot(&wsUpdate); err != nil {
		return err
	}

	return nil
}

func SnapshotCommand(call *Call) error {
	wsUpdate := gritfs.WorkspaceUpdate{
		Message: "snapshot command",
		NewState: gritfs.WorkspaceState{
			AutoSnapshot: true,
		},
		TS: time.Now(),
	}
	res, err := call.RepoNode.Snapshot(&wsUpdate)
	if err != nil {
		return err
	}
	call.Printf("Recomputed %d hashes\n", res.Recomputed)
	return nil
}

var fileModeNames = map[filemode.FileMode]string{
	filemode.Dir:        "tree",
	filemode.Regular:    "blob",
	filemode.Executable: "blob",
	filemode.Symlink:    "symlink",
	filemode.Submodule:  "commit",
}

// `dir` is the path leading up to `root`.
func lsTree(root *fs.Inode, dir string, recursive bool, call *Call) error {
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
			lsTree(root.GetChild(e.Name), fn, recursive, call)
		} else {
			// bug - mode -> string prefixes 0.
			call.Println("%s %s %s\t%s", e.Mode.String()[1:], fileModeNames[e.Mode], e.Hash, fn)
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

func LsTreeCommand(call *Call) error {
	flagSet := flag.NewFlagSet("ls-tree", flag.ContinueOnError)
	recursive := flagSet.Bool("r", false, "recursive")
	flagSet.SetOutput(call)
	flagSet.Usage = usage(flagSet)

	if err := flagSet.Parse(call.Args); err != nil {
		return fmt.Errorf("flag parse")
	}

	inode := call.RepoNode.EmbeddedInode()
	current, err := walkPath(inode, call.Dir)
	if err != nil {
		return err
	}
	arg := ""
	if len(flagSet.Args()) > 0 {
		arg = flagSet.Arg(0)
	}

	current, err = walkPath(current, arg)
	if err != nil {
		return err
	}

	if err := lsTree(current, arg, *recursive, call); err != nil {
		return err
	}

	return nil
}

func find(root *fs.Inode, dir string, nameFilter, typeFilter string, call *Call) error {
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
			call.Println("%s", filepath.Join(dir, k))
		}

		if v.IsDir() {
			if err := find(v, filepath.Join(dir, k), nameFilter, typeFilter, call); err != nil {
				return err
			}
		}
	}
	return nil
}

func FindCommand(call *Call) error {
	flagSet := flag.NewFlagSet("find", flag.ContinueOnError)
	nameFilter := flagSet.String("name", "", "name glob")
	typeFilter := flagSet.String("type", "", "filter to type")
	flagSet.SetOutput(call)
	flagSet.Usage = usage(flagSet)
	if err := flagSet.Parse(call.Args); err != nil {
		return nil // Parse already prints diagnostics.
	}

	inode := call.RepoNode.EmbeddedInode()
	current, err := walkPath(inode, call.Dir)
	if err != nil {
		return err
	}
	if err := find(current, "", *nameFilter, *typeFilter, call); err != nil {
		return err
	}

	return nil

}

func CheckoutCommand(call *Call) error {
	flagSet := flag.NewFlagSet("checkout", flag.ContinueOnError)
	flagSet.SetOutput(call)
	flagSet.Usage = usage(flagSet)
	if err := flagSet.Parse(call.Args); err != nil {
		return fmt.Errorf("flag parse")
	}

	if len(flagSet.Args()) != 1 {
		flagSet.Usage()
		return fmt.Errorf("need 1 arg")
	}
	h := plumbing.NewHash(flagSet.Arg(0))
	if h == plumbing.ZeroHash {
		return fmt.Errorf("could not parse as SHA1: %q", flagSet.Arg(0))
	}
	if _, err := call.RepoNode.Repository().FetchCommit(h); err != nil {
		return err
	}

	if err := call.RepoNode.SetID(h, time.Now()); err != nil {
		return err
	}
	return nil

}

func visit(n *fs.Inode) {
	for _, v := range n.Children() {
		visit(v)
	}
}

// For benchmarking.
func VisitCommand(call *Call) error {
	flagSet := flag.NewFlagSet("visit", flag.ContinueOnError)
	flagSet.SetOutput(call)
	flagSet.Usage = usage(flagSet)
	if err := flagSet.Parse(call.Args); err != nil {
		return err
	}

	node := call.RepoNode.EmbeddedInode()
	visit(node)
	return nil

}

var dispatch = map[string]func(*Call) error{
	"log":      LogCommand,
	"wslog":    WSLogCommand,
	"amend":    AmendCommand,
	"ls-tree":  LsTreeCommand,
	"commit":   CommitCommand,
	"find":     FindCommand,
	"checkout": CheckoutCommand,
	"snapshot": SnapshotCommand,
	"visit":    VisitCommand,
}

func Usage(call *Call) error {
	call.Printf("Usage: grit <subcommand>\n\nAvailable subcommands:\n\n")
	var ks []string
	for k := range dispatch {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		call.Printf("  %s\n", k)
	}

	call.Println("")
	return nil
}

func WSCommand(call *Call) error {
	if len(call.Args) == 0 || call.Args[0] != "workspace" {
		return fmt.Errorf("must use 'workspace' outside workspace")
	}

	call.Args = call.Args[1:]

	if len(call.Args) == 0 {
		return fmt.Errorf("Subcommand missing. Use one of list, create, delete.")
	}
	root := call.Root.Operations().(*gritfs.WorkspacesNode)
	switch call.Args[0] {
	case "list":
		names, err := gritfs.WorkspaceNames(root.Repository())
		if err != nil {
			return err
		}

		for _, nm := range names {
			call.Println(nm)
		}
	case "create":
		if len(call.Args) == 0 {
			return fmt.Errorf("usage 'workspace create WORKSPACE-NAME [COMMIT]'")
		}
		wsName := call.Args[1]
		if err := root.AddWorkspace(wsName); err != nil {
			return err
		}
		if len(call.Args) > 2 {
			c2 := &Call{
				IOClientAPI: call.IOClientAPI,
				Dir:         wsName,
				Args:        []string{call.Args[2]},
				RepoNode:    call.Root.GetChild(wsName).Operations().(*gritfs.RepoNode),
				Root:        call.Root,
			}
			return CheckoutCommand(c2)
		}
	case "delete":
		if err := root.DelWorkspace(call.Args[1]); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown subcommand %q.", call.Args[0])
	}
	return nil
}
