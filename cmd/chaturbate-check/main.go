package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/bcmk/siren/lib"
)

var verbose = flag.Bool("v", false, "verbose output")
var timeout = flag.Int("t", 10, "timeout in seconds")
var address = flag.String("a", "", "source IP address")
var cookies = flag.Bool("c", false, "use cookies")

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [options] <model ID>\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		return
	}
	modelID := flag.Arg(0)
	modelID = lib.ChaturbateCanonicalModelID(modelID)
	if !lib.ModelIDRegexp.MatchString(modelID) {
		fmt.Println("invalid model ID")
		return
	}
	client := lib.HTTPClientWithTimeoutAndAddress(*timeout, *address, *cookies)
	checker := lib.ChaturbateChecker{}
	checker.Init([]string{""}, []*lib.Client{client}, nil, *verbose, nil)
	fmt.Println(checker.CheckSingle(modelID))
}
