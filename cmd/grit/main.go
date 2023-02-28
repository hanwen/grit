// Copyright 2023 Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"github.com/hanwen/gritfs/server"
)

func main() {
	dir := flag.String("dir", "", "grit checkout; defaults to $CWD")
	profile := flag.String("profile", "", "dump profile date to this file")
	flag.Parse()

	if *dir == "" {
		var err error
		*dir, err = os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
	}

	*dir = filepath.Clean(*dir)
	sock, topdir, err := server.FindGritSocket(*dir)
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
	code, err := server.ClientRun(sock, flag.Args(), relCWD, *profile)
	if err != nil {
		log.Fatal(err)
	}
	os.Exit(code)
}
