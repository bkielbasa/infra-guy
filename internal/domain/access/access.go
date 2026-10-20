package access

type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

type Fact struct {
	Personal  bool
	AccountID int64
	UserID    int64
	State     string
	OrgRole   string
	Failed    bool
}

func Decide(f Fact) (Role, bool) {
	if f.Failed {
		return "", false
	}
	if f.Personal {
		if f.AccountID != 0 && f.AccountID == f.UserID {
			return RoleAdmin, true
		}
		return "", false
	}
	if f.State != "active" {
		return "", false
	}
	switch f.OrgRole {
	case "admin":
		return RoleAdmin, true
	case "member":
		return RoleMember, true
	default:
		return "", false
	}
}
