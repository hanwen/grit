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

package server

import (
	"bytes"
	"context"
	"fmt"
	iofs "io/fs"
	"io/ioutil"
	"log"
	"net"
	"net/rpc"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/gritfs/gritfs"
	"github.com/hanwen/gritfs/repo"
)

// CommandServer serves RPC calls
type CommandServer struct {
	root *Root
}

// Root is a RepoNode that makes .grit socket available
type Root struct {
	*gritfs.RepoNode
	Socket string
}

func (r *Root) OnAdd(ctx context.Context) {
	r.RepoNode.OnAdd(ctx)
	ch := r.NewPersistentInode(ctx, &fs.MemSymlink{
		Data: []byte(r.Socket),
	}, fs.StableAttr{Mode: fuse.S_IFLNK})
	r.AddChild(".grit", ch, true)
}

func NewCommandServer(cas *gritfs.CAS, repo *repo.Repository, workspaceName string) (*Root, error) {
	r, err := gritfs.NewRoot(cas, repo, workspaceName)
	if err != nil {
		return nil, err
	}

	root := &Root{
		RepoNode: r,
	}
	commandServer := &CommandServer{
		root: root,
	}
	l, s, err := newSocket()
	if err != nil {
		return nil, err
	}
	root.Socket = s
	srv := rpc.NewServer()
	if err := srv.Register(commandServer); err != nil {
		return nil, err
	}
	go srv.Accept(l)

	return root, nil
}

func (s *CommandServer) Exec(req *CommandRequest, rep *CommandReply) error {
	start := time.Now()

	var startUsage, endUsage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &startUsage); err != nil {
		return fmt.Errorf("Getrusage: %v", err)
	}
	log.Printf("executing %#v", req)
	if req.Profile != "" {
		f, err := os.Create(req.Profile)
		if err != nil {
			return err
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	iocAPI, err := NewIOClientAPI(req.RPCSocket)
	if err != nil {
		return err
	}

	call := Call{
		IOClientAPI: iocAPI,
		Args:        req.Args,
		Dir:         req.Dir,
		Root:        s.root.RepoNode,
	}
	if err := RunCommand(&call); err != nil {
		rep.ExitCode = 1
		call.Write([]byte(err.Error()))
		err = nil
	}

	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &endUsage); err != nil {
		return fmt.Errorf("Getrusage: %v", err)
	}

	ns := endUsage.Utime.Nano() - startUsage.Utime.Nano()
	wall := time.Now().Sub(start)
	user := time.Duration(ns)

	log.Printf("finished in %v; utilization %d%%. ", time.Now().Sub(start), int64(100*user/wall))
	return err
}

type CommandRequest struct {
	Args []string

	// relative to top of FS
	Dir       string
	RPCSocket string

	// Write CPU profile data.
	Profile string
}

type CommandReply struct {
	ExitCode int
}

type WriteRequest struct {
	Data []byte
}

type WriteReply struct {
	N int
}

type EditRequest struct {
	Name string
	Data []byte
}

type EditReply struct {
	Data []byte
}

// Interface for communication back to client invocation
type IOClientAPI interface {
	Edit(name string, data []byte) ([]byte, error)
	Write(data []byte) (n int, err error)
}

// RPC client for talking back to command-line process
type Call struct {
	IOClientAPI

	Args []string
	Dir  string
	Root gritfs.Node
}

func (call *Call) Printf(str string, args ...any) (int, error) {
	return fmt.Fprintf(call.IOClientAPI, str, args...)
}

func (call *Call) Println(str string, args ...any) (int, error) {
	return fmt.Fprintf(call.IOClientAPI, str+"\n", args...)
}

func (call *Call) Edit(name string, data []byte) ([]byte, error) {
	ret, err := call.IOClientAPI.Edit(name, data)

	trimmed := make([]byte, 0, len(ret))
	for _, l := range bytes.Split(ret, []byte("\n")) {
		if len(l) > 0 && l[0] == '#' {
			continue
		}

		trimmed = append(trimmed, l...)
		trimmed = append(trimmed, '\n')
	}

	return trimmed, err
}

func NewIOClientAPI(sock string) (IOClientAPI, error) {
	client, err := rpc.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("dial(%q): %v", sock, err)
	}

	ioClient := &rpcIOClient{
		client: client,
	}
	return ioClient, nil
}

type rpcIOClient struct {
	client *rpc.Client
}

func (call *rpcIOClient) Edit(name string, data []byte) ([]byte, error) {
	req := EditRequest{Data: data, Name: name}
	rep := EditReply{}

	err := call.client.Call("IOServer.Edit", &req, &rep)
	return rep.Data, err
}

func (call *rpcIOClient) Write(data []byte) (n int, err error) {
	req := WriteRequest{
		Data: data,
	}

	rep := WriteReply{}
	err = call.client.Call("IOServer.Write", &req, &rep)
	return rep.N, err
}

type IOServer struct {
	Socket string
}

func newSocket() (net.Listener, string, error) {
	dir, err := ioutil.TempDir("", "")
	if err != nil {
		return nil, "", err
	}
	s := filepath.Join(dir, "socket")
	l, err := net.Listen("unix", s)
	return l, s, err
}

func NewIOServer() (*IOServer, error) {
	l, sock, err := newSocket()
	if err != nil {
		return nil, err
	}
	srv := &IOServer{
		Socket: sock,
	}

	rpcSrv := rpc.NewServer()
	if err := rpcSrv.Register(srv); err != nil {
		return nil, err
	}

	go rpcSrv.Accept(l)
	return srv, nil
}

func (s *IOServer) Write(req *WriteRequest, rep *WriteReply) error {
	n, err := os.Stdout.Write(req.Data)
	rep.N = n
	return err
}

func (s *IOServer) Edit(req *EditRequest, rep *EditReply) error {
	f, err := ioutil.TempFile("", req.Name)
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())

	if err := ioutil.WriteFile(f.Name(), req.Data, 0644); err != nil {
		return err
	}

	cmd := exec.Command("/bin/sh", "-c", os.Getenv("EDITOR")+" "+f.Name())
	if err := cmd.Run(); err != nil {
		return err
	}
	rep.Data, err = ioutil.ReadFile(f.Name())
	return err
}

func FindGritSocket(startDir string) (socket string, topdir string, err error) {
	for dir := startDir; dir != "/"; dir = filepath.Dir(dir) {
		p := filepath.Join(dir, ".grit")
		fi, err := os.Stat(p)
		if fi == nil || fi.Mode()&iofs.ModeType != iofs.ModeSocket {
			continue
		}
		val, err := os.Readlink(p)
		if err != nil {
			return "", "", err
		}
		if filepath.IsAbs(val) {
			return val, dir, nil
		}
	}

	return "", "", fmt.Errorf("grit socket not found")
}

// Runs a command on the server for use in the command-line program
func ClientRun(socket string, args []string, dir, profile string) (int, error) {
	srv, err := NewIOServer()
	if err != nil {
		return 0, err
	}

	client, err := rpc.Dial("unix", socket)
	if err != nil {
		return 0, err
	}

	req := CommandRequest{
		Args:      args,
		Dir:       dir,
		RPCSocket: srv.Socket,
		Profile:   profile,
	}
	rep := CommandReply{}

	err = client.Call("CommandServer.Exec", &req, &rep)
	return rep.ExitCode, err
}
