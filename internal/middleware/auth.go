package middleware

type Auth struct {
	allowed map[int64]bool
}

func NewAuth(users []int64) *Auth {
	m := make(map[int64]bool, len(users))
	for _, u := range users {
		m[u] = true
	}
	return &Auth{allowed: m}
}

func (a *Auth) IsAllowed(userID int64) bool {
	return a.allowed[userID]
}
