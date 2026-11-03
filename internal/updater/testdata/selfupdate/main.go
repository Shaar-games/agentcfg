// This fixture exercises self-update from a process that is still executing
// the binary being replaced, including Windows' mapped-image rename behavior.
package main

import (
	"context"
	"fmt"
	"os"

	"agentcfg/internal/updater"
)

func main() {
	result, err := updater.Update(context.Background(), updater.Options{
		CurrentVersion: "v0.1.0",
		LatestAPI:      os.Getenv("AGENTCFG_TEST_LATEST_API"),
		DownloadBase:   os.Getenv("AGENTCFG_TEST_DOWNLOAD_BASE"),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("updated", result.Version)
	if result.BackupPath != "" {
		fmt.Println("retained backup", result.BackupPath)
	}
}
