package previewprocess

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPreviewLifecycleUsesProfileWithoutShellInterpolation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	root := t.TempDir()
	project := filepath.Join(root, "project-a")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "package.json"), []byte(`{"scripts":{"dev":"node server.mjs"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	npm := filepath.Join(bin, "npm")
	if err := os.WriteFile(npm, []byte("#!/bin/sh\necho preview-started\ntrap 'exit 0' TERM INT\nwhile :; do sleep 1; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := New(ctx, root, 1)
	// Tie readiness to output from the process under test. Using the unrelated
	// fixture listener alone can report ready before the child has even written
	// its first log line, especially under the race detector.
	manager.probe = func(context.Context, int) error {
		logs, logErr := manager.Logs("preview-a", 1024)
		if logErr == nil && strings.Contains(string(logs), "preview-started") {
			return nil
		}
		return errors.New("preview process has not started")
	}
	status, duplicate, err := manager.Start(Config{ID: "preview-a", ProjectID: "project-a", WorkingDir: project, Hostname: "p-example.preview.localhost", Profile: "npm", Port: port})
	if err != nil || duplicate || status.State != "starting" {
		t.Fatalf("status=%+v duplicate=%v err=%v", status, duplicate, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, _ = manager.Get("preview-a")
		if status.Ready {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !status.Ready || status.State != "running" {
		t.Fatalf("preview did not become ready: %+v", status)
	}
	var logs []byte
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		logs, err = manager.Logs("preview-a", 1024)
		if len(logs) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || string(logs) != "preview-started\n" {
		t.Fatalf("logs=%q err=%v", logs, err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	stopped, err := manager.Stop(stopCtx, "preview-a")
	if err != nil || stopped.State != "stopped" || stopped.Ready {
		t.Fatalf("stopped=%+v err=%v", stopped, err)
	}
}

func TestPreviewRejectsWorkspaceEscapeAndUnsupportedProfile(t *testing.T) {
	root := t.TempDir()
	manager := New(context.Background(), root, 1)
	outside := t.TempDir()
	if _, _, err := manager.Start(Config{ID: "preview-a", ProjectID: "project-a", WorkingDir: outside, Hostname: "p-example.preview.localhost", Profile: "npm", Port: 20000}); err == nil {
		t.Fatal("workspace escape was accepted")
	}
	project := filepath.Join(root, "project-a")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "package.json"), []byte(`{"scripts":{"dev":"node server.mjs"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Start(Config{ID: "preview-a", ProjectID: "project-a", WorkingDir: project, Hostname: "p-example.preview.localhost", Profile: "shell", Port: 20000}); err == nil {
		t.Fatal("unsupported profile was accepted")
	}
}

func TestPreviewUsesOnlyExplicitApplicationPathInsideSelectedProject(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project-a")
	app := filepath.Join(project, "company-landing")
	otherProject := filepath.Join(root, "project-b")
	for _, directory := range []string{app, otherProject} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(app, "package.json"), []byte(`{"scripts":{"dev":"next dev"},"dependencies":{"next":"1.0.0"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	manager := New(context.Background(), root, 1)
	config := Config{ID: "preview-a", ProjectID: "project-a", WorkingDir: app, Hostname: "p-example.preview.localhost", Profile: "auto", Port: 20000}
	if err := manager.validate(config); err != nil {
		t.Fatalf("explicit nested application path was rejected: %v", err)
	}
	command, args, profile, err := resolveCommand(config)
	if err != nil || command != "npm" || profile != "next" || strings.Join(args, " ") != "run dev -- --hostname 0.0.0.0 --port 20000" {
		t.Fatalf("command=%q args=%q profile=%q err=%v", command, args, profile, err)
	}
	config.WorkingDir = otherProject
	if err := manager.validate(config); err == nil || !strings.Contains(err.Error(), "selected project") {
		t.Fatalf("another project path was accepted: %v", err)
	}
}

func TestPreviewRejectsMissingDevScriptAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project-a")
	app := filepath.Join(project, "app")
	if err := os.MkdirAll(app, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "package.json"), []byte(`{"scripts":{"build":"next build"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	config := Config{ID: "preview-a", ProjectID: "project-a", WorkingDir: app, Hostname: "p-example.preview.localhost", Profile: "auto", Port: 20000}
	if _, _, _, err := resolveCommand(config); err == nil || !strings.Contains(err.Error(), "scripts.dev") {
		t.Fatalf("missing dev script err=%v", err)
	}
	outside := t.TempDir()
	link := filepath.Join(project, "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	manager := New(context.Background(), root, 1)
	config.WorkingDir = link
	if err := manager.validate(config); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("symlink escape was accepted: %v", err)
	}
}

func TestDiscoverApplicationsReturnsOnlyBoundedRunnableProjectApps(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project-a")
	for _, directory := range []string{
		project,
		filepath.Join(project, "apps", "web"),
		filepath.Join(project, "tools", "worker"),
		filepath.Join(project, "node_modules", "dependency"),
		filepath.Join(project, ".next", "generated"),
	} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	manifests := map[string]string{
		filepath.Join(project, "package.json"):                               `{"name":"Root Portal","scripts":{"dev":"vite"},"devDependencies":{"vite":"1"}}`,
		filepath.Join(project, "apps", "web", "package.json"):                `{"name":"Customer Web","scripts":{"dev":"next dev"},"dependencies":{"next":"1"}}`,
		filepath.Join(project, "tools", "worker", "package.json"):            `{"name":"Worker","scripts":{"build":"node build.mjs"}}`,
		filepath.Join(project, "node_modules", "dependency", "package.json"): `{"name":"Dependency","scripts":{"dev":"node server.mjs"}}`,
		filepath.Join(project, ".next", "generated", "package.json"):         `{"name":"Generated","scripts":{"dev":"node server.mjs"}}`,
	}
	for target, data := range manifests {
		if err := os.WriteFile(target, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	applications, err := New(context.Background(), root, 1).DiscoverApplications("project-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(applications) != 2 {
		t.Fatalf("applications=%+v", applications)
	}
	if applications[0] != (Application{Path: ".", Name: "Root Portal", Profile: "vite"}) {
		t.Fatalf("root=%+v", applications[0])
	}
	if applications[1] != (Application{Path: "apps/web", Name: "Customer Web", Profile: "next"}) {
		t.Fatalf("nested=%+v", applications[1])
	}
}

func TestDiscoverApplicationsDoesNotFollowDirectorySymlinks(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project-a")
	outside := t.TempDir()
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "package.json"), []byte(`{"name":"Outside","scripts":{"dev":"vite"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(project, "outside")); err != nil {
		t.Fatal(err)
	}
	applications, err := New(context.Background(), root, 1).DiscoverApplications("project-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(applications) != 0 {
		t.Fatalf("symlinked application escaped discovery: %+v", applications)
	}
}
