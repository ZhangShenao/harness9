package memory_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/harness9/internal/memory"
)

func TestFileRecordStore_AppendAndList(t *testing.T) {
	dir := t.TempDir()
	store := memory.NewFileRecordStore(dir)

	r1 := memory.CompactionRecord{
		ID:           "rec-001",
		SessionID:    "sess1",
		Tier:         memory.TierFull,
		TokensBefore: 45200,
		TokensAfter:  8100,
		MsgsBefore:   45,
		MsgsAfter:    8,
		Anchors: []memory.Anchor{
			{Type: memory.AnchorUserIntent, Content: "test intent"},
		},
		Summarized:    28,
		PreservedTail: 6,
	}
	r1.FillDefaults()

	if err := store.Append(r1); err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	r2 := memory.CompactionRecord{
		ID:        "rec-002",
		SessionID: "sess1",
		Tier:      memory.TierWarn,
	}
	r2.FillDefaults()
	store.Append(r2)

	records, err := store.List("sess1")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("want 2 records, got %d", len(records))
	}
	if records[0].ID != "rec-001" {
		t.Errorf("first record ID: want rec-001, got %s", records[0].ID)
	}
	if records[1].ID != "rec-002" {
		t.Errorf("second record ID: want rec-002, got %s", records[1].ID)
	}
	if records[0].Tier != memory.TierFull {
		t.Errorf("first tier: want TierFull, got %d", records[0].Tier)
	}
}

func TestFileRecordStore_EmptySession(t *testing.T) {
	dir := t.TempDir()
	store := memory.NewFileRecordStore(dir)
	records, err := store.List("nonexistent")
	if err != nil {
		t.Fatalf("List on empty should not error: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("want 0 records, got %d", len(records))
	}
}

func TestFileRecordStore_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "deep")
	store := memory.NewFileRecordStore(dir)
	r := memory.CompactionRecord{ID: "r1", SessionID: "s1"}
	r.FillDefaults()
	if err := store.Append(r); err != nil {
		t.Fatalf("Append should create dirs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "s1.jsonl")); err != nil {
		t.Error("file should exist after Append")
	}
}
