package control

import (
	"context"
	"errors"
	"testing"
)

func TestPreviewOwnershipQuotaAndTargetValidation(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	integration, err := service.PutIntegration(ctx, Integration{ID: "integration-a", DisplayName: "Integration A", Status: "active", MaxUsers: 10, MaxProjectsPerUser: 10, MaxPreviewsPerUser: 1, MaxConversationsPerUser: 2, Credential: CredentialProfile{TargetPath: ".pie/credential.json", Format: "json", MaxBytes: 1024}}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutUser(ctx, User{ID: "owner-a", Status: "active"}, 0, MutationMeta{}); err != nil {
		t.Fatal(err)
	}
	binding, err := service.PutIntegrationUser(ctx, IntegrationUser{ID: "binding-a", IntegrationID: integration.ID, ExternalUserID: "external-a", OwnerUserID: "owner-a", Status: "ready"}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	project, err := service.PutProject(ctx, Project{ID: "project-a", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: "owner-a", Name: "Project A", Locale: "ko", Status: "ready"}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.PutPreview(ctx, Preview{ID: "preview-a", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: "owner-a", ProjectID: project.ID, AppPath: "company-landing", Hostname: "p-abcdefghijklmnopqrstuvwx23.preview.kroot.io", BackendHost: "preview-backend-1234", Port: 20000, Profile: "next", Visibility: "private", Status: "starting"}, 0, MutationMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := service.PreviewByHostname(preview.Hostname); !ok {
		t.Fatal("hostname lookup did not return the preview")
	}
	if values := service.PreviewsForIntegrationUser(binding.ID); len(values) != 1 || values[0].ID != preview.ID {
		t.Fatalf("integration user preview index=%+v", values)
	}
	if values := service.PreviewsForProject(project.ID, 1); len(values) != 1 || values[0].ID != preview.ID {
		t.Fatalf("project preview index=%+v", values)
	}
	_, err = service.PutPreview(ctx, Preview{ID: "preview-b", IntegrationID: integration.ID, IntegrationUserID: binding.ID, OwnerUserID: "owner-a", ProjectID: project.ID, Hostname: "p-bbcdefghijklmnopqrstuvwx23.preview.kroot.io", BackendHost: "preview-backend-1234", Port: 20001, Profile: "next", Visibility: "private", Status: "starting"}, 0, MutationMeta{})
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("quota err=%v", err)
	}
	preview.BackendHost = "127.0.0.1"
	if _, err := service.PutPreview(ctx, preview, preview.Version, MutationMeta{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("arbitrary backend target err=%v", err)
	}
	preview.BackendHost = "preview-backend-1234"
	preview.AppPath = "../project-b"
	if _, err := service.PutPreview(ctx, preview, preview.Version, MutationMeta{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("project path escape err=%v", err)
	}
	preview.AppPath = "admin"
	if _, err := service.PutPreview(ctx, preview, preview.Version, MutationMeta{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("application path mutation err=%v", err)
	}
	preview.AppPath = "company-landing"
	preview.Visibility = "public"
	preview.AccessVersion++
	preview, err = service.PutPreview(ctx, preview, preview.Version, MutationMeta{})
	if err != nil || preview.Visibility != "public" || preview.AccessVersion != 1 {
		t.Fatalf("visibility update preview=%+v err=%v", preview, err)
	}
	tampered := preview
	tampered.AccessVersion++
	if _, err := service.PutPreview(ctx, tampered, preview.Version, MutationMeta{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("standalone access generation mutation err=%v", err)
	}
	preview.OwnerUserID = "owner-b"
	if _, err := service.PutPreview(ctx, preview, preview.Version, MutationMeta{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ownership mutation err=%v", err)
	}
	if err := service.DeletePreview(ctx, preview.ID, preview.Version-1, MutationMeta{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale delete version err=%v", err)
	}
	if err := service.DeletePreview(ctx, preview.ID, preview.Version, MutationMeta{ActorUserID: "owner-a"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := service.PreviewByHostname(preview.Hostname); ok || len(service.PreviewsForIntegrationUser(binding.ID)) != 0 || len(service.PreviewsForProject(project.ID, 10)) != 0 {
		t.Fatal("deleted preview remained in an index")
	}
	deletedAudit := false
	for _, event := range service.AuditEvents() {
		if event.Action == "preview.deleted" && event.TargetID == preview.ID && event.ActorUserID == "owner-a" {
			deletedAudit = true
		}
	}
	if !deletedAudit {
		t.Fatal("preview deletion audit was not retained")
	}
}

func TestNormalizePreviewAppPathAndWorkingDirectory(t *testing.T) {
	for input, expected := range map[string]string{"": ".", ".": ".", " company-landing ": "company-landing", "apps//web": "apps/web", "한글 앱": "한글 앱"} {
		actual, ok := NormalizePreviewAppPath(input)
		if !ok || actual != expected {
			t.Fatalf("NormalizePreviewAppPath(%q)=%q,%v want %q,true", input, actual, ok, expected)
		}
	}
	for _, input := range []string{"/workspace/app", "../other", "app/../../other", `app\\other`, "bad\x00path"} {
		if actual, ok := NormalizePreviewAppPath(input); ok {
			t.Fatalf("unsafe path %q normalized to %q", input, actual)
		}
	}
	if actual, ok := ProjectPreviewWorkingDir("project-a", "company-landing"); !ok || actual != "/workspace/projects/project-a/company-landing" {
		t.Fatalf("working directory=%q,%v", actual, ok)
	}
}
