package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"gitlab.futo.org/stonefruit/stonefruit-server/installer/internal/api"
	"gitlab.futo.org/stonefruit/stonefruit-server/installer/internal/docker"
)

var (
	statusBaseURL string
	statusJSON    bool
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check server health",
	RunE: func(cmd *cobra.Command, args []string) error {
		baseURL := statusBaseURL
		if baseURL == "" {
			if wd, err := os.Getwd(); err == nil {
				if cfg, _ := docker.ParseExistingCompose(wd); cfg != nil {
					baseURL = "http://localhost:" + strconv.Itoa(cfg.Port)
				}
			}
			if baseURL == "" {
				baseURL = "http://localhost:3000"
			}
		}

		health := api.CheckHealth(baseURL)
		if statusJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(map[string]any{"url": baseURL, "health": health})
		}

		if health == nil {
			fmt.Printf("  Stonefruit server at %s\n", baseURL)
			fmt.Println("  Status: unreachable")
			os.Exit(1)
		}

		fmt.Println()
		fmt.Println("  Stonefruit server status")
		fmt.Printf("    URL:       %s\n", baseURL)
		fmt.Printf("    Health:    %s\n", health.Status)
		fmt.Printf("    Database:  %s\n", health.DB)
		fmt.Println()
		return nil
	},
}

func init() {
	statusCmd.Flags().StringVar(&statusBaseURL, "base-url", "", "server URL (defaults to port from local docker-compose.yml)")
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "output JSON")
}
