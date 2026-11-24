package store

import (
	"strings"
	"testing"
)

func TestAutoUpdateTargetName(t *testing.T) {
	if n := (AutoUpdateTarget{Kind: "stack", StackName: "app", ContainerName: "x"}).Name(); n != "app" {
		t.Fatalf("stack: %q", n)
	}
	if n := (AutoUpdateTarget{Kind: "container", ContainerName: "web"}).Name(); n != "web" {
		t.Fatalf("container: %q", n)
	}
}

// The ON CONFLICT clauses in autoupdates.go name partial unique indexes;
// Postgres only accepts them if the migration's indexes match exactly.
func TestAutoUpdateMigrationMatchesQueries(t *testing.T) {
	b, err := migrations.ReadFile("migrations/0013_auto_updates.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(b)
	for _, want := range []string{
		"ON auto_update_targets (stack_id) WHERE kind = 'stack'",
		"ON auto_update_targets (host_id, container_name) WHERE kind = 'container'",
		"REFERENCES stacks(id) ON DELETE CASCADE",
		"REFERENCES hosts(id) ON DELETE CASCADE",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("migration lacks %q", want)
		}
	}
}
