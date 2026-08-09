package verifier

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/harness9/internal/mission"
	_ "modernc.org/sqlite"
)

func newTestStoreImpl(t *testing.T) *mission.Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := mission.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
