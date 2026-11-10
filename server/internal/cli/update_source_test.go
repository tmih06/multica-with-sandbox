package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReleaseSourceDefaultsToGitHub(t *testing.T) {
	t.Setenv(releaseAPIBaseURLEnv, "")
	t.Setenv(releaseDownloadBaseURLEnv, "")
	t.Setenv(releaseRepoEnv, "")

	if got := releaseAPIBaseURL(); got != defaultReleaseAPIBaseURL {
		t.Fatalf("release API base URL = %q, want %q", got, defaultReleaseAPIBaseURL)
	}
	if got := releaseRepo(); got != defaultReleaseRepo {
		t.Fatalf("release repo = %q, want %q", got, defaultReleaseRepo)
	}
	if got := releaseAssetDownloadURL("archive.tar.gz", "v1.2.3", "https://github.example/archive.tar.gz"); got != "https://github.example/archive.tar.gz" {
		t.Fatalf("fallback download URL = %q", got)
	}
}

func TestReleaseSourceOverridesGitHub(t *testing.T) {
	t.Setenv(releaseAPIBaseURLEnv, "https://mirror.example/api/")
	t.Setenv(releaseDownloadBaseURLEnv, "https://mirror.example/releases/")

	if got := releaseAPIBaseURL(); got != "https://mirror.example/api" {
		t.Fatalf("release API base URL = %q", got)
	}
	if got := releaseAssetDownloadURL("multica-cli-1.2.3-linux-amd64.tar.gz", "v1.2.3", "https://github.example/archive.tar.gz"); got != "https://mirror.example/releases/v1.2.3/multica-cli-1.2.3-linux-amd64.tar.gz" {
		t.Fatalf("mirror download URL = %q", got)
	}
}

// A fork publishes its own releases under its own slug; the shipped binary has
// to follow that slug instead of polling upstream, or the daemon's auto-update
// silently replaces the fork's build with an upstream one.
func TestReleaseRepoOverride(t *testing.T) {
	t.Setenv(releaseRepoEnv, " someone/multica-fork ")

	if got := releaseRepo(); got != "someone/multica-fork" {
		t.Fatalf("release repo = %q, want someone/multica-fork", got)
	}
}

func TestReleaseRepoRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{
		"",
		"   ",
		"no-slash",
		"/missing-owner",
		"missing-name/",
		"too/many/slashes",
		"has space/repo",
		"owner/re po",
	} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(releaseRepoEnv, value)
			if got := releaseRepo(); got != defaultReleaseRepo {
				t.Fatalf("release repo = %q, want fallback %q", got, defaultReleaseRepo)
			}
		})
	}
}

func TestFetchLatestReleaseUsesConfiguredAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+defaultReleaseRepo+"/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GitHubRelease{TagName: "v9.8.7"})
	}))
	defer server.Close()
	t.Setenv(releaseAPIBaseURLEnv, server.URL)
	t.Setenv(releaseRepoEnv, "")

	got, err := FetchLatestRelease()
	if err != nil {
		t.Fatalf("FetchLatestRelease() error = %v", err)
	}
	if got.TagName != "v9.8.7" {
		t.Fatalf("release tag = %q, want v9.8.7", got.TagName)
	}
}

func TestFetchLatestReleaseFollowsConfiguredRepo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/fork-owner/fork-repo/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GitHubRelease{TagName: "v0.9.0"})
	}))
	defer server.Close()
	t.Setenv(releaseAPIBaseURLEnv, server.URL)
	t.Setenv(releaseRepoEnv, "fork-owner/fork-repo")

	got, err := FetchLatestRelease()
	if err != nil {
		t.Fatalf("FetchLatestRelease() error = %v", err)
	}
	if got.TagName != "v0.9.0" {
		t.Fatalf("release tag = %q, want v0.9.0", got.TagName)
	}
}
