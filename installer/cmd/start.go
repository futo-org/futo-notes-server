package cmd

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"gitlab.futo.org/stonefruit/stonefruit-server/installer/internal/api"
	"gitlab.futo.org/stonefruit/stonefruit-server/installer/internal/config"
	"gitlab.futo.org/stonefruit/stonefruit-server/installer/internal/docker"
	"gitlab.futo.org/stonefruit/stonefruit-server/installer/internal/tui"
)

var startComposeDir string

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Stonefruit server (runs setup if not installed)",
	RunE: func(cmd *cobra.Command, args []string) error {
		workDir := resolveWorkDir(startComposeDir)

		cfg, err := docker.ParseExistingCompose(workDir)
		if err != nil {
			return fmt.Errorf("read compose: %w", err)
		}
		if cfg == nil {
			fmt.Println("  No Stonefruit install found here — running setup.")
			fmt.Println()
			return tui.Run(workDir, config.DefaultPort, config.DefaultDataPath)
		}

		if err := docker.RemoveStaleContainers(workDir, func(s string) { fmt.Println("  " + s) }); err != nil {
			return err
		}

		fmt.Print("  Starting containers... ")
		if err := docker.ComposeUp(workDir, os.Stdout); err != nil {
			return err
		}
		fmt.Println("done")

		baseURL := "http://localhost:" + strconv.Itoa(cfg.Port)
		fmt.Print("  Waiting for server... ")
		if err := api.WaitForHealthy(baseURL, 90*time.Second); err != nil {
			return err
		}
		fmt.Println("healthy")

		fmt.Println()
		fmt.Printf("  Stonefruit is running at %s\n\n", baseURL)
		return nil
	},
}

func init() {
	startCmd.Flags().StringVar(&startComposeDir, "compose-dir", ".", "directory with docker-compose.yml")
}

func resolveWorkDir(flag string) string {
	if flag != "" && flag != "." {
		return flag
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}
