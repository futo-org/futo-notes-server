package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"gitlab.futo.org/stonefruit/stonefruit-server/installer/internal/api"
	"gitlab.futo.org/stonefruit/stonefruit-server/installer/internal/docker"
)

var releaseCmd = &cobra.Command{
	Use:   "release <track>",
	Short: "Switch release track (stable or latest)",
	Long: `Switch which Docker image tag the server follows.

  stable  tagged releases (recommended; the default)
  latest  main-branch rolling builds`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		track := strings.ToLower(strings.TrimSpace(args[0]))
		valid := false
		for _, t := range docker.Tracks {
			if t == track {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("track must be one of: %s", strings.Join(docker.Tracks, ", "))
		}

		workDir, err := os.Getwd()
		if err != nil {
			return err
		}
		cfg, err := docker.ParseExistingCompose(workDir)
		if err != nil {
			return fmt.Errorf("read compose: %w", err)
		}
		if cfg == nil {
			return fmt.Errorf("no install found in %s (run `stonefruit setup` first)", workDir)
		}

		current := cfg.Track
		if current == "" {
			current = docker.DefaultTrack
		}
		if current == track {
			fmt.Printf("  Already on '%s' track.\n", track)
			return nil
		}

		fmt.Printf("  Switching from '%s' to '%s' track\n", current, track)
		cfg.Track = track
		if err := docker.WriteCompose(workDir, *cfg); err != nil {
			return fmt.Errorf("write compose: %w", err)
		}

		fmt.Print("  Pulling image... ")
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
		baseURL := "http://localhost:" + strconv.Itoa(cfg.Port)
		if err := api.WaitForHealthy(baseURL, 90*time.Second); err != nil {
			return err
		}
		fmt.Println("healthy")

		fmt.Println()
		fmt.Printf("  Now on '%s' track.\n\n", track)
		return nil
	},
}
