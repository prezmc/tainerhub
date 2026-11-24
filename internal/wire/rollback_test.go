package wire

import (
	"strings"
	"testing"
)

func TestRollbackTags(t *testing.T) {
	if got := RollbackTag("app", "web"); got != "tainerhub-rollback/app:web" {
		t.Fatal(got)
	}
	if got := PinnedTag("app", "-x"); got != "tainerhub-rollback/app/pinned:_-x" {
		t.Fatal(got)
	}
	for tag, stack := range map[string]string{
		"tainerhub-rollback/app:web":        "app",
		"tainerhub-rollback/app/pinned:web": "app",
		"nginx:1":                           "",
		"ghcr.io/tainerhub-rollback/x:1":    "",
	} {
		if got := RollbackStackOf(tag); got != stack {
			t.Errorf("%s: %q, want %q", tag, got, stack)
		}
	}
}

func TestValidRollbackCommand(t *testing.T) {
	id := "sha256:" + strings.Repeat("a", 64)
	ok := StackCommand{Action: StackRollback, Name: "app", Rollback: []RollbackService{{Service: "web", ImageID: id, Ref: "nginx:1"}}}
	if err := ValidStackCommand(ok); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]StackCommand{
		"no services":  {Action: StackRollback, Name: "app"},
		"twice":        {Action: StackRollback, Name: "app", Rollback: []RollbackService{{Service: "web", ImageID: id}, {Service: "web", ImageID: id}}},
		"bare id":      {Action: StackRollback, Name: "app", Rollback: []RollbackService{{Service: "web", ImageID: strings.Repeat("a", 64)}}},
		"yaml in name": {Action: StackRollback, Name: "app", Rollback: []RollbackService{{Service: "web:\n  x", ImageID: id}}},
		"on a deploy":  {Action: StackDeploy, Name: "app", Compose: "services: {}", Rollback: ok.Rollback},
		"with a login": {Action: StackRollback, Name: "app", Rollback: ok.Rollback, Auth: []RegistryAuth{{Registry: "ghcr.io", Username: "u", Password: "p"}}},
		"with files":   {Action: StackRollback, Name: "app", Rollback: ok.Rollback, Files: []StackFile{{Path: "a", Content: []byte("x")}}},
		"bad ref":      {Action: StackRollback, Name: "app", Rollback: []RollbackService{{Service: "web", ImageID: id, Ref: "../x"}}},
	} {
		if ValidStackCommand(c) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if ValidOperation(Operation{Kind: OpStackImages, Target: "app", Arg: StackImagesProtect}) != nil ||
		ValidOperation(Operation{Kind: OpStackImages, Target: "../x"}) == nil ||
		ValidOperation(Operation{Kind: OpStackImages, Target: "app", Arg: "all"}) == nil {
		t.Error("OpStackImages validation")
	}
	if ValidOperation(Operation{Kind: OpImageRemove, Target: strings.Repeat("a", 64), Arg: "yes"}) == nil {
		t.Error("image remove accepted an unknown mode")
	}
}
