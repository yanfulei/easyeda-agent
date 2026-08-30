package distribution

import "testing"

func TestReleaseRepo(t *testing.T) {
	t.Setenv(ReleaseRepoEnv, "other-owner/other-repo")
	if got := ReleaseRepo(); got != "other-owner/other-repo" {
		t.Fatalf("ReleaseRepo()=%q", got)
	}

	t.Setenv(ReleaseRepoEnv, "https://example.invalid/not-a-slug")
	if got := ReleaseRepo(); got != DefaultReleaseRepo {
		t.Fatalf("invalid override should fall back to %q, got %q", DefaultReleaseRepo, got)
	}
}

func TestValidReleaseRepo(t *testing.T) {
	for _, repo := range []string{"owner/repo", "owner-name/repo.name", "owner_1/repo_2"} {
		if !ValidReleaseRepo(repo) {
			t.Errorf("ValidReleaseRepo(%q)=false", repo)
		}
	}
	for _, repo := range []string{"", "owner", "/repo", "owner/", "a/b/c", "owner/repo?x=1"} {
		if ValidReleaseRepo(repo) {
			t.Errorf("ValidReleaseRepo(%q)=true", repo)
		}
	}
}
