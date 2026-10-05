package autotix

import (
	"os"
	"strconv"
	"strings"
)

// DetectGitContext reads the pull request a process runs inside from the
// environment. It returns nil outside a recognized PR preview.
//
// A generic AUTOTIX_GIT_PROVIDER / AUTOTIX_GIT_PR pair (plus optional
// AUTOTIX_GIT_REF and AUTOTIX_GIT_PREVIEW) wins, so any CI can opt in.
// Tugboat previews are recognized out of the box through their per-provider
// variables (TUGBOAT_GITHUB_PR, TUGBOAT_GITLAB_MR, TUGBOAT_BITBUCKET_PR,
// TUGBOAT_STASH_PR) together with TUGBOAT_PREVIEW_REF and
// TUGBOAT_PREVIEW_NAME.
//
// New calls this once at construction. Override the result with
// WithGitContext.
func DetectGitContext() *GitContext {
	return detectGitContext(os.Getenv)
}

var tugboatVars = []struct{ provider, name string }{
	{"github", "TUGBOAT_GITHUB_PR"},
	{"gitlab", "TUGBOAT_GITLAB_MR"},
	{"bitbucket", "TUGBOAT_BITBUCKET_PR"},
	{"stash", "TUGBOAT_STASH_PR"},
}

func detectGitContext(getenv func(string) string) *GitContext {
	provider := getenv("AUTOTIX_GIT_PROVIDER")
	pr := getenv("AUTOTIX_GIT_PR")
	ref := getenv("AUTOTIX_GIT_REF")
	preview := getenv("AUTOTIX_GIT_PREVIEW")

	if provider == "" || pr == "" {
		for _, v := range tugboatVars {
			if val := getenv(v.name); val != "" {
				if provider == "" {
					provider = v.provider
				}
				if pr == "" {
					pr = val
				}
				break
			}
		}
		if ref == "" {
			ref = getenv("TUGBOAT_PREVIEW_REF")
		}
		if preview == "" {
			preview = getenv("TUGBOAT_PREVIEW_NAME")
		}
	}

	if provider == "" || pr == "" {
		return nil
	}
	n, err := strconv.Atoi(pr)
	if err != nil || n <= 0 {
		return nil
	}
	return &GitContext{
		Provider: strings.ToLower(provider),
		PR:       n,
		Ref:      ref,
		Preview:  preview,
	}
}
