// Command finbot mirrors YNAB into a local SQLite file. See cmd for the
// subcommands.
package main

import (
	"os"

	"github.com/deasa/YNAB_AutoCategorizer/cmd"
)

func main() {
	os.Exit(cmd.Main(os.Args[1:], os.Stdout, os.Stderr))
}
