package wire

import (
	"errors"
	"strings"
)

// ---------------- stack rollback ----------------
//
// Before every deploy, Pull & redeploy, automatic update and rollback the
// hub records which image each of the stack's services runs (OpStackImages).
// The agent keeps those images from being pruned by tagging them under a
// reserved repository, and a rollback (StackRollback) brings them back with
// an extra compose file that pins each service to its old image.

const (
	// RollbackRepoPrefix starts every tag TainerHub keeps for rollbacks.
	// TainerHub's own prune and remove paths leave these images alone.
	RollbackRepoPrefix = "tainerhub-rollback/"
	// StackRollbackFile is the compose override a rollback writes in the
	// stack's folder. While it's there the stack is "pinned": every compose
	// command but a deploy uses it.
	StackRollbackFile = ".tainerhub-rollback.yaml"
	// StackImagesProtect is OpStackImages's Arg for also tagging the images
	// it finds (so they survive pruning) and dropping older tags.
	StackImagesProtect = "protect"
	// RollbackUnchanged ends a rollback's error when it failed before
	// changing anything on the host.
	RollbackUnchanged = "nothing was changed"
	// MaxRollbackServices caps how many services one rollback names.
	MaxRollbackServices = 100
)

// StackImage is what one service of a stack runs.
type StackImage struct {
	Ref     string `json:"ref"`               // the image reference as written (nginx:1.27)
	ImageID string `json:"imageId"`           // sha256:…
	Digest  string `json:"digest,omitempty"`  // repo digest (nginx@sha256:…), if it came from a registry
	Created string `json:"created,omitempty"` // when the image was built (RFC 3339)
}

// StackImages is OpStackImages's result: service name -> image. Empty when
// the stack has no containers on the host.
type StackImages struct {
	Services map[string]StackImage `json:"services"`
}

// RollbackService is one service a rollback puts back on an older image.
type RollbackService struct {
	Service string `json:"service"`
	ImageID string `json:"imageId"`
	Ref     string `json:"ref,omitempty"` // for messages only
}

// ValidServiceName accepts compose service names (letters, digits, '.',
// '_', '-'), short enough to be an image tag.
func ValidServiceName(s string) bool {
	if s == "" || len(s) > 120 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// RollbackTag is the tag that keeps a service's newest rollback image:
// tainerhub-rollback/<stack>:<service>. A tag can't start with '.' or '-',
// so such service names get a leading '_'.
func RollbackTag(stack, service string) string {
	return RollbackRepo(stack) + ":" + tagPart(service)
}

// RollbackRepo is the repository holding a stack's rollback images.
func RollbackRepo(stack string) string { return RollbackRepoPrefix + stack }

// PinnedRepo holds the images a rolled-back stack runs now. It is separate
// from RollbackRepo because recording the next rollback point retags that
// one, and a pinned stack must not silently switch images.
func PinnedRepo(stack string) string { return RollbackRepoPrefix + stack + "/pinned" }

// PinnedTag is the tag the rollback override file uses for a service.
func PinnedTag(stack, service string) string { return PinnedRepo(stack) + ":" + tagPart(service) }

func tagPart(service string) string {
	if strings.HasPrefix(service, ".") || strings.HasPrefix(service, "-") {
		return "_" + service
	}
	return service
}

// IsRollbackTag says whether an image tag is one TainerHub keeps for a rollback.
func IsRollbackTag(tag string) bool { return strings.HasPrefix(tag, RollbackRepoPrefix) }

// RollbackStackOf is the stack a rollback tag belongs to ("" if it isn't one).
func RollbackStackOf(tag string) string {
	if !IsRollbackTag(tag) {
		return ""
	}
	rest := strings.TrimPrefix(tag, RollbackRepoPrefix)
	if i := strings.IndexAny(rest, "/:"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// ValidRollback checks a rollback command's services.
func ValidRollback(svcs []RollbackService) error {
	if len(svcs) == 0 {
		return errors.New("the rollback names no services")
	}
	if len(svcs) > MaxRollbackServices {
		return errors.New("the rollback names too many services")
	}
	seen := map[string]bool{}
	for _, s := range svcs {
		if !ValidServiceName(s.Service) {
			return errors.New("invalid service name in the rollback")
		}
		if seen[s.Service] {
			return errors.New("a service is listed twice in the rollback")
		}
		seen[s.Service] = true
		if !strings.HasPrefix(s.ImageID, "sha256:") || !ValidImageID(s.ImageID) {
			return errors.New("invalid image ID in the rollback")
		}
		if s.Ref != "" && !ValidImageRef(s.Ref) {
			return errors.New("invalid image reference in the rollback")
		}
	}
	return nil
}
