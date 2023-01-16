// Copyright 2023 Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"log"
	"os"

	"github.com/hanwen/gitfs/fs"
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

	sock, err := fs.FindGlitSocket(*dir)
	if err != nil {
		log.Fatal(err)
	}

	// TODO - pass os.Environ.
	code, err := fs.ClientRun(sock, flag.Args(), nil)
	if err != nil {
		log.Fatal(err)
	}
	os.Exit(code)
}
