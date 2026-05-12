package sessions

import (
	"path/filepath"
	"testing"
	"time"
)

func seedSession(t *testing.T, store *Store, id string, n int) {
	t.Helper()
	if err := store.UpsertSession(id, "cli", ""); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		role := RoleUser
		if i%2 == 1 {
			role = RoleAssistant
		}
		if err := store.AppendMessage(id, Message{
			Role:      role,
			Content:   "msg-" + string(rune('A'+i)),
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompactRecordsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	seedSession(t, store, "s1", 10)

	removed, err := store.Compact("s1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 7 {
		t.Fatalf("removed: got %d want 7", removed)
	}

	list := store.CompactionList("s1")
	if len(list) != 1 {
		t.Fatalf("CompactionList: got %d records, want 1", len(list))
	}
	rec := list[0]
	if rec.RemovedCount != 7 || rec.KeepN != 3 {
		t.Fatalf("record metadata: removed=%d keepN=%d", rec.RemovedCount, rec.KeepN)
	}
	if len(rec.PreMessages) != 10 {
		t.Fatalf("PreMessages: got %d want 10", len(rec.PreMessages))
	}

	got, ok := store.CompactionGet(rec.ID)
	if !ok {
		t.Fatal("CompactionGet should find the record we just listed")
	}
	if got.ID != rec.ID || len(got.PreMessages) != 10 {
		t.Fatalf("CompactionGet returned mismatched record: %+v", got)
	}
}

func TestCompactNoRemoveDoesNotRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	store, _ := New(path)
	seedSession(t, store, "s1", 3)

	// keepN >= total — should not trim and should not record.
	removed, err := store.Compact("s1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed: got %d want 0", removed)
	}
	if list := store.CompactionList("s1"); len(list) != 0 {
		t.Fatalf("no-op compaction must not record; got %d records", len(list))
	}
}

func TestCompactionRestoreReplacesMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	store, _ := New(path)
	seedSession(t, store, "s1", 8)

	_, _ = store.Compact("s1", 2)
	hist, _ := store.History("s1")
	if len(hist) != 2 {
		t.Fatalf("after compact: got %d want 2", len(hist))
	}

	rec := store.CompactionList("s1")[0]
	if err := store.CompactionRestore(rec.ID); err != nil {
		t.Fatal(err)
	}
	hist, _ = store.History("s1")
	if len(hist) != 8 {
		t.Fatalf("after restore: got %d want 8", len(hist))
	}
}

func TestCompactionBranchForksWithoutMutatingParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	store, _ := New(path)
	seedSession(t, store, "parent", 6)
	_, _ = store.Compact("parent", 2)

	rec := store.CompactionList("parent")[0]
	branch, err := store.CompactionBranch(rec.ID, "branch-1")
	if err != nil {
		t.Fatal(err)
	}
	if branch.ID != "branch-1" {
		t.Fatalf("branch id: got %q want branch-1", branch.ID)
	}
	if len(branch.Messages) != 6 {
		t.Fatalf("branch messages: got %d want 6", len(branch.Messages))
	}

	// Parent must still hold the post-compact 2 messages, not the snapshot.
	parentHist, _ := store.History("parent")
	if len(parentHist) != 2 {
		t.Fatalf("parent should be unchanged; got %d msgs", len(parentHist))
	}
}

func TestCompactionBranchAutoIDWhenEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	store, _ := New(path)
	seedSession(t, store, "p", 4)
	_, _ = store.Compact("p", 1)
	rec := store.CompactionList("p")[0]

	branch, err := store.CompactionBranch(rec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if branch.ID == "" {
		t.Fatal("expected auto-generated branch id")
	}
}

func TestCompactionBranchRejectsCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	store, _ := New(path)
	seedSession(t, store, "p", 4)
	seedSession(t, store, "already-exists", 1)
	_, _ = store.Compact("p", 1)
	rec := store.CompactionList("p")[0]

	if _, err := store.CompactionBranch(rec.ID, "already-exists"); err == nil {
		t.Fatal("expected error when branching onto existing session id")
	}
}

func TestCompactionPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")

	store1, _ := New(path)
	seedSession(t, store1, "s1", 5)
	_, _ = store1.Compact("s1", 2)
	firstList := store1.CompactionList("s1")
	if len(firstList) != 1 {
		t.Fatalf("pre-reopen: got %d records", len(firstList))
	}

	store2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := store2.CompactionList("s1")
	if len(reloaded) != 1 {
		t.Fatalf("after reopen: got %d records want 1", len(reloaded))
	}
	if reloaded[0].ID != firstList[0].ID {
		t.Fatalf("reloaded record id %q != original %q", reloaded[0].ID, firstList[0].ID)
	}
	if len(reloaded[0].PreMessages) != 5 {
		t.Fatalf("reloaded PreMessages: got %d want 5", len(reloaded[0].PreMessages))
	}
}

func TestCompactionGetNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	store, _ := New(path)
	if _, ok := store.CompactionGet("missing"); ok {
		t.Fatal("CompactionGet should return false for unknown id")
	}
	if err := store.CompactionRestore("missing"); err == nil {
		t.Fatal("CompactionRestore should error for unknown id")
	}
	if _, err := store.CompactionBranch("missing", ""); err == nil {
		t.Fatal("CompactionBranch should error for unknown id")
	}
}

func TestCompactionListChronologicalOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	store, _ := New(path)
	seedSession(t, store, "s", 6)

	// Three compactions in a row; the first one trims to keep=4 (removes 2),
	// the next to keep=3 (removes 1), the next to keep=2 (removes 1).
	for _, keep := range []int{4, 3, 2} {
		if _, err := store.Compact("s", keep); err != nil {
			t.Fatal(err)
		}
	}
	list := store.CompactionList("s")
	if len(list) != 3 {
		t.Fatalf("got %d records want 3", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i].At.Before(list[i-1].At) {
			t.Fatalf("records out of order: %v then %v", list[i-1].At, list[i].At)
		}
	}
}
