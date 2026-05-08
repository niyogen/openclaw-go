package secretstore

import (
	"path/filepath"
	"testing"
)

func TestSecretStoreSetGetDelete(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "secrets.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("mykey", "myvalue"); err != nil {
		t.Fatal(err)
	}
	val, err := s.Get("mykey")
	if err != nil {
		t.Fatal(err)
	}
	if val != "myvalue" {
		t.Fatalf("expected myvalue, got %q", val)
	}
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 secret, got %d", len(list))
	}
	// value must not be in list output
	if list[0].Name != "mykey" {
		t.Fatal("wrong name in list")
	}
	deleted, err := s.Delete("mykey")
	if err != nil || !deleted {
		t.Fatalf("Delete: deleted=%v err=%v", deleted, err)
	}
	if len(s.List()) != 0 {
		t.Fatal("expected 0 secrets after delete")
	}
}

func TestSecretStoreNotFound(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "secrets.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Get("nosuchkey")
	if err == nil {
		t.Fatal("expected error for missing key")
	}
}
