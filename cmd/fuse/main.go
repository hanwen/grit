package main

import (
	"flag"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/gritfs/gritfs"
	"github.com/hanwen/gritfs/server"
)

func main() {
	repoPath := flag.String("repo", "", "")
	originURL := flag.String("url", "", "URL for the repo")
	casDir := flag.String("cas", filepath.Join(os.Getenv("HOME"), ".cache", "gritfs2"), "")
	debug := flag.Bool("debug", false, "FUSE debug")
	id := flag.String("id", "", "")
	flag.Parse()
	if len(flag.Args()) != 1 {
		log.Fatal("usage")
	}
	mntDir := flag.Arg(0)

	if *originURL == "" {
		log.Fatal("must specify -url")
	}

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

	// submodule URLs are relative; resolution goes wrong if it
	// doesn't end in '/'.
	if !strings.HasSuffix(*originURL, "/") {
		*originURL += "/"
	}
	repoURL, err := url.Parse(*originURL)
	if err != nil {
		log.Fatalf("Parse(%q): %v", *originURL, err)
	}

	root, err := server.NewCommandServer(cas, repo, *repoPath, h, repoURL)
	if err != nil {
		log.Fatal("NewRoot", err)
	}

	// Mount the file system
	log.Println("Mounting...")
	server, err := fusefs.Mount(mntDir, root, &fusefs.Options{
		MountOptions: fuse.MountOptions{Debug: *debug},
		UID:          uint32(os.Getuid()),
		GID:          uint32(os.Getgid()),
	})
	if err != nil {
		log.Fatal("Mount", err)
	}

	log.Printf("Mounted git on %s\n", mntDir)
	// Serve the file system, until unmounted by calling fusermount -u
	server.Wait()
}
