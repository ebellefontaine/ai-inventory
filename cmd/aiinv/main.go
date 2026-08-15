package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "aiinv: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Println("aiinv")
	return nil
}
