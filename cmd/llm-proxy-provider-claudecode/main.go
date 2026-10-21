// Command llm-proxy-provider-claudecode is a command-line tool.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/thelonelyghost/llm-proxy-provider-claudecode/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.New(os.Stdout, os.Stderr).Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
