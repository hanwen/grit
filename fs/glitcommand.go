// Copyright 2023 Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fs

import (
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
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

func findRoot(dir string, root *glitRoot) (*fs.Inode, string, error) {
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
			return nil, "", fmt.Errorf("cannot find child %q at %v", c, rootInode.Path(root.EmbeddedInode()))
		}

		if _, ok := ch.Operations().(*gitFSRoot); ok {
			rootInode = ch
			rootIdx = i
		}

		current = ch
	}

	return rootInode, strings.Join(components[rootIdx:], "/"), nil
}

func LogCommand(args []string, dir string, ioc *IOClient, root *glitRoot) (int, error) {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	patch := fs.Bool("p", false, "show patches")
	fs.Bool("help", false, "show help")
	commitCount := fs.Int("n", 1, "number of commits to show. 0 = unlimited")
	fs.SetOutput(ioc)
	fs.Usage = usage(fs)

	if err := fs.Parse(args); err != nil {
		return 2, nil // Parse already prints diagnostics.
	}

	rootInode, _, err := findRoot(dir, root)
	if err != nil {
		ioc.Println("%s", err)
		return 2, nil
	}
	rootGitNode := rootInode.Operations().(gitNode)

	args = fs.Args()
	var startHash plumbing.Hash
	if len(args) > 0 && plumbing.IsHash(args[0]) {
		startHash = plumbing.NewHash(args[0])
		args = args[1:]
	} else {
		startHash, err = rootGitNode.gitID()
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

	iter, err := rootGitNode.gitRepo().Log(opts)
	if err != nil {
		ioc.Println("Log: %v\n", err)
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

			p, err := c.Patch(parent)
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

var dispatch = map[string]func([]string, string, *IOClient, *glitRoot) (int, error){
	"log": LogCommand,
}

func RunCommand(args []string, dir string, ioc *IOClient, root *glitRoot) (int, error) {
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

	return fn(args, dir, ioc, root)
}
