package pr

import (
	"fmt"
	"strings"

	"github.com/google/go-github/v92/github"
)

const (
	// AppLogin is the login of the marge App's bot user.
	AppLogin = "giantswarm-marge[bot]"
	// appUserID is the numeric ID of that bot user, which its noreply
	// address carries.
	appUserID = 329428426
)

// AppEmail is the noreply address of the marge App's bot user. The
// devctl-generated renovate.json5 lists it under gitIgnoredAuthors, so a
// commit it authors does not count as a person editing Renovate's branch.
var AppEmail = fmt.Sprintf("%d+%s@users.noreply.github.com", appUserID, AppLogin)

// AppAuthor is the author of every commit marge writes onto a bot's branch,
// whatever token the sweep runs under. Renovate reads a branch whose last
// commit has any other author as edited by a person: it stops rebasing it
// and, on autoclose, retitles it "- abandoned" instead of closing it. The
// committer stays the token's own identity, so the commit still says who ran
// the sweep.
func AppAuthor() *github.CommitAuthor {
	return &github.CommitAuthor{Name: new(AppLogin), Email: new(AppEmail)}
}

// BotRebasesAfter reports whether the bot that opened a PR still rebases its
// branch when the branch's last commit has the given author. Renovate
// rebases a branch whose last commit is its own or by an ignored author,
// which marge is; a commit by anyone else stops it for good.
//
// login is the GitHub user the commit is linked to and email the address it
// carries; either one identifying the PR's author or marge is enough. An
// unlinked commit with an unknown address is a person's.
func BotRebasesAfter(prAuthor, login, email string) bool {
	switch {
	case login != "" && (strings.EqualFold(login, prAuthor) || strings.EqualFold(login, AppLogin)):
		return true
	case strings.EqualFold(email, AppEmail):
		return true
	}
	return false
}
