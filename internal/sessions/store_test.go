package sessions

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSetSessionModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSession("sess1", "cli", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSessionModel("sess1", "openai", "gpt-4o"); err != nil {
		t.Fatalf("SetSessionModel: %v", err)
	}
	sess, ok := store.Get("sess1")
	if !ok {
		t.Fatal("session not found after SetSessionModel")
	}
	if sess.Provider != "openai" || sess.Model != "gpt-4o" {
		t.Fatalf("unexpected provider/model: %s/%s", sess.Provider, sess.Model)
	}
}

func TestSetSessionModelNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	store, _ := New(path)
	if err := store.SetSessionModel("nonexistent", "openai", "gpt-4"); err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestSetSessionModelPersistedAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	store, _ := New(path)
	_ = store.UpsertSession("s", "telegram", "")
	_ = store.SetSessionModel("s", "anthropic", "claude-3-5-haiku-20241022")

	// Reload
	store2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := store2.Get("s")
	if sess.Provider != "anthropic" || sess.Model != "claude-3-5-haiku-20241022" {
		t.Fatalf("model not persisted: %s/%s", sess.Provider, sess.Model)
	}
}

func TestSessionMaxMessagesCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	store, _ := New(path)
	store.SetMaxMessages(3)
	_ = store.UpsertSession("m", "cli", "")
	for i := 0; i < 5; i++ {
		_ = store.AppendMessage("m", Message{
			Role:      RoleUser,
			Content:   "msg",
			CreatedAt: time.Now().UTC(),
		})
	}
	hist, _ := store.History("m")
	if len(hist) != 3 {
		t.Fatalf("expected 3 messages (cap), got %d", len(hist))
	}
}

func TestMemoryInlineCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	store, _ := New(path)
	store.SetMemoryCompaction(2, true)
	_ = store.UpsertSession("m", "cli", "")
	for i := 0; i < 5; i++ {
		_ = store.AppendMessage("m", Message{
			Role:      RoleUser,
			Content:   "x",
			CreatedAt: time.Now().UTC(),
		})
	}
	hist, _ := store.History("m")
	if len(hist) != 2 {
		t.Fatalf("expected 2 after memory compaction, got %d", len(hist))
	}
}

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
