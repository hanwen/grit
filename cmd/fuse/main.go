package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/gritfs/gritfs"
	"github.com/hanwen/gritfs/server"
)

func main() {
	repoPath := flag.String("repo", "", "")
	casDir := flag.String("cas", filepath.Join(os.Getenv("HOME"), ".cache", "gritfs2"), "")

	id := flag.String("id", "", "")
	flag.Parse()
	if len(flag.Args()) != 1 {
		log.Fatal("usage")
	}
	mntDir := flag.Arg(0)

	if fi, err := os.Stat(filepath.Join(*repoPath, ".git")); err == nil && fi.IsDir() {
		*repoPath = filepath.Join(*repoPath, ".git")
	}

	repo, err := git.PlainOpen(*repoPath)
	if err != nil {
		log.Fatal("PlainOpen", err)
	}

	h := plumbing.NewHash(*id) // err handling?

	cas, err := gritfs.NewCAS(*casDir)
	if err != nil {
		log.Fatal("NewCAS", err)
	}
	root, err := server.NewCommandServer(cas, repo, *repoPath, h)
	if err != nil {
		log.Fatal("NewRoot", err)
	}

	// Mount the file system
	server, err := fusefs.Mount(mntDir, root, &fusefs.Options{
		MountOptions: fuse.MountOptions{Debug: true},
	})
	if err != nil {
		log.Fatal("Mount", err)
	}

	fmt.Printf("Mounted git on %s\n", mntDir)
	// Serve the file system, until unmounted by calling fusermount -u
	server.Wait()
}
