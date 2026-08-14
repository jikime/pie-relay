package credential

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreWritesAndAtomicallyReplacesCredential(t *testing.T) {
	root := t.TempDir()
	store, err := New(root, "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Write("owner-a", ".partner/credential.json", []byte(`{"pat":"one"}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Write("owner-a", ".partner/credential.json", []byte(`{"pat":"two"}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == second.Digest {
		t.Fatal("credential digest did not change")
	}
	path := filepath.Join(root, "owner-a", ".partner", "credential.json")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"pat":"two"}` {
		t.Fatalf("data=%s err=%v", data, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}

func TestStoreRejectsTraversalSymlinkAndNonObject(t *testing.T) {
	root := t.TempDir()
	store, _ := New(root, "")
	if _, err := store.Write("owner-a", "../escape.json", []byte(`{"pat":"x"}`), 1024); err == nil {
		t.Fatal("accepted traversal")
	}
	if _, err := store.Write("owner-a", "credential.json", []byte(`[]`), 1024); err == nil {
		t.Fatal("accepted non-object")
	}
	if err := os.MkdirAll(filepath.Join(root, "owner-a"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".", filepath.Join(root, "owner-a", "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Write("owner-a", "linked/credential.json", []byte(`{"pat":"x"}`), 1024); err == nil {
		t.Fatal("accepted symlink parent")
	}
}
