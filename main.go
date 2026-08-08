package main

import (
	"fmt"
	"os"
)

func main() {
	if err := runDetect(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
		os.Exit(1)
	}
}