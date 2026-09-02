package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/foodibd/socialstats/internal/auth"
	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/httpx"
)

// Handlers.
//
// Every state-changing handler follows post-redirect-get: it does the work,
// queues a flash message and redirects. Rendering the result of a POST directly
// would leave the browser on a URL that re-submits on refresh and behaves badly
// under the back button — which for a lookup means spending an API call every
// time somebody hits reload.

// Dashboard renders the lookup form and this user's history.
func (s *Server) Dashboard(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		s.redirectToLogin(w, r)
		return
	}

	history, err := s.lookups.History(r.Context(), user.ID)
	if err != nil {
		s.renderError(w, r, err)
		return
	}

	rows := make([]LookupRow, 0, len(history))
	for _, l := range history {
		rows = append(rows, toRow(l))
	}

	s.render(w, r, http.StatusOK, "dashboard.html", Page{
		Title: "Dashboard",
		Nav:   "dashboard",
		Data: DashboardView{
			Recent:    rows,
			Platforms: s.lookups.Platforms(),
			Empty:     len(rows) == 0,
		},
	})
}

// Lookup handles the paste-a-URL form.
//
// The fetch happens here, synchronously, because the whole point of the page is
// that a pasted link produces a number now. The service bounds it with a
// timeout so a slow platform cannot hold the request open.
func (s *Server) Lookup(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		s.redirectToLogin(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, httpx.Detail(domain.ErrInvalidURL, "that form could not be read"))
		return
	}

	rawURL := strings.TrimSpace(r.PostFormValue("url"))
	if rawURL == "" {
		s.setFlash(w, "error", "Paste a video link first.")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	result, err := s.lookups.Lookup(r.Context(), user.ID, rawURL)
	if err != nil {
		s.log.WarnContext(r.Context(), "lookup failed", "url", rawURL, "error", err)
		s.setFlash(w, "error", lookupFailureMessage(err))
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// Straight to the result rather than back to the list: the user asked a
	// question and the answer is the page they want to be on.
	http.Redirect(w, r, "/lookups/"+result.PublicID, http.StatusSeeOther)
}

// LookupDetail renders one past lookup.
func (s *Server) LookupDetail(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		s.redirectToLogin(w, r)
		return
	}

	record, err := s.lookups.Get(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		s.renderError(w, r, err)
		return
	}

	view := toLookupView(*record)
	s.render(w, r, http.StatusOK, "lookup.html", Page{
		Title: view.Row.Title,
		Nav:   "dashboard",
		Data:  view,
	})
}

// RemoveLookup deletes one entry from the history.
func (s *Server) RemoveLookup(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		s.redirectToLogin(w, r)
		return
	}

	if err := s.lookups.Remove(r.Context(), user.ID, r.PathValue("id")); err != nil {
		if !errors.Is(err, domain.ErrRecordNotFound) {
			s.renderError(w, r, err)
			return
		}
	}

	s.setFlash(w, "success", "Removed from your history.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------- accounts ----------

// LoginForm renders the sign-in page.
func (s *Server) LoginForm(w http.ResponseWriter, r *http.Request) {
	if auth.UserFrom(r.Context()) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	s.render(w, r, http.StatusOK, "login.html", Page{
		Title: "Sign in",
		Data: map[string]any{
			"Email":            "",
			"Next":             safeNext(r.URL.Query().Get("next")),
			"RegistrationOpen": s.registrationOpen,
		},
	})
}

// Login handles the sign-in form.
func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, httpx.Detail(domain.ErrInvalidURL, "that form could not be read"))
		return
	}

	email := r.PostFormValue("email")
	next := safeNext(r.PostFormValue("next"))

	issued, err := s.auth.Login(r.Context(), auth.LoginInput{
		Email:     email,
		Password:  r.PostFormValue("password"),
		UserAgent: r.UserAgent(),
		IP:        auth.ClientIP(r, s.trustProxy),
	})
	if err != nil {
		// Re-rendered rather than redirected, so the typed email survives and
		// the person does not have to enter it again to find out what went
		// wrong.
		s.render(w, r, statusFor(err), "login.html", Page{
			Title: "Sign in",
			Flash: &Flash{Level: "error", Message: signInFailureMessage(err)},
			Data: map[string]any{
				"Email":            email,
				"Next":             next,
				"RegistrationOpen": s.registrationOpen,
			},
		})
		return
	}

	s.startSession(w, issued)
	if next == "" {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// RegisterForm renders the sign-up page.
func (s *Server) RegisterForm(w http.ResponseWriter, r *http.Request) {
	if auth.UserFrom(r.Context()) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.registrationOpen {
		s.renderError(w, r, domain.ErrRegistrationClosed)
		return
	}

	s.render(w, r, http.StatusOK, "register.html", Page{
		Title: "Create an account",
		Data:  map[string]any{"Email": "", "DisplayName": ""},
	})
}

// Register handles the sign-up form.
func (s *Server) Register(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, httpx.Detail(domain.ErrInvalidURL, "that form could not be read"))
		return
	}

	email := r.PostFormValue("email")
	displayName := r.PostFormValue("display_name")

	issued, err := s.auth.Register(r.Context(), auth.RegisterInput{
		Email:       email,
		Password:    r.PostFormValue("password"),
		DisplayName: displayName,
		UserAgent:   r.UserAgent(),
		IP:          auth.ClientIP(r, s.trustProxy),
	})
	if err != nil {
		s.render(w, r, statusFor(err), "register.html", Page{
			Title: "Create an account",
			Flash: &Flash{Level: "error", Message: registerFailureMessage(err)},
			Data:  map[string]any{"Email": email, "DisplayName": displayName},
		})
		return
	}

	s.startSession(w, issued)
	s.setFlash(w, "success", "Welcome. Paste a video link to start tracking it.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Logout ends this session.
func (s *Server) Logout(w http.ResponseWriter, r *http.Request) {
	if token, _ := auth.TokenFromRequest(r, s.auth.Cookies().Name); token != "" {
		if err := s.auth.Logout(r.Context(), token); err != nil {
			s.log.WarnContext(r.Context(), "logout failed", "error", err)
		}
	}
	s.auth.Cookies().ClearSession(w)
	s.auth.Cookies().ClearCSRF(w)

	s.setFlash(w, "info", "Signed out.")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// LogoutEverywhere revokes every session for the account.
func (s *Server) LogoutEverywhere(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		s.redirectToLogin(w, r)
		return
	}

	revoked, err := s.auth.LogoutEverywhere(r.Context(), user.ID)
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	s.auth.Cookies().ClearSession(w)
	s.auth.Cookies().ClearCSRF(w)

	s.setFlash(w, "info", "Signed out of "+pluralise(int(revoked), "session")+".")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// Settings renders the account page.
func (s *Server) Settings(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		s.redirectToLogin(w, r)
		return
	}

	history, err := s.lookups.History(r.Context(), user.ID)
	if err != nil {
		s.renderError(w, r, err)
		return
	}

	s.render(w, r, http.StatusOK, "settings.html", Page{
		Title: "Settings",
		Nav:   "settings",
		Data: map[string]any{
			"Timezones": timezoneOptions(user.Timezone),
			"Lookups":   len(history),
			"Joined":    user.CreatedAt.Format("2 January 2006"),
			"JoinedAt":  &user.CreatedAt,
		},
	})
}

// SaveProfile handles the profile form.
func (s *Server) SaveProfile(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		s.redirectToLogin(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, httpx.Detail(domain.ErrInvalidURL, "that form could not be read"))
		return
	}

	if _, err := s.auth.UpdateProfile(r.Context(), user.ID,
		r.PostFormValue("display_name"), r.PostFormValue("timezone")); err != nil {
		s.setFlash(w, "error", firstLine(err.Error()))
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	s.setFlash(w, "success", "Profile saved.")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// ChangePassword handles the password form.
func (s *Server) ChangePassword(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		s.redirectToLogin(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, httpx.Detail(domain.ErrInvalidURL, "that form could not be read"))
		return
	}

	err := s.auth.ChangePassword(r.Context(), user,
		r.PostFormValue("current_password"), r.PostFormValue("new_password"))
	if err != nil {
		s.setFlash(w, "error", passwordFailureMessage(err))
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	// Every session was revoked, including this one, so the cookies are cleared
	// here too — leaving them would show a signed-in shell that 401s on click.
	s.auth.Cookies().ClearSession(w)
	s.auth.Cookies().ClearCSRF(w)

	s.setFlash(w, "success", "Password changed. Sign in again with the new one.")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// startSession writes the cookies for a newly issued session.
func (s *Server) startSession(w http.ResponseWriter, issued *auth.Issued) {
	cookies := s.auth.Cookies()
	cookies.WriteSession(w, issued.Token, issued.ExpiresAt)
	cookies.WriteCSRF(w, issued.CSRFToken, issued.ExpiresAt)
}

// redirectToLogin sends an anonymous visitor to sign in, remembering where they
// were going so they land there rather than on a generic dashboard.
func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.RequestURI()
	target := "/login"
	if safeNext(next) != "" && next != "/" {
		target += "?next=" + url.QueryEscape(next)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// safeNext accepts only same-site paths.
//
// An open redirect is a phishing primitive: without this check, a link to
// /login?next=https://evil.example would take somebody through a real sign-in
// and out to an attacker's page, with our domain in the referrer.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") {
		return ""
	}
	// "//host" and "/\host" are protocol-relative and leave the site.
	if strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return ""
	}
	return next
}
