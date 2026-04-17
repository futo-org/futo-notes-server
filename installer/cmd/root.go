package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func init() {
	// Preserve AddCommand order in help output instead of sorting alphabetically.
	cobra.EnableCommandSorting = false
}

// Version is set at build time via -ldflags="-X ...cmd.Version=v1.2.3".
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:           "stonefruit",
	Short:         "Self-hosted Stonefruit sync server",
	Version:       Version,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.AddCommand(setupCmd)
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(updateCmd)
	rootCmd.AddCommand(releaseCmd)
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "  error:", err)
		os.Exit(1)
	}
}
