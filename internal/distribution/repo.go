package distribution

import (
	"os"
	"strings"
)

const (
	DefaultReleaseRepo = "yanfulei/easyeda-agent"
	ReleaseRepoEnv     = "EASYEDA_RELEASE_REPO"
)

// ReleaseRepo returns the GitHub owner/repo used for release assets. A fork can
// keep its updater on its own release channel without changing the Go module
// path or upstream attribution.
func ReleaseRepo() string {
	repo := strings.TrimSpace(os.Getenv(ReleaseRepoEnv))
	if ValidReleaseRepo(repo) {
		return repo
	}
	return DefaultReleaseRepo
}

// ValidReleaseRepo accepts the GitHub owner/repo subset used in release URLs.
func ValidReleaseRepo(repo string) bool {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	for _, part := range parts {
		for _, r := range part {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
				continue
			}
			return false
		}
	}
	return true
}
