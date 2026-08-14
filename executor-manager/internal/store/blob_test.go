package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSaveBlob(t *testing.T) {
	root := t.TempDir()
	ref, n, err := SaveBlob(root, "user-1", "image.png", bytes.NewBufferString("pixels"), 16)
	if err != nil || n != 6 {
		t.Fatalf("ref=%q n=%d err=%v", ref, n, err)
	}
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ref)))
	if err != nil || string(b) != "pixels" {
		t.Fatalf("data=%q err=%v", b, err)
	}
}

func TestBlobStoreEnforcesUserQuotaValidatesAndDeletes(t *testing.T) {
	s := NewBlobStore(t.TempDir(), 8, 10)
	ref, _, err := s.Save("user", "first.bin", bytes.NewBufferString("123456"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateRefs("user", []string{ref}); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateRefs("other", []string{ref}); err == nil {
		t.Fatal("cross-user reference accepted")
	}
	if _, _, err := s.Save("user", "second.bin", bytes.NewBufferString("12345")); !errors.Is(err, ErrBlobQuotaExceeded) {
		t.Fatalf("quota err=%v", err)
	}
	if err := s.Delete("user", ref); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateRefs("user", []string{ref}); err == nil {
		t.Fatal("deleted reference still validates")
	}
	if _, _, err := s.Save("user", "second.bin", bytes.NewBufferString("12345")); err != nil {
		t.Fatalf("quota was not released: %v", err)
	}
}

func TestBlobStoreSerializesQuotaForSameUser(t *testing.T) {
	s := NewBlobStore(t.TempDir(), 8, 10)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.Save("user", "data.bin", bytes.NewBufferString("123456"))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	var succeeded, rejected int
	for err := range errs {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrBlobQuotaExceeded) {
			rejected++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("succeeded=%d rejected=%d", succeeded, rejected)
	}
}

func TestSaveBlobRejectsTraversalAndRemovesOversizedPartial(t *testing.T) {
	root := t.TempDir()
	if _, _, err := SaveBlob(root, "../user", "x", bytes.NewBufferString("x"), 1); err == nil {
		t.Fatal("expected invalid user")
	}
	if _, _, err := SaveBlob(root, ".", "x", bytes.NewBufferString("x"), 1); err == nil {
		t.Fatal("expected dot user to be rejected")
	}
	if _, _, err := SaveBlob(root, "user", "../x", bytes.NewBufferString("x"), 1); err == nil {
		t.Fatal("expected invalid filename")
	}
	if _, _, err := SaveBlob(root, "user", "large.bin", bytes.NewBuffer(make([]byte, 32)), 8); err == nil {
		t.Fatal("expected size error")
	}
	entries, err := os.ReadDir(filepath.Join(root, "user"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial files=%d", len(entries))
	}
}
