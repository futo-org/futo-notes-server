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

var (
	setupPort     int
	setupDataPath string
	setupPassword string
	setupYes      bool
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Install or upgrade the Stonefruit server",
	Long: `Interactive installer. Walks you through setup, writes docker-compose.yml,
pulls images, and starts the server. Re-running in a directory that already
has a docker-compose.yml pulls the latest image and restarts.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		workDir, err := os.Getwd()
		if err != nil {
			return err
		}
		if setupYes {
			return runNonInteractive(workDir, setupPort, setupDataPath, setupPassword)
		}
		return tui.Run(workDir, setupPort, setupDataPath)
	},
}

func init() {
	setupCmd.Flags().IntVar(&setupPort, "port", config.DefaultPort, "server port")
	setupCmd.Flags().StringVar(&setupDataPath, "data-path", config.DefaultDataPath, "notes storage directory (bind-mounted into the server container)")
	setupCmd.Flags().StringVar(&setupPassword, "password", "", "admin password (--yes only; ignored if an existing install has a password hash)")
	setupCmd.Flags().BoolVar(&setupYes, "yes", false, "skip interactive TUI and use defaults / provided flags")
}

func runNonInteractive(workDir string, port int, dataPath, password string) error {
	fmt.Println()
	fmt.Println("  Stonefruit Server Setup")
	fmt.Println("  -----------------------")
	fmt.Println()

	fmt.Print("  Checking Docker... ")
	version, err := docker.CheckDocker()
	if err != nil {
		fmt.Println("not available")
		return err
	}
	fmt.Printf("Docker %s\n", version)

	existing, err := docker.ParseExistingCompose(workDir)
	if err != nil {
		return fmt.Errorf("read existing compose: %w", err)
	}

	var cfg config.Config
	newInstall := existing == nil
	if !newInstall {
		cfg = *existing
		fmt.Printf("  Existing install found — upgrading on port %d\n", cfg.Port)
	} else {
		if port < 1 || port > 65535 {
			return fmt.Errorf("invalid port %d", port)
		}
		pw, err := config.GeneratePassword()
		if err != nil {
			return err
		}
		cfg = config.Config{Port: port, DataPath: dataPath, PostgresPassword: pw, Track: docker.DefaultTrack}
		if err := docker.WriteCompose(workDir, cfg); err != nil {
			return fmt.Errorf("write docker-compose.yml: %w", err)
		}
		fmt.Println("  Wrote docker-compose.yml")
	}

	// Password handling: reuse existing hash if present, else hash the given
	// or a generated password.
	existingHash, err := docker.ParseExistingEnv(workDir)
	if err != nil {
		return fmt.Errorf("read existing .env: %w", err)
	}
	passwordHash := existingHash
	var announcedPassword string
	if passwordHash == "" {
		plain := password
		if plain == "" {
			plain, err = config.GeneratePassword()
			if err != nil {
				return err
			}
			announcedPassword = plain
		}
		// Hashing uses the server image; make sure it's pulled first.
		fmt.Print("  Pulling images... ")
		if err := docker.ComposePull(workDir, os.Stdout); err != nil {
			return err
		}
		fmt.Println("done")

		fmt.Print("  Hashing password... ")
		passwordHash, err = docker.HashPassword(docker.ServerImage(cfg.Track), plain)
		if err != nil {
			fmt.Println("failed")
			return err
		}
		fmt.Println("done")

		if err := docker.WriteEnvFile(workDir, passwordHash); err != nil {
			return fmt.Errorf("write .env: %w", err)
		}
	} else {
		fmt.Print("  Pulling images... ")
		if err := docker.ComposePull(workDir, os.Stdout); err != nil {
			return err
		}
		fmt.Println("done")
	}

	if err := docker.RemoveStaleContainers(workDir, func(s string) { fmt.Println("  " + s) }); err != nil {
		return err
	}

	fmt.Print("  Starting containers... ")
	if err := docker.ComposeUp(workDir, os.Stdout); err != nil {
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
	fmt.Println("  Stonefruit server is running!")
	fmt.Println()
	fmt.Printf("    Server URL:  %s\n", baseURL)
	if announcedPassword != "" {
		fmt.Printf("    Password:    %s\n", announcedPassword)
		fmt.Println("    (Save this — it is not shown again.)")
	}
	fmt.Println()
	fmt.Println("  Open Stonefruit, go to Settings > Sync, and enter the server URL and password.")
	fmt.Println()
	return nil
}
