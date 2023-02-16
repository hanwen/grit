package protov2

import (
	"log"
	"net"
	"net/http"
	"net/http/cgi"
	"os/exec"
	"strings"
)

var gitExecDir string

func init() {
	cmd := exec.Command("git", "--exec-path")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Fatalf("git --exec-path: %v", err)
	}
	gitExecDir = strings.TrimSpace(string(out))
}

// from go-git common_test.go
func serveGit(root string, l net.Listener) {
	server := &http.Server{
		Handler: &cgi.Handler{
			Path: gitExecDir + "/git-http-backend",
			Dir:  root,
			Env: []string{
				"GIT_HTTP_EXPORT_ALL=true",
				"GIT_PROJECT_ROOT=" + root,
			},
		},
	}
	go server.Serve(l)
}
