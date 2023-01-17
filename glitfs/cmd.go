// Copyright 2023 Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package glitfs

import (
	"fmt"
	iofs "io/fs"
	"io/ioutil"
	"log"
	"net"
	"net/rpc"
	"os"
	"os/exec"
	"path/filepath"
)

type CommandServer struct {
	root   *glitRoot
	Socket string
}

func (s *CommandServer) Exec(req *CommandRequest, rep *CommandReply) error {
	log.Printf("executing %#v", req)
	ioc, err := NewIOClient(req.RPCSocket)
	if err != nil {
		return err
	}
	exit, err := RunCommand(req.Args, req.Dir, ioc, s.root)
	rep.ExitCode = exit
	return err
}

type CommandRequest struct {
	Args []string

	// relative to top of FS
	Dir       string
	RPCSocket string
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
	Data []byte
}

type EditReply struct {
	Data []byte
}

type IOClient struct {
	client *rpc.Client
}

func (ioc *IOClient) Printf(str string, args ...any) (int, error) {
	return fmt.Fprintf(ioc, str, args...)
}

func (ioc *IOClient) Println(str string, args ...any) (int, error) {
	return fmt.Fprintf(ioc, str+"\n", args...)
}

func NewIOClient(sock string) (*IOClient, error) {
	client, err := rpc.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("dial(%q): %v", sock, err)
	}

	ioClient := &IOClient{
		client: client,
	}
	return ioClient, nil
}

func (ioc *IOClient) Edit(data []byte) ([]byte, error) {
	req := EditRequest{data}
	rep := EditReply{}

	err := ioc.client.Call("IOServer.Edit", &req, &rep)
	return rep.Data, err
}

func (ioc *IOClient) Write(data []byte) (n int, err error) {
	req := WriteRequest{
		Data: data,
	}

	rep := WriteReply{}
	err = ioc.client.Call("IOServer.Write", &req, &rep)
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
	f, err := ioutil.TempFile("", "")
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

func FindGlitSocket(startDir string) (socket string, topdir string, err error) {
	for dir := startDir; dir != "/"; dir = filepath.Dir(dir) {
		p := filepath.Join(dir, ".glit")
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

	return "", "", fmt.Errorf("glit socket not found")
}

func ClientRun(socket string, args []string, dir string) (int, error) {
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
	}
	rep := CommandReply{}

	err = client.Call("CommandServer.Exec", &req, &rep)
	return rep.ExitCode, err
}
