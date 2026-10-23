package user

import (
	"strconv"
	"time"
)

type User struct {
	ID          string
	Login       string
	DisplayName string
}

type Identity struct {
	UserID   string
	Provider string
	Issuer   string
	Subject  string
}

func GitHubIdentity(userID string, githubUserID int64) Identity {
	return Identity{
		UserID: userID, Provider: "github",
		Issuer: "https://github.com", Subject: strconv.FormatInt(githubUserID, 10),
	}
}

type Session struct {
	ID        string
	UserID    string
	TenantID  string
	Role      string
	Login     string
	ExpiresAt time.Time
}
