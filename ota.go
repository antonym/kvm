package kvm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
)

type UpdateMetadata struct {
	AppVersion    string `json:"appVersion"`
	AppUrl        string `json:"appUrl"`
	AppHash       string `json:"appHash"`
	SystemVersion string `json:"systemVersion"`
	SystemUrl     string `json:"systemUrl"`
	SystemHash    string `json:"systemHash"`
}

type LocalMetadata struct {
	AppVersion    string `json:"appVersion"`
	SystemVersion string `json:"systemVersion"`
}

// UpdateStatus represents the current update status
type UpdateStatus struct {
	Local                 *LocalMetadata  `json:"local"`
	Remote                *UpdateMetadata `json:"remote"`
	SystemUpdateAvailable bool            `json:"systemUpdateAvailable"`
	AppUpdateAvailable    bool            `json:"appUpdateAvailable"`
}

const UpdateMetadataUrl = "https://api.jetkvm.com/releases"

var builtAppVersion = "0.1.0+dev"

func GetLocalVersion() (systemVersion *semver.Version, appVersion *semver.Version, err error) {
	appVersion, err = semver.NewVersion(builtAppVersion)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid built-in app version: %w", err)
	}

	systemVersionBytes, err := os.ReadFile("/version")
	if err != nil {
		return nil, appVersion, fmt.Errorf("error reading system version: %w", err)
	}

	systemVersion, err = semver.NewVersion(strings.TrimSpace(string(systemVersionBytes)))
	if err != nil {
		return nil, appVersion, fmt.Errorf("invalid system version: %w", err)
	}

	return systemVersion, appVersion, nil
}

func fetchUpdateMetadata(ctx context.Context, deviceId string, includePreRelease bool) (*UpdateMetadata, error) {
	metadata := &UpdateMetadata{}

	updateUrl, err := url.Parse(UpdateMetadataUrl)
	if err != nil {
		return nil, fmt.Errorf("error parsing update metadata URL: %w", err)
	}

	query := updateUrl.Query()
	query.Set("deviceId", deviceId)
	query.Set("prerelease", fmt.Sprintf("%v", includePreRelease))
	updateUrl.RawQuery = query.Encode()

	logger.Infof("Checking for updates at: %s", updateUrl)

	req, err := http.NewRequestWithContext(ctx, "GET", updateUrl.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	err = json.NewDecoder(resp.Body).Decode(metadata)
	if err != nil {
		return nil, fmt.Errorf("error decoding response: %w", err)
	}

	return metadata, nil
}

func downloadFile(ctx context.Context, path string, url string, downloadProgress *float32) error {
	if _, err := os.Stat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("error removing existing file: %w", err)
		}
	}

	unverifiedPath := path + ".unverified"
	if _, err := os.Stat(unverifiedPath); err == nil {
		if err := os.Remove(unverifiedPath); err != nil {
			return fmt.Errorf("error removing existing unverified file: %w", err)
		}
	}

	file, err := os.Create(unverifiedPath)
	if err != nil {
		return fmt.Errorf("error creating file: %w", err)
	}
	defer file.Close()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("error creating request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("error downloading file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	totalSize := resp.ContentLength
	if totalSize <= 0 {
		return fmt.Errorf("invalid content length")
	}

	var written int64
	buf := make([]byte, 32*1024)
	for {
		nr, er := resp.Body.Read(buf)
		if nr > 0 {
			nw, ew := file.Write(buf[0:nr])
			if nw < nr {
				return fmt.Errorf("short write: %d < %d", nw, nr)
			}
			written += int64(nw)
			if ew != nil {
				return fmt.Errorf("error writing to file: %w", ew)
			}
			progress := float32(written) / float32(totalSize)
			if progress-*downloadProgress >= 0.01 {
				*downloadProgress = progress
				triggerOTAStateUpdate()
			}
		}
		if er != nil {
			if er == io.EOF {
				break
			}
			return fmt.Errorf("error reading response body: %w", er)
		}
	}

	file.Close()

	// Flush filesystem buffers to ensure all data is written to disk
	err = exec.Command("sync").Run()
	if err != nil {
		return fmt.Errorf("error flushing filesystem buffers: %w", err)
	}

	// Clear the filesystem caches to force a read from disk
	err = os.WriteFile("/proc/sys/vm/drop_caches", []byte("1"), 0644)
	if err != nil {
		return fmt.Errorf("error clearing filesystem caches: %w", err)
	}

	return nil
}

func verifyFile(path string, expectedHash string, verifyProgress *float32) error {
	unverifiedPath := path + ".unverified"
	fileToHash, err := os.Open(unverifiedPath)
	if err != nil {
		return fmt.Errorf("error opening file for hashing: %w", err)
	}
	defer fileToHash.Close()

	hash := sha256.New()
	fileInfo, err := fileToHash.Stat()
	if err != nil {
		return fmt.Errorf("error getting file info: %w", err)
	}
	totalSize := fileInfo.Size()

	buf := make([]byte, 32*1024)
	verified := int64(0)

	for {
		nr, er := fileToHash.Read(buf)
		if nr > 0 {
			nw, ew := hash.Write(buf[0:nr])
			if nw < nr {
				return fmt.Errorf("short write: %d < %d", nw, nr)
			}
			verified += int64(nw)
			if ew != nil {
				return fmt.Errorf("error writing to hash: %w", ew)
			}
			progress := float32(verified) / float32(totalSize)
			if progress-*verifyProgress >= 0.01 {
				*verifyProgress = progress
				triggerOTAStateUpdate()
			}
		}
		if er != nil {
			if er == io.EOF {
				break
			}
			return fmt.Errorf("error reading file: %w", er)
		}
	}

	hashSum := hash.Sum(nil)
	logger.Infof("SHA256 hash of %s: %x", path, hashSum)

	if hex.EncodeToString(hashSum) != expectedHash {
		return fmt.Errorf("hash mismatch: %x != %s", hashSum, expectedHash)
	}

	if err := os.Rename(unverifiedPath, path); err != nil {
		return fmt.Errorf("error renaming file: %w", err)
	}

	if err := os.Chmod(path, 0755); err != nil {
		return fmt.Errorf("error making file executable: %w", err)
	}

	return nil
}

type OTAState struct {
	Updating                   bool       `json:"updating"`
	Error                      string     `json:"error,omitempty"`
	MetadataFetchedAt          *time.Time `json:"metadataFetchedAt,omitempty"`
	AppUpdatePending           bool       `json:"appUpdatePending"`
	SystemUpdatePending        bool       `json:"systemUpdatePending"`
	AppDownloadProgress        float32    `json:"appDownloadProgress,omitempty"` //TODO: implement for progress bar
	AppDownloadFinishedAt      *time.Time `json:"appDownloadFinishedAt,omitempty"`
	SystemDownloadProgress     float32    `json:"systemDownloadProgress,omitempty"` //TODO: implement for progress bar
	SystemDownloadFinishedAt   *time.Time `json:"systemDownloadFinishedAt,omitempty"`
	AppVerificationProgress    float32    `json:"appVerificationProgress,omitempty"`
	AppVerifiedAt              *time.Time `json:"appVerifiedAt,omitempty"`
	SystemVerificationProgress float32    `json:"systemVerificationProgress,omitempty"`
	SystemVerifiedAt           *time.Time `json:"systemVerifiedAt,omitempty"`
	AppUpdateProgress          float32    `json:"appUpdateProgress,omitempty"` //TODO: implement for progress bar
	AppUpdatedAt               *time.Time `json:"appUpdatedAt,omitempty"`
	SystemUpdateProgress       float32    `json:"systemUpdateProgress,omitempty"` //TODO: port rk_ota, then implement
	SystemUpdatedAt            *time.Time `json:"systemUpdatedAt,omitempty"`
}

var otaState = OTAState{}

func triggerOTAStateUpdate() {
	go func() {
		if currentSession == nil {
			logger.Info("No active RPC session, skipping update state update")
			return
		}
		writeJSONRPCEvent("otaState", otaState, currentSession)
	}()
}

func TryUpdate(ctx context.Context, deviceId string, includePreRelease bool) error {
	logger.Info("Trying to update...")
	if otaState.Updating {
		return fmt.Errorf("update already in progress")
	}

	otaState = OTAState{
		Updating: true,
	}
	triggerOTAStateUpdate()

	defer func() {
		otaState.Updating = false
		triggerOTAStateUpdate()
	}()

	updateStatus, err := GetUpdateStatus(ctx, deviceId, includePreRelease)
	if err != nil {
		otaState.Error = fmt.Sprintf("Error checking for updates: %v", err)
		return fmt.Errorf("error checking for updates: %w", err)
	}

	now := time.Now()
	otaState.MetadataFetchedAt = &now
	otaState.AppUpdatePending = updateStatus.AppUpdateAvailable
	otaState.SystemUpdatePending = updateStatus.SystemUpdateAvailable
	triggerOTAStateUpdate()

	local := updateStatus.Local
	remote := updateStatus.Remote
	appUpdateAvailable := updateStatus.AppUpdateAvailable
	systemUpdateAvailable := updateStatus.SystemUpdateAvailable

	rebootNeeded := false

	if appUpdateAvailable {
		logger.Infof("App update available: %s -> %s", local.AppVersion, remote.AppVersion)

		err := downloadFile(ctx, "/userdata/jetkvm/jetkvm_app.update", remote.AppUrl, &otaState.AppDownloadProgress)
		if err != nil {
			otaState.Error = fmt.Sprintf("Error downloading app update: %v", err)
			triggerOTAStateUpdate()
			return err
		}
		downloadFinished := time.Now()
		otaState.AppDownloadFinishedAt = &downloadFinished
		otaState.AppDownloadProgress = 1
		triggerOTAStateUpdate()

		err = verifyFile("/userdata/jetkvm/jetkvm_app.update", remote.AppHash, &otaState.AppVerificationProgress)
		if err != nil {
			otaState.Error = fmt.Sprintf("Error verifying app update hash: %v", err)
			triggerOTAStateUpdate()
			return err
		}
		verifyFinished := time.Now()
		otaState.AppVerifiedAt = &verifyFinished
		otaState.AppVerificationProgress = 1
		otaState.AppUpdatedAt = &verifyFinished
		otaState.AppUpdateProgress = 1
		triggerOTAStateUpdate()

		logger.Info("App update downloaded")
		rebootNeeded = true
	} else {
		logger.Info("App is up to date")
	}

	if systemUpdateAvailable {
		logger.Infof("System update available: %s -> %s", local.SystemVersion, remote.SystemVersion)
		err := downloadFile(ctx, "/userdata/jetkvm/update_system.tar", remote.SystemUrl, &otaState.SystemDownloadProgress)
		if err != nil {
			otaState.Error = fmt.Sprintf("Error downloading system update: %v", err)
			triggerOTAStateUpdate()
			return err
		}
		downloadFinished := time.Now()
		otaState.SystemDownloadFinishedAt = &downloadFinished
		otaState.SystemDownloadProgress = 1
		triggerOTAStateUpdate()

		err = verifyFile("/userdata/jetkvm/update_system.tar", remote.SystemHash, &otaState.SystemVerificationProgress)
		if err != nil {
			otaState.Error = fmt.Sprintf("Error verifying system update hash: %v", err)
			triggerOTAStateUpdate()
			return err
		}
		logger.Info("System update downloaded")
		verifyFinished := time.Now()
		otaState.SystemVerifiedAt = &verifyFinished
		otaState.SystemVerificationProgress = 1
		triggerOTAStateUpdate()

		cmd := exec.Command("rk_ota", "--misc=update", "--tar_path=/userdata/jetkvm/update_system.tar", "--save_dir=/userdata/jetkvm/ota_save", "--partition=all")
		var b bytes.Buffer
		cmd.Stdout = &b
		cmd.Stderr = &b
		err = cmd.Start()
		if err != nil {
			otaState.Error = fmt.Sprintf("Error starting rk_ota command: %v", err)
			return fmt.Errorf("error starting rk_ota command: %w", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() {
			ticker := time.NewTicker(1800 * time.Millisecond)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					if otaState.SystemUpdateProgress >= 0.99 {
						return
					}
					otaState.SystemUpdateProgress += 0.01
					if otaState.SystemUpdateProgress > 0.99 {
						otaState.SystemUpdateProgress = 0.99
					}
					triggerOTAStateUpdate()
				case <-ctx.Done():
					return
				}
			}
		}()

		err = cmd.Wait()
		cancel()
		output := b.String()
		if err != nil {
			otaState.Error = fmt.Sprintf("Error executing rk_ota command: %v\nOutput: %s", err, output)
			return fmt.Errorf("error executing rk_ota command: %w\nOutput: %s", err, output)
		}

		logger.Infof("rk_ota success, output: %s", output)
		otaState.SystemUpdateProgress = 1
		otaState.SystemUpdatedAt = &verifyFinished
		triggerOTAStateUpdate()
		rebootNeeded = true
	} else {
		logger.Info("System is up to date")
	}

	if rebootNeeded {
		logger.Info("System Rebooting in 10s")
		time.Sleep(10 * time.Second)
		cmd := exec.Command("reboot")
		err := cmd.Start()
		if err != nil {
			otaState.Error = fmt.Sprintf("Failed to start reboot: %v", err)
			return fmt.Errorf("failed to start reboot: %w", err)
		} else {
			os.Exit(0)
		}
	}

	return nil
}

func GetUpdateStatus(ctx context.Context, deviceId string, includePreRelease bool) (*UpdateStatus, error) {
	// Get local versions
	systemVersionLocal, appVersionLocal, err := GetLocalVersion()
	if err != nil {
		return nil, fmt.Errorf("error getting local version: %w", err)
	}

	// Get remote metadata
	remoteMetadata, err := fetchUpdateMetadata(ctx, deviceId, includePreRelease)
	if err != nil {
		return nil, fmt.Errorf("error checking for updates: %w", err)
	}

	// Build local UpdateMetadata
	localMetadata := &LocalMetadata{
		AppVersion:    appVersionLocal.String(),
		SystemVersion: systemVersionLocal.String(),
	}

	systemVersionRemote, err := semver.NewVersion(remoteMetadata.SystemVersion)
	if err != nil {
		return nil, fmt.Errorf("error parsing remote system version: %w", err)
	}
	appVersionRemote, err := semver.NewVersion(remoteMetadata.AppVersion)
	if err != nil {
		return nil, fmt.Errorf("error parsing remote app version: %w, %s", err, remoteMetadata.AppVersion)
	}

	systemUpdateAvailable := systemVersionRemote.GreaterThan(systemVersionLocal)
	appUpdateAvailable := appVersionRemote.GreaterThan(appVersionLocal)

	// Handle pre-release updates
	isRemoteSystemPreRelease := systemVersionRemote.Prerelease() != ""
	isRemoteAppPreRelease := appVersionRemote.Prerelease() != ""

	if isRemoteSystemPreRelease && !includePreRelease {
		systemUpdateAvailable = false
	}
	if isRemoteAppPreRelease && !includePreRelease {
		appUpdateAvailable = false
	}

	updateStatus := &UpdateStatus{
		Local:                 localMetadata,
		Remote:                remoteMetadata,
		SystemUpdateAvailable: systemUpdateAvailable,
		AppUpdateAvailable:    appUpdateAvailable,
	}

	return updateStatus, nil
}

func IsUpdatePending() bool {
	return otaState.Updating
}

// make sure our current a/b partition is set as default
func confirmCurrentSystem() {
	output, err := exec.Command("rk_ota", "--misc=now").CombinedOutput()
	if err != nil {
		logger.Warnf("failed to set current partition in A/B setup: %s", string(output))
	}
}
