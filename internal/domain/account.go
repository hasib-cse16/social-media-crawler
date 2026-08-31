package domain

import "time"

// UserStatus controls whether an account may sign in.
type UserStatus string

const (
	UserActive    UserStatus = "active"
	UserSuspended UserStatus = "suspended"
)

// User is an account, as everything above the storage layer sees it.
//
// It deliberately carries no password hash. Credentials are a separate type
// fetched by a separate method, so that marshalling a User — into a JSON
// response, a log line, a template — cannot leak one by accident. The type
// system enforces what a review comment otherwise would.
type User struct {
	ID          int64      `json:"-"`
	PublicID    string     `json:"id"`
	Email       string     `json:"email"`
	DisplayName string     `json:"display_name,omitempty"`
	Timezone    string     `json:"timezone"`
	Status      UserStatus `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"-"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// Active reports whether this account may sign in.
func (u *User) Active() bool { return u.Status == UserActive }

// Credentials is what the login path needs and nothing else. It never leaves
// the auth package.
type Credentials struct {
	UserID       int64
	PasswordHash string
	Status       UserStatus
}

// NewUser is the input to creating an account. The hash is computed by the auth
// package; storage stores what it is given and never sees a plaintext password.
type NewUser struct {
	Email        string
	PasswordHash string
	DisplayName  string
	Timezone     string
}

// Session is one signed-in browser or client.
//
// The opaque token is not here and is never stored: the database holds only its
// SHA-256, so a dump does not yield a set of live sessions. Callers pass the
// hash; computing it is the auth package's job.
type Session struct {
	UserID     int64     `json:"-"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	UserAgent  string    `json:"user_agent,omitempty"`
	IP         string    `json:"ip,omitempty"`
}

// NewSession is the input to issuing a session.
type NewSession struct {
	TokenHash []byte
	UserID    int64
	ExpiresAt time.Time
	UserAgent string
	IP        string
}
