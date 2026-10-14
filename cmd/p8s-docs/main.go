// Command p8s-docs writes the P8S OS Administrator Guide (docs/guide) as a
// standalone website: p8s-docs [-src docs/guide] OUTDIR. Without -src it uses
// the copy built into the binary. The same pages are in every P8S OS
// dashboard at https://SERVER:8006/docs/.
package main

import (
	"flag"
	"fmt"
	"os"

	"p8sos/internal/guide"
)

func main() {
	src := flag.String("src", "", "folder with the guide's NN-name.md files (default: the built-in copy)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: p8s-docs [-src docs/guide] OUTDIR")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	var (
		s   *guide.Site
		err error
	)
	if *src != "" {
		s, err = guide.Load(os.DirFS(*src))
	} else {
		s, err = guide.Builtin()
	}
	if err == nil {
		err = s.WriteSite(flag.Arg(0))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "p8s-docs:", err)
		os.Exit(1)
	}
	fmt.Printf("%d pages written to %s (open index.html)\n", len(s.Pages), flag.Arg(0))
}
