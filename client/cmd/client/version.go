package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
)

// 릴리스 워크플로가 -ldflags -X로 값을 주입한다. 개발 빌드는 출처가
// 불분명한 버전으로 가장하지 않도록 명시적으로 dev를 표시한다.
var (
	clientVersion   = "dev"
	clientCommit    = "unknown"
	clientBuildDate = "unknown"
)

type clientVersionInfo struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

func currentClientVersion() clientVersionInfo {
	return clientVersionInfo{
		Name:      "pie-client",
		Version:   normalizedBuildValue(clientVersion, "dev"),
		Commit:    normalizedBuildValue(clientCommit, "unknown"),
		BuildDate: normalizedBuildValue(clientBuildDate, "unknown"),
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

func normalizedBuildValue(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

func runClientVersion(args []string) {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "JSON 형식으로 출력")
	_ = fs.Parse(args)
	value := currentClientVersion()
	if *asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
			fmt.Fprintln(os.Stderr, "Pie Client 버전 출력 실패:", err)
			os.Exit(1)
		}
		return
	}
	fmt.Printf("%s %s (%s, %s, %s)\n", value.Name, value.Version, value.Commit, value.BuildDate, value.Platform)
}
