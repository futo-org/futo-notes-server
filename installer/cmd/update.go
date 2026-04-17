package cmd

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"gitlab.futo.org/stonefruit/stonefruit-server/installer/internal/api"
	"gitlab.futo.org/stonefruit/stonefruit-server/installer/internal/docker"
)

var updateComposeDir string

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Pull latest image and restart",
	RunE: func(cmd *cobra.Command, args []string) error {
		workDir := updateComposeDir
		if workDir == "." || workDir == "" {
			if wd, err := os.Getwd(); err == nil {
				workDir = wd
			}
		}

		cfg, err := docker.ParseExistingCompose(workDir)
		if err != nil {
			return fmt.Errorf("read compose: %w", err)
		}
		port := 3000
		if cfg != nil {
			port = cfg.Port
		}
		baseURL := "http://localhost:" + strconv.Itoa(port)

		start := time.Now()
		oldImage := docker.GetContainerImageID("stonefruit")

		fmt.Print("  Pulling latest image... ")
		if err := docker.ComposePull(workDir, os.Stdout); err != nil {
			return err
		}
		fmt.Println("done")

		fmt.Print("  Restarting container... ")
		if err := docker.ComposeUpRecreate(workDir, os.Stdout); err != nil {
			return err
		}
		fmt.Println("done")

		fmt.Print("  Waiting for server... ")
		if err := api.WaitForHealthy(baseURL, 90*time.Second); err != nil {
			return err
		}
		fmt.Println("healthy")

		newImage := docker.GetContainerImageID("stonefruit")
		elapsed := int(time.Since(start).Seconds())

		fmt.Println()
		if oldImage != "" && newImage != "" && oldImage != newImage {
			fmt.Println("  Updated to a new version.")
			fmt.Printf("    Image: %s -> %s\n", short(oldImage), short(newImage))
		} else {
			fmt.Println("  Already up to date.")
			if newImage != "" {
				fmt.Printf("    Image: %s\n", short(newImage))
			}
		}
		fmt.Printf("  Done in %ds.\n\n", elapsed)
		return nil
	},
}

func init() {
	updateCmd.Flags().StringVar(&updateComposeDir, "compose-dir", ".", "directory with docker-compose.yml")
}

func short(id string) string {
	// IDs may be in form "sha256:abcdef…" — strip prefix and take first 12.
	if len(id) > 7 && id[:7] == "sha256:" {
		id = id[7:]
	}
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
