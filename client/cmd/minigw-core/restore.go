package main

import (
	"flag"
	"fmt"
	"io"

	"minigw/core"
)

// restoreMain is `minigw-core restore -store <folder> [-yes] <file|name>`: core.RestoreCLI, the code of minigw serve's
// `gw core restore`. It has its own flags and never reaches the helper mode (no socket, no secret). Returns the exit code.
func restoreMain(args []string, in io.Reader, out, errw io.Writer) int {
	fs := flag.NewFlagSet("minigw-core restore", flag.ContinueOnError)
	fs.SetOutput(errw)
	store := fs.String("store", "", "folder of the core store")
	yes := fs.Bool("yes", false, "do not ask")
	if fs.Parse(args) != nil || *store == "" || fs.NArg() != 1 {
		fmt.Fprintln(errw, "usage: minigw-core restore -store <folder> [-yes] <file|name>")
		return exitUsage
	}
	return core.RestoreCLI(*store, fs.Arg(0), *yes, in, out, errw, "quit the Mini Gateway app first")
}
