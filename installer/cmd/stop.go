package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"gitlab.futo.org/stonefruit/stonefruit-server/installer/internal/docker"
)

var stopComposeDir string

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the Stonefruit server (containers stay on disk)",
	RunE: func(cmd *cobra.Command, args []string) error {
		workDir := resolveWorkDir(stopComposeDir)

		cfg, err := docker.ParseExistingCompose(workDir)
		if err != nil {
			return fmt.Errorf("read compose: %w", err)
		}
		if cfg == nil {
			return fmt.Errorf("no Stonefruit install in %s (run `stonefruit setup` first)", workDir)
		}

		fmt.Print("  Stopping containers... ")
		if err := docker.ComposeStop(workDir, os.Stdout); err != nil {
			return err
		}
		fmt.Println("done")
		fmt.Println()
		fmt.Println("  Stonefruit is stopped. Run `stonefruit start` to resume.")
		return nil
	},
}

func init() {
	stopCmd.Flags().StringVar(&stopComposeDir, "compose-dir", ".", "directory with docker-compose.yml")
}
