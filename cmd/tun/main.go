// Command tun is a remote execution service for coding agents. See `tun help`.
package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/altdansalt/tun/internal/client"
	"github.com/altdansalt/tun/internal/server"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "token" {
		os.Exit(server.TokenMain(args[1:], os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "serve" {
		os.Exit(server.Main(args[1:], os.Stderr))
	}
	if len(args) > 0 && args[0] == "runner" {
		os.Exit(server.RunnerMain(args[1:], os.Stderr))
	}
	cwd, err := os.Getwd()
	if err != nil {
		os.Stderr.WriteString("tun: cannot determine working directory: " + err.Error() + "\n")
		os.Exit(125)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	os.Exit(client.Main(args, client.Env{
		Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, Cwd: cwd, Signals: sigs,
	}))
}
