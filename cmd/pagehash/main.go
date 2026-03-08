package main

import (
	"fmt"
	"os"

	"github.com/dataverse/hub/vhost"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: pagehash <ref>\n")
		os.Exit(1)
	}
	fmt.Println(vhost.PageHash(os.Args[1]))
}
