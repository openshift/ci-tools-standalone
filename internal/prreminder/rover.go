package prreminder

import "github.com/sirupsen/logrus"

type User struct {
	GitHubUsername string `json:"github_username,omitempty"`
	UID            string `json:"uid,omitempty"`
	CostCenter     string `json:"cost_center,omitempty"`
}

// MapGithubToKerberos projects a list of users into a dictionary that maps
// a user's GitHub username to their Kerberos ID. When two users share a
// GitHub username, the first is kept and the latter is skipped.
func MapGithubToKerberos(users []User) map[string]string {
	mapping := map[string]string{}
	for _, user := range users {
		if uid, ok := mapping[user.GitHubUsername]; ok {
			logrus.WithField("uid1", uid).WithField("uid2", user.UID).WithField("github_username", user.GitHubUsername).
				Warn("Two users with the same GitHub username: ignoring the latter")
		} else {
			mapping[user.GitHubUsername] = user.UID
		}
	}
	return mapping
}
