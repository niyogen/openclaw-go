package sessions

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSession("a", "cli", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("a", Message{Role: RoleUser, Content: "x", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.Delete("a")
	if err != nil || !deleted {
		t.Fatalf("Delete: deleted=%v err=%v", deleted, err)
	}
	deleted, err = store.Delete("a")
	if err != nil || deleted {
		t.Fatalf("second Delete: deleted=%v err=%v", deleted, err)
	}
}
