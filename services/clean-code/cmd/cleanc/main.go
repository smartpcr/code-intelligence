// -----------------------------------------------------------------------
// <copyright file="main.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("cleanc v0.0.1-dev")
		return
	}
	fmt.Fprintln(os.Stderr, "cleanc: not yet implemented")
	os.Exit(1)
}