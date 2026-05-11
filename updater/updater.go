package updater

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	defaultReleaseAPI = "https://api.github.com/repos/maximo896/aspanel/releases/latest"
)

type releaseResponse struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func HandleUpdateCLI(args []string) ([]string, bool, error) {
	shouldUpdate, remaining := parseUpdateFlags(args)
	if !shouldUpdate {
		return args, false, nil
	}
	if err := selfUpdate(); err != nil {
		return remaining, true, err
	}
	if err := restartSelf(remaining); err != nil {
		return remaining, true, err
	}
	return remaining, true, nil
}

func parseUpdateFlags(args []string) (bool, []string) {
	out := make([]string, 0, len(args))
	update := false
	for _, arg := range args {
		switch strings.TrimSpace(arg) {
		case "-up", "--update":
			update = true
		default:
			out = append(out, arg)
		}
	}
	return update, out
}

func selfUpdate() error {
	assetName := fmt.Sprintf("awvs-sqlmap-panel-%s-%s", runtime.GOOS, runtime.GOARCH)
	downloadURL, tagName, err := findLatestReleaseAsset(defaultReleaseAPI, assetName)
	if err != nil {
		return err
	}
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	resolvedPath, err := filepath.EvalSymlinks(exePath)
	if err == nil && strings.TrimSpace(resolvedPath) != "" {
		exePath = resolvedPath
	}
	targetDir := filepath.Dir(exePath)
	tempPath := filepath.Join(targetDir, "."+filepath.Base(exePath)+".new")
	if err := downloadFile(downloadURL, tempPath); err != nil {
		return err
	}
	if err := os.Chmod(tempPath, 0755); err != nil {
		return err
	}
	if err := os.Rename(tempPath, exePath); err != nil {
		return fmt.Errorf("replace binary failed: %w", err)
	}
	log.Printf("[update] updated binary from release tag=%s asset=%s", tagName, assetName)
	return nil
}

func findLatestReleaseAsset(releaseAPI, assetName string) (string, string, error) {
	req, _ := http.NewRequest("GET", releaseAPI, nil)
	req.Header.Set("User-Agent", "awvs-sqlmap-panel-updater")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("release api status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var release releaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", "", err
	}
	for _, item := range release.Assets {
		if strings.TrimSpace(item.Name) == assetName {
			return strings.TrimSpace(item.URL), strings.TrimSpace(release.TagName), nil
		}
	}
	return "", strings.TrimSpace(release.TagName), fmt.Errorf("asset not found in latest release: %s", assetName)
}

func downloadFile(downloadURL, targetPath string) error {
	req, _ := http.NewRequest("GET", downloadURL, nil)
	req.Header.Set("User-Agent", "awvs-sqlmap-panel-updater")
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("download failed status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	file, err := os.Create(targetPath)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := io.Copy(file, resp.Body); err != nil {
		return err
	}
	return nil
}

func restartSelf(args []string) error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	resolvedPath, err := filepath.EvalSymlinks(exePath)
	if err == nil && strings.TrimSpace(resolvedPath) != "" {
		exePath = resolvedPath
	}
	restartArgs := append([]string{exePath}, args...)
	if runtime.GOOS != "windows" {
		return syscall.Exec(exePath, restartArgs, os.Environ())
	}
	cmd := exec.Command(exePath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Start(); err != nil {
		return err
	}
	return nil
}
