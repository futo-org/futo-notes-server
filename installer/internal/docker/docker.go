package docker

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

func CheckDocker() (string, error) {
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return "", fmt.Errorf("docker is not available: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ComposePull runs `docker compose pull`. Output is written to w (pass
// os.Stdout for passthrough or a bytes.Buffer to capture for later
// error reporting).
func ComposePull(workDir string, w io.Writer) error {
	return runCompose(workDir, w, "pull")
}

func ComposeUp(workDir string, w io.Writer) error {
	return runCompose(workDir, w, "up", "-d", "--remove-orphans")
}

func ComposeUpRecreate(workDir string, w io.Writer) error {
	return runCompose(workDir, w, "up", "-d", "--force-recreate", "--remove-orphans")
}

func ComposeDown(workDir string, w io.Writer) error {
	return runCompose(workDir, w, "down")
}

func ComposeStop(workDir string, w io.Writer) error {
	return runCompose(workDir, w, "stop")
}

func runCompose(workDir string, w io.Writer, args ...string) error {
	full := append([]string{"compose"}, args...)
	cmd := exec.Command("docker", full...)
	cmd.Dir = workDir
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

// GetContainerImageID returns the image digest of a running container, or
// "" if the container doesn't exist.
func GetContainerImageID(name string) string {
	out, err := exec.Command("docker", "inspect", "--format", "{{.Image}}", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// getContainerComposeDir returns the compose working dir label a container
// was created from, "" if the container has no such label, or an error if
// inspect fails for a reason other than "no such object".
func getContainerComposeDir(name string) (string, bool, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("docker", "inspect", "--format",
		`{{index .Config.Labels "com.docker.compose.project.working_dir"}}`, name)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// "No such object" / "no such container" is benign — the
		// container doesn't exist. Docker's capitalization varies
		// across versions, so compare case-insensitively.
		lower := strings.ToLower(stderr.String())
		if strings.Contains(lower, "no such object") || strings.Contains(lower, "no such container") {
			return "", false, nil
		}
		return "", false, fmt.Errorf("docker inspect %s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), true, nil
}

// RemoveStaleContainers removes Stonefruit containers created from a
// compose working dir other than workDir. Prevents "container name in use"
// errors when a user re-runs setup after deleting an old install dir.
// Containers from the current workDir are left alone so `docker compose up`
// can reuse them.
func RemoveStaleContainers(workDir string, log func(string)) error {
	for _, name := range []string{"stonefruit", "stonefruit-postgres"} {
		composeDir, exists, err := getContainerComposeDir(name)
		if err != nil {
			return err
		}
		if !exists || composeDir == workDir {
			continue
		}
		origin := composeDir
		if origin == "" {
			origin = "unknown"
		}
		if log != nil {
			log(fmt.Sprintf("removing stale %s container from %s", name, origin))
		}
		if err := exec.Command("docker", "rm", "-f", name).Run(); err != nil {
			return fmt.Errorf("failed to remove stale container %s: %w", name, err)
		}
	}
	return nil
}
