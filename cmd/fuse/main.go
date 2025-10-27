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

package main

import (
	"flag"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/grit/gritfs"
	"github.com/hanwen/grit/repo"
	"github.com/hanwen/grit/server"
)

func main() {
	repoPath := flag.String("repo", "", "")
	originURL := flag.String("url", "", "URL for the repo")
	casDir := flag.String("cas", filepath.Join(os.Getenv("HOME"), ".cache", "gritfs2"), "")
	debug := flag.Bool("debug", false, "FUSE debug")
	gitDebug := flag.Bool("git_debug", false, "git debug")
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

	gitRepo, err := git.PlainOpen(*repoPath)
	if err != nil {
		log.Fatalf("PlainOpen(%q): %v", *repoPath, err)
	}

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

	gritRepo, err := repo.NewRepo(gitRepo, *repoPath, repoURL)
	if err != nil {
		log.Fatalf("NewRepo(%q, %s): %v", *repoPath, repoURL, err)
	}
	if *gitDebug {
		gritRepo.SetDebug(*gitDebug)
	}
	root, err := gritfs.NewWorkspacesNode(cas, gritRepo)
	if err != nil {
		log.Fatalf("NewRoot: %v", err)
	}

	// Mount the file system
	log.Println("Mounting...")
	srv, err := fusefs.Mount(mntDir, root, &fusefs.Options{
		MountOptions: fuse.MountOptions{Debug: *debug},
		UID:          uint32(os.Getuid()),
		GID:          uint32(os.Getgid()),
	})
	if err != nil {
		log.Fatal("Mount", err)
	}
	defer srv.Unmount()
	if err := server.StartCommandServer(root.EmbeddedInode(), srv); err != nil {
		log.Fatalf("StartCommandServer: %v", err)
	}
	log.Printf("Mounted git on %s\n", mntDir)
	// Serve the file system, until unmounted by calling fusermount -u
	srv.Wait()
}
