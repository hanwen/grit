// Copyright 2023 Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"github.com/hanwen/gitfs/server"
)

func main() {
	dir := flag.String("dir", "", "glit checkout; defaults to $CWD")
	flag.Parse()

	if *dir == "" {
		var err error
		*dir, err = os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
	}

	*dir = filepath.Clean(*dir)
	sock, topdir, err := server.FindGlitSocket(*dir)
	if err != nil {
		log.Fatal(err)
	}

	relCWD, err := filepath.Rel(topdir, *dir)
	if err != nil {
		log.Fatal(err)
	}

	if relCWD == "." {
		relCWD = ""
	}
	code, err := server.ClientRun(sock, flag.Args(), relCWD)
	if err != nil {
		log.Fatal(err)
	}
	os.Exit(code)
}
