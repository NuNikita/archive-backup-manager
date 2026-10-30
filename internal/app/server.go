package app

import (
	"crypto/subtle"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed web/*
var web embed.FS

type Server struct {
	store     *Store
	cfg       Config
	templates *template.Template
}
type Page struct {
	User                    User
	CSRF, Error, Message    string
	Plans                   []Plan
	Tasks                   []Task
	Snapshots               []Snapshot
	Audits                  []Audit
	Sources                 []string
	Edit                    *Plan
	Success, Failed, Active int
}

func formatTime(t time.Time) string { return t.UTC().Format("02.01.2006 15:04 UTC") }
func NewServer(s *Store, cfg Config) http.Handler {
	funcs := template.FuncMap{"when": formatTime, "short": func(s string) string {
		if len(s) > 12 {
			return s[:12]
		}
		return s
	}, "status": func(s string) string {
		switch s {
		case "queued":
			return "В очереди"
		case "running":
			return "Выполняется"
		case "success":
			return "Успешно"
		default:
			return "Ошибка"
		}
	}, "kind": func(s string) string {
		switch s {
		case "backup":
			return "Копирование"
		case "restore":
			return "Восстановление"
		default:
			return "Проверка"
		}
	}}
	server := &Server{store: s, cfg: cfg, templates: template.Must(template.New("").Funcs(funcs).ParseFS(web, "web/*.html"))}
	mux := http.NewServeMux()
	assets, _ := fs.Sub(web, "web")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Pool.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /login", server.loginPage)
	mux.HandleFunc("POST /login", server.login)
	mux.HandleFunc("GET /{$}", server.dashboard)
	for _, path := range []string{"/logout", "/plans/save", "/plans/delete", "/tasks/backup", "/tasks/restore", "/tasks/check"} {
		mux.HandleFunc("POST "+path, server.mutate)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
					http.Error(w, "Недопустимый источник запроса", 403)
					return
				}
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) render(w http.ResponseWriter, name string, p Page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, name, p); err != nil {
		slog.Error("render page", "error", err)
	}
}
func (s *Server) cookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	csrf := token()
	s.cookie(w, "login_csrf", csrf, 600)
	s.render(w, "login", Page{CSRF: csrf, Error: r.URL.Query().Get("error")})
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Неверная форма", 400)
		return
	}
	cookie, err := r.Cookie("login_csrf")
	if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(r.FormValue("csrf"))) != 1 {
		http.Error(w, "Обновите страницу входа", 403)
		return
	}
	raw, err := s.store.Login(r.Context(), strings.TrimSpace(r.FormValue("username")), r.FormValue("password"))
	if err != nil {
		http.Redirect(w, r, "/login?error="+url.QueryEscape("Неверное имя пользователя или пароль"), http.StatusSeeOther)
		return
	}
	s.cookie(w, "session", raw, 12*60*60)
	s.cookie(w, "login_csrf", "", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func (s *Server) user(w http.ResponseWriter, r *http.Request) (User, bool) {
	cookie, err := r.Cookie("session")
	if err != nil {
		http.Redirect(w, r, "/login", 303)
		return User{}, false
	}
	u, err := s.store.Session(r.Context(), cookie.Value)
	if err != nil {
		http.Redirect(w, r, "/login", 303)
		return User{}, false
	}
	return u, true
}
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	u, ok := s.user(w, r)
	if !ok {
		return
	}
	p := Page{User: u, CSRF: u.CSRF, Message: r.URL.Query().Get("message"), Error: r.URL.Query().Get("error")}
	var err error
	if p.Plans, err = s.store.Plans(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	if p.Tasks, err = s.store.Tasks(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	if p.Snapshots, err = s.store.Snapshots(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	if p.Audits, err = s.store.Audits(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	for _, t := range p.Tasks {
		switch t.Status {
		case "success":
			p.Success++
		case "failed":
			p.Failed++
		default:
			p.Active++
		}
	}
	entries, err := os.ReadDir(s.cfg.SourceRoot)
	if err != nil {
		slog.Error("read sources", "error", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			p.Sources = append(p.Sources, entry.Name())
		}
	}
	editID, _ := strconv.ParseInt(r.URL.Query().Get("edit"), 10, 64)
	for i := range p.Plans {
		if p.Plans[i].ID == editID {
			p.Edit = &p.Plans[i]
			break
		}
	}
	s.render(w, "dashboard", p)
}
func (s *Server) fail(w http.ResponseWriter, err error) {
	slog.Error("request failed", "error", err)
	http.Error(w, "Не удалось выполнить операцию. Подробности в журнале сервера.", 500)
}
func parseID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("неверный идентификатор")
	}
	return id, nil
}
func (s *Server) mutate(w http.ResponseWriter, r *http.Request) {
	u, ok := s.user(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Неверная форма", 400)
		return
	}
	if subtle.ConstantTimeCompare([]byte(u.CSRF), []byte(r.FormValue("csrf"))) != 1 {
		http.Error(w, "Обновите страницу и повторите действие", 403)
		return
	}
	if r.URL.Path == "/logout" {
		cookie, _ := r.Cookie("session")
		if _, err := s.store.Pool.Exec(r.Context(), "DELETE FROM sessions WHERE token_hash=$1", tokenHash(cookie.Value)); err != nil {
			s.fail(w, err)
			return
		}
		s.cookie(w, "session", "", -1)
		http.Redirect(w, r, "/login", 303)
		return
	}
	if u.Role != "admin" {
		http.Error(w, "Действие доступно администратору", 403)
		return
	}
	var err error
	message := "Действие выполнено"
	switch r.URL.Path {
	case "/plans/save":
		var p Plan
		if value := r.FormValue("id"); value != "" {
			p.ID, err = parseID(value)
			if err != nil {
				break
			}
		}
		p.Name = strings.TrimSpace(r.FormValue("name"))
		p.Source = r.FormValue("source")
		p.Interval, err = strconv.Atoi(r.FormValue("interval"))
		if err != nil {
			break
		}
		p.Keep, err = strconv.Atoi(r.FormValue("keep"))
		if err != nil {
			break
		}
		p.Enabled = r.FormValue("enabled") == "on"
		if p.Name == "" || len([]rune(p.Name)) > 100 || p.Interval < 1 || p.Interval > 43200 || p.Keep < 1 || p.Keep > 100 {
			err = fmt.Errorf("проверьте название, интервал (1–43200 минут) и число версий (1–100)")
			break
		}
		p.Source, err = ValidateSource(s.cfg.SourceRoot, p.Source)
		if err != nil {
			break
		}
		err = s.store.SavePlan(r.Context(), u, p)
		message = "Задание сохранено"
	case "/plans/delete":
		var id int64
		id, err = parseID(r.FormValue("id"))
		if err == nil {
			err = s.store.DeletePlan(r.Context(), u, id)
		}
		message = "Задание удалено. Существующие копии остались в репозитории"
	default:
		var id int64
		id, err = parseID(r.FormValue("id"))
		if err != nil {
			break
		}
		kind := strings.TrimPrefix(r.URL.Path, "/tasks/")
		err = s.store.Enqueue(r.Context(), u, id, kind, r.FormValue("snapshot"))
		message = "Операция добавлена в очередь. Обновите страницу через несколько секунд"
	}
	if err != nil {
		slog.Warn("action rejected", "path", r.URL.Path, "error", err)
		http.Redirect(w, r, "/?error="+url.QueryEscape("Операция не выполнена: проверьте поля и дождитесь завершения активного задания. Подробности в журнале сервера."), 303)
		return
	}
	http.Redirect(w, r, "/?message="+url.QueryEscape(message), 303)
}
