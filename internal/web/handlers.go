package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/foodibd/socialstats/internal/auth"
	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/httpx"
	"github.com/foodibd/socialstats/internal/storage/postgres"
	"github.com/foodibd/socialstats/internal/tracking"
)

// Handlers.
//
// Every state-changing handler follows post-redirect-get: it does the work,
// queues a flash message and redirects. Rendering the result of a POST directly
// would leave the browser on a URL that re-submits on refresh and behaves badly
// under the back button — which for "stop tracking" is a genuinely annoying way
// to lose something.

// Dashboard renders the tracked video list.
func (s *Server) Dashboard(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		s.redirectToLogin(w, r)
		return
	}

	q := r.URL.Query()
	sort := firstOf(q.Get("sort"), "views", validSorts)
	window := firstOf(q.Get("window"), "168h", validWindows)
	platform := q.Get("platform")
	if !validPlatform(platform) {
		platform = ""
	}

	windowDuration, err := time.ParseDuration(window)
	if err != nil {
		windowDuration = 7 * 24 * time.Hour
	}

	entries, err := s.tracking.List(r.Context(), tracking.ListQuery{
		UserID:   user.ID,
		Window:   windowDuration,
		Platform: domain.Platform(platform),
		Sort:     postgres.DashboardSort(sort),
		Limit:    200,
		// The sparkline is the column that makes a list of numbers legible at a
		// glance, so the dashboard pays for it here even though the JSON API
		// leaves it off by default.
		Sparkline: 24,
	})
	if err != nil {
		s.renderError(w, r, err)
		return
	}

	summary, err := s.tracking.Summarise(r.Context(), user.ID, windowDuration, 0)
	if err != nil {
		s.renderError(w, r, err)
		return
	}

	rows := make([]VideoRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, toRow(e))
	}

	filters := buildFilters(sort, platform, window)
	filters.PlatformOptions = platformOptions(s.platformsFor(), platform)

	s.render(w, r, http.StatusOK, "dashboard.html", Page{
		Title: "Dashboard",
		Nav:   "dashboard",
		Data: DashboardView{
			Videos:  rows,
			Summary: toSummary(summary),
			Filters: filters,
			// "Nothing tracked yet" and "nothing matches this filter" need
			// different words, so the two cases are distinguished here rather
			// than both rendering as an empty table.
			Empty: summary.TrackedVideos == 0,
		},
	})
}

// Video renders one video's detail page.
func (s *Server) Video(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		s.redirectToLogin(w, r)
		return
	}

	id := r.PathValue("id")
	rangeWindow := firstOf(r.URL.Query().Get("range"), "168h", validWindows)
	windowDuration, err := time.ParseDuration(rangeWindow)
	if err != nil {
		windowDuration = 7 * 24 * time.Hour
	}

	entry, err := s.tracking.Get(r.Context(), user.ID, id)
	if err != nil {
		s.renderError(w, r, err)
		return
	}
	tracked, err := s.tracking.Tracked(r.Context(), user.ID, id)
	if err != nil {
		s.renderError(w, r, err)
		return
	}

	now := time.Now()
	history, err := s.tracking.HistoryFor(r.Context(), tracking.HistoryQuery{
		UserID: user.ID, PublicID: id,
		From: now.Add(-windowDuration), To: now,
		// Hourly, not raw: a 90-day range at six-hourly readings is 360 points
		// through a 720-pixel chart, and the extra detail is noise the reader
		// cannot see anyway.
		Bucket: bucketFor(windowDuration),
	})
	if err != nil {
		s.renderError(w, r, err)
		return
	}

	attempts, err := s.tracking.Attempts(r.Context(), user.ID, id, 15)
	if err != nil {
		s.renderError(w, r, err)
		return
	}

	// The growth figure on this page has to be measured over the range being
	// charted, or the headline and the chart disagree about the same video.
	scoped, err := s.tracking.List(r.Context(), tracking.ListQuery{
		UserID: user.ID, Window: windowDuration, Limit: 200,
	})
	if err != nil {
		s.renderError(w, r, err)
		return
	}

	row := toRow(*entry)
	for _, e := range scoped {
		if e.Video.PublicID == id {
			row = toRow(e)
			break
		}
	}

	view := VideoView{
		Row:      row,
		Notes:    tracked.Notes,
		Chart:    Chart(history.Snapshots),
		Range:    rangeWindow,
		Ranges:   rangeOptions(rangeWindow),
		Points:   len(history.Snapshots) + len(history.Daily),
		Source:   history.Source,
		Attempts: toAttempts(attempts),
		Schedule: toSchedule(entry.Video),
	}

	s.render(w, r, http.StatusOK, "video.html", Page{
		Title: row.Title,
		Nav:   "dashboard",
		Data:  view,
	})
}

// AddVideo handles the dashboard's track form.
func (s *Server) AddVideo(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		s.redirectToLogin(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.setFlash(w, "error", "That form could not be read.")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	entry, err := s.tracking.Add(r.Context(), user.ID,
		r.PostFormValue("url"), r.PostFormValue("label"))
	if err != nil {
		s.setFlash(w, "error", addFailureMessage(err))
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// The first fetch may have failed while the tracking succeeded, and saying
	// so plainly is better than a success message next to an empty figure.
	if entry.Video.Latest.ViewCount == nil && entry.Video.Schedule.LastFetchStatus != domain.FetchOK {
		s.setFlash(w, "info",
			"Tracking started, but "+platformName(entry.Video.Platform)+
				" did not return figures on the first try. It will be retried automatically.")
	} else {
		s.setFlash(w, "success", "Now tracking "+videoTitle(entry.Video, entry.Label)+".")
	}
	http.Redirect(w, r, "/videos/"+entry.Video.PublicID, http.StatusSeeOther)
}

// UpdateVideo saves the per-user label and notes.
func (s *Server) UpdateVideo(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		s.redirectToLogin(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, httpx.Detail(domain.ErrInvalidURL, "that form could not be read"))
		return
	}

	id := r.PathValue("id")
	if _, err := s.tracking.Update(r.Context(), user.ID, id,
		r.PostFormValue("label"), r.PostFormValue("notes")); err != nil {
		s.renderError(w, r, err)
		return
	}

	s.setFlash(w, "success", "Saved.")
	http.Redirect(w, r, "/videos/"+id, http.StatusSeeOther)
}

// RemoveVideo untracks a video.
//
// A form post rather than DELETE, because HTML forms only speak GET and POST.
// Faking the method with a hidden _method field is a convention borrowed from
// frameworks that need it; here the route can simply say what it does.
func (s *Server) RemoveVideo(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		s.redirectToLogin(w, r)
		return
	}

	if err := s.tracking.Remove(r.Context(), user.ID, r.PathValue("id")); err != nil {
		s.renderError(w, r, err)
		return
	}

	s.setFlash(w, "success", "Stopped tracking. The history has been kept, so adding it back restores it.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// RefreshVideo brings the next fetch forward.
func (s *Server) RefreshVideo(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user == nil {
		s.redirectToLogin(w, r)
		return
	}

	id := r.PathValue("id")
	if _, err := s.tracking.Refresh(r.Context(), user.ID, id); err != nil {
		if errors.Is(err, domain.ErrGone) {
			s.setFlash(w, "error", "That video has been removed by its platform, so it is no longer fetched.")
			http.Redirect(w, r, "/videos/"+id, http.StatusSeeOther)
			return
		}
		s.renderError(w, r, err)
		return
	}

	// Queued, not fetched — and the copy says so, because a "Refreshed" message
	// followed by an unchanged number reads as a broken button.
	s.setFlash(w, "info", "Queued for the next refresh cycle.")
	http.Redirect(w, r, "/videos/"+id, http.StatusSeeOther)
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

	tracked, err := s.tracking.List(r.Context(), tracking.ListQuery{UserID: user.ID, Limit: 200})
	if err != nil {
		s.renderError(w, r, err)
		return
	}

	s.render(w, r, http.StatusOK, "settings.html", Page{
		Title: "Settings",
		Nav:   "settings",
		Data: map[string]any{
			"Timezones": timezoneOptions(user.Timezone),
			"Tracked":   len(tracked),
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
