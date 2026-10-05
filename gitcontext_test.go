package autotix

import (
	"errors"
	"net/http"
	"testing"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDetectGitContext(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want *GitContext
	}{
		{name: "outside a preview", env: map[string]string{}, want: nil},
		{
			name: "tugboat github pr",
			env:  map[string]string{"TUGBOAT_GITHUB_PR": "42", "TUGBOAT_PREVIEW_REF": "feat/x", "TUGBOAT_PREVIEW_NAME": "pr-42"},
			want: &GitContext{Provider: "github", PR: 42, Ref: "feat/x", Preview: "pr-42"},
		},
		{
			name: "tugboat gitlab mr",
			env:  map[string]string{"TUGBOAT_GITLAB_MR": "7"},
			want: &GitContext{Provider: "gitlab", PR: 7},
		},
		{
			name: "tugboat bitbucket pr",
			env:  map[string]string{"TUGBOAT_BITBUCKET_PR": "9"},
			want: &GitContext{Provider: "bitbucket", PR: 9},
		},
		{
			name: "generic override wins over tugboat",
			env: map[string]string{
				"AUTOTIX_GIT_PROVIDER": "GitHub", "AUTOTIX_GIT_PR": "100",
				"AUTOTIX_GIT_REF": "main", "AUTOTIX_GIT_PREVIEW": "staging",
				"TUGBOAT_GITHUB_PR": "42",
			},
			want: &GitContext{Provider: "github", PR: 100, Ref: "main", Preview: "staging"},
		},
		{
			name: "generic provider without pr falls back to tugboat pr",
			env:  map[string]string{"AUTOTIX_GIT_PROVIDER": "github", "TUGBOAT_GITHUB_PR": "5"},
			want: &GitContext{Provider: "github", PR: 5},
		},
		{name: "non numeric pr is ignored", env: map[string]string{"TUGBOAT_GITHUB_PR": "abc"}, want: nil},
		{name: "empty pr is ignored", env: map[string]string{"TUGBOAT_GITHUB_PR": ""}, want: nil},
		{name: "zero pr is ignored", env: map[string]string{"TUGBOAT_GITHUB_PR": "0"}, want: nil},
		{name: "provider without pr is ignored", env: map[string]string{"AUTOTIX_GIT_PROVIDER": "github"}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectGitContext(envFrom(tc.env))
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if got != nil && *got != *tc.want {
				t.Fatalf("got %+v, want %+v", *got, *tc.want)
			}
		})
	}
}

func TestDetectGitContextReadsProcessEnv(t *testing.T) {
	t.Setenv("AUTOTIX_GIT_PROVIDER", "github")
	t.Setenv("AUTOTIX_GIT_PR", "12")
	got := DetectGitContext()
	if got == nil || got.Provider != "github" || got.PR != 12 {
		t.Fatalf("DetectGitContext = %+v", got)
	}
}

func TestCaptureAttachesDetectedGitContext(t *testing.T) {
	t.Setenv("TUGBOAT_GITHUB_PR", "42")
	t.Setenv("TUGBOAT_PREVIEW_NAME", "pr-42")
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv)
	c.Capture(errors.New("in a preview"))
	flush(t, c)
	git := rec.wait(t, 1)[0].Wire.Details.Git
	if git == nil || git.Provider != "github" || git.PR != 42 || git.Preview != "pr-42" {
		t.Fatalf("details.git = %+v", git)
	}
}

func TestWithGitContextOverridesDetection(t *testing.T) {
	t.Setenv("TUGBOAT_GITHUB_PR", "42")
	srv, rec := newServer(t, http.StatusOK)
	c := newClient(t, srv, WithGitContext(GitContext{Provider: "github", PR: 99, Ref: "release"}))
	c.Capture(errors.New("explicit"))
	flush(t, c)
	git := rec.wait(t, 1)[0].Wire.Details.Git
	if git == nil || git.PR != 99 || git.Ref != "release" {
		t.Fatalf("details.git = %+v", git)
	}
}
