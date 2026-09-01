package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/foodibd/socialstats/internal/auth"
	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/httpx"
)

// AuthHandler serves the account endpoints.
type AuthHandler struct {
	svc *auth.Service
	mw  *auth.Middleware
}

func NewAuthHandler(svc *auth.Service, mw *auth.Middleware) *AuthHandler {
	return &AuthHandler{svc: svc, mw: mw}
}

// credentialsRequest is the body of register and login.
type credentialsRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name,omitempty"`
	Timezone    string `json:"timezone,omitempty"`
}

// Register handles POST /v1/auth/register.
func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if !decodeCredentials(w, r, &req) {
		return
	}

	issued, err := h.svc.Register(r.Context(), auth.RegisterInput{
		Email:       req.Email,
		Password:    req.Password,
		DisplayName: req.DisplayName,
		Timezone:    req.Timezone,
		UserAgent:   r.UserAgent(),
		IP:          h.mw.ClientIP(r),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}

	h.respondSignedIn(w, r, issued, http.StatusCreated)
}

// Login handles POST /v1/auth/login.
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if !decodeCredentials(w, r, &req) {
		return
	}

	issued, err := h.svc.Login(r.Context(), auth.LoginInput{
		Email:     req.Email,
		Password:  req.Password,
		UserAgent: r.UserAgent(),
		IP:        h.mw.ClientIP(r),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}

	h.respondSignedIn(w, r, issued, http.StatusOK)
}

// Logout handles POST /v1/auth/logout.
//
// It succeeds whether or not there was a session to end. Signing out is an
// intention, and reporting "you were not signed in" as an error gives the
// caller nothing to do with the information.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	token, _ := auth.TokenFromRequest(r, h.svc.Cookies().Name)
	if token != "" {
		if err := h.svc.Logout(r.Context(), token); err != nil {
			h.fail(w, r, err)
			return
		}
	}

	h.svc.Cookies().ClearSession(w)
	h.svc.Cookies().ClearCSRF(w)
	httpx.Data(w, r, http.StatusOK, map[string]any{"signed_out": true})
}

// LogoutEverywhere handles POST /v1/auth/logout-all.
func (h *AuthHandler) LogoutEverywhere(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		h.fail(w, r, domain.ErrUnauthenticated)
		return
	}

	revoked, err := h.svc.LogoutEverywhere(r.Context(), user.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	h.svc.Cookies().ClearSession(w)
	h.svc.Cookies().ClearCSRF(w)
	httpx.Data(w, r, http.StatusOK, map[string]any{"signed_out": true, "sessions_revoked": revoked})
}

// Me handles GET /v1/auth/me.
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		h.fail(w, r, domain.ErrUnauthenticated)
		return
	}
	httpx.Data(w, r, http.StatusOK, user)
}

// changePasswordRequest is the body of the password change endpoint.
type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ChangePassword handles POST /v1/auth/password.
//
// It signs every session out, including this one: the usual reason to change a
// password is believing someone else has it, and leaving their session alive
// would make the change ceremonial.
func (h *AuthHandler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		h.fail(w, r, domain.ErrUnauthenticated)
		return
	}

	var req changePasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Error(w, r, http.StatusBadRequest, "invalid_body",
			"body must be a json object with 'current_password' and 'new_password'")
		return
	}

	if err := h.svc.ChangePassword(r.Context(), user, req.CurrentPassword, req.NewPassword); err != nil {
		h.fail(w, r, err)
		return
	}

	h.svc.Cookies().ClearSession(w)
	h.svc.Cookies().ClearCSRF(w)
	httpx.Data(w, r, http.StatusOK, map[string]any{
		"password_changed": true,
		"signed_out":       true,
	})
}

// respondSignedIn writes the session cookies and the user.
//
// The token is echoed in the body as well as set as a cookie, so a script can
// hold onto it and send it as a bearer token. That is what makes the API usable
// from curl without juggling a cookie jar — and a bearer request is exempt from
// CSRF precisely because a browser will not attach that header for a third
// party.
func (h *AuthHandler) respondSignedIn(w http.ResponseWriter, r *http.Request, issued *auth.Issued, status int) {
	cookies := h.svc.Cookies()
	cookies.WriteSession(w, issued.Token, issued.ExpiresAt)
	cookies.WriteCSRF(w, issued.CSRFToken, issued.ExpiresAt)

	httpx.Data(w, r, status, map[string]any{
		"user":       issued.User,
		"token":      string(issued.Token),
		"csrf_token": issued.CSRFToken,
		"expires_at": issued.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// fail renders an auth error, adding Retry-After when the failure carries one.
func (h *AuthHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var tooMany *auth.TooManyAttemptsError
	if errors.As(err, &tooMany) {
		seconds := int(tooMany.RetryAfter.Round(time.Second).Seconds())
		if seconds < 1 {
			seconds = 1
		}
		// A truthful Retry-After, computed from the bucket's actual refill
		// rate, so a well-behaved client waits exactly as long as it must.
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
	}

	status, code, message := httpErrorFor(err)
	httpx.Error(w, r, status, code, message)
}

// decodeCredentials reads a register or login body, accepting either JSON or a
// form post so the same endpoints serve the API and the browser forms.
func decodeCredentials(w http.ResponseWriter, r *http.Request, out *credentialsRequest) bool {
	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") ||
		strings.HasPrefix(contentType, "multipart/form-data") {

		if err := r.ParseForm(); err != nil {
			httpx.Error(w, r, http.StatusBadRequest, "invalid_body", "could not read the submitted form")
			return false
		}
		out.Email = r.PostFormValue("email")
		out.Password = r.PostFormValue("password")
		out.DisplayName = r.PostFormValue("display_name")
		out.Timezone = r.PostFormValue("timezone")
		return true
	}

	if err := decodeJSON(r, out); err != nil {
		httpx.Error(w, r, http.StatusBadRequest, "invalid_body",
			"body must be a json object with 'email' and 'password'")
		return false
	}
	return true
}

// decodeJSON reads a bounded JSON body, rejecting unknown fields so a typo in a
// field name fails loudly instead of being silently ignored.
func decodeJSON(r *http.Request, out any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}
