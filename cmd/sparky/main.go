package main

import (
	"os"
	"sparky/internal/sparky"
)

func main() { os.Exit(sparky.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
