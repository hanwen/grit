// Copyright 2023 Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fs

import (
	"flag"
	"log"
)

func Usage(ioc *IOClient) (int, error) {
	ioc.Printf("Usage blah blah\n")
	return 0, nil
}

func LogCommand(args []string, env []string, ioc *IOClient) (int, error) {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	patch := fs.Bool("p", false, "show patches")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}

	ioc.Printf("todo")
	_ = *patch
	return 0, nil
}

var dispatch = map[string]func([]string, []string, *IOClient) (int, error){
	"log": LogCommand,
}

func RunCommand(args []string, env []string, ioc *IOClient, root *glitRoot) (int, error) {
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

	return fn(args, env, ioc)
}
