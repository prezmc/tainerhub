package store

import (
	"strings"
	"testing"
)

// The rollback points go with their stack, and automatic rollback is on
// unless someone turns it off.
func TestRollbackMigration(t *testing.T) {
	b, err := migrations.ReadFile("migrations/0014_stack_rollback.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(b)
	for _, want := range []string{
		"stack_id     uuid NOT NULL REFERENCES stacks(id) ON DELETE CASCADE",
		"CHECK (trigger IN ('deploy', 'pull', 'auto', 'rollback'))",
		"ADD COLUMN rolled_back_to timestamptz",
		"ADD COLUMN auto_rollback boolean NOT NULL DEFAULT true",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("migration lacks %q", want)
		}
	}
	if RollbackKeep != 5 {
		t.Errorf("keeps %d points, want 5", RollbackKeep)
	}
	if !strings.Contains(stackCols, "rolled_back_to") || !strings.Contains(autoUpdateCols, "auto_rollback") {
		t.Error("queries don't read the new columns")
	}
}
