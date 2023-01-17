// Copyright 2023 Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/hanwen/glitfs/glitfs"
	"github.com/hanwen/go-fuse/v2/fs"
)

func Usage(ioc *IOClient) (int, error) {
	ioc.Printf("Usage blah blah\n")
	return 0, nil
}

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

func findRoot(dir string, root *Root) (*fs.Inode, string, error) {
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

		if _, ok := ch.Operations().(*glitfs.RepoNode); ok {
			rootInode = ch
			rootIdx = i
		}

		current = ch
	}

	return rootInode, strings.Join(components[rootIdx:], "/"), nil
}

func LogCommand(args []string, dir string, ioc *IOClient, root glitfs.Node) (int, error) {
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

	iter, err := root.GetRepoNode().Repository().Log(opts)
	if err != nil {
		ioc.Println("Log(%v): %v\n", opts, err)
		return 1, nil
	}

	stop := fmt.Errorf("stop")
	if err := iter.ForEach(func(c *object.Commit) error {
		_, err := ioc.Println("%v", c)

		if *patch {
			parent, err := c.Parent(0)
			if err == object.ErrParentNotFound {
				err = nil
				parent = nil
			}

			if err != nil {
				return err
			}

			p, err := parent.Patch(c)
			if err != nil {
				return err
			}

			ioc.Println("")
			// should filter patch by paths as well?
			p.Encode(ioc)
		}

		if *commitCount > 0 {
			*commitCount--
			if *commitCount == 0 {
				return stop
			}
		}

		// propagate err if client went away
		return err
	}); err != nil {
		if err != stop {
			log.Printf("ForEach: %v", err)
		}
	}

	return 0, nil
}

func amend(ioc *IOClient, root glitfs.Node) error {
	// Update commit
	root.GetRepoNode().ID()

	c := root.GetRepoNode().GetCommit()

	msg := `# provide a new commit message below.
# remove the Glit-Commit footer for further edits to
# create a new commit

` + c.Message
	data, err := ioc.Edit([]byte(msg))
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

	return root.GetRepoNode().StoreCommit(&c)
}

func AmendCommand(args []string, dir string, ioc *IOClient, root glitfs.Node) (int, error) {
	if err := amend(ioc, root); err != nil {
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

func lsTree(root *fs.Inode, dir string, recursive bool, ioc *IOClient) error {
	Node, ok := root.Operations().(glitfs.Node)
	if !ok {
		return fmt.Errorf("path %q is not a Node", root.Path(nil))
	}

	tree := Node.GetTreeNode()
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
			// bug - mode _> string prefixes 0.
			ioc.Println("%s %s %s\t%s", e.Mode.String()[1:], fileModeNames[e.Mode], e.Hash, fn)
		}
	}

	return nil
}

func LsTreeCommand(args []string, dir string, ioc *IOClient, root glitfs.Node) (int, error) {
	flagSet := flag.NewFlagSet("ls-tree", flag.ContinueOnError)
	recursive := flagSet.Bool("r", false, "recursive")
	flagSet.SetOutput(ioc)
	flagSet.Usage = usage(flagSet)

	if err := flagSet.Parse(args); err != nil {
		return 2, nil // Parse already prints diagnostics.
	}

	args = flagSet.Args()
	var components []string
	if len(dir) > 0 {
		components = strings.Split(dir, "/")
	}

	current := root.(fs.InodeEmbedder).EmbeddedInode()
	for _, c := range components {
		ch := current.GetChild(c)
		if ch == nil {
			return 1, fmt.Errorf("cannot find child %q at %v", c, current.Path(nil))
		}

		current = ch
	}

	if err := lsTree(current, "", *recursive, ioc); err != nil {
		ioc.Println("lstree: %v", err)
		return 1, nil
	}

	return 0, nil
}

var dispatch = map[string]func([]string, string, *IOClient, glitfs.Node) (int, error){
	"log":     LogCommand,
	"amend":   AmendCommand,
	"ls-tree": LsTreeCommand,
}

func RunCommand(args []string, dir string, ioc *IOClient, root *Root) (int, error) {
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
	rootGitNode := rootInode.Operations().(glitfs.Node)

	return fn(args, dir, ioc, rootGitNode)
}
