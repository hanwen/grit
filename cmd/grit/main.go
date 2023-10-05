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
