package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"
	"golang.org/x/term"
)

const (
	cookieName   = "rr_session"
	sessionDays  = 7
	argonTime    = 2
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
)

// ---------- senhas ----------

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

func checkPassword(pw, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 2 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// Usado quando o e-mail nao existe, para o tempo de resposta ser igual
var dummyHash = func() string {
	h, _ := hashPassword("senha-que-ninguem-usa")
	return h
}()

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// ---------- limite de tentativas ----------

type limiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

func (l *limiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-15 * time.Minute)
	var kept []time.Time
	for _, t := range l.fails[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, key)
	} else {
		l.fails[key] = kept
	}
	return len(kept) >= 5
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[key] = append(l.fails[key], time.Now())
}

// ---------- Auth ----------

type Auth struct {
	pool   *pgxpool.Pool
	secure bool
	lim    *limiter
}

func newAuth(pool *pgxpool.Pool) *Auth {
	return &Auth{
		pool:   pool,
		secure: os.Getenv("COOKIE_SECURE") == "1",
		lim:    &limiter{fails: map[string][]time.Time{}},
	}
}

func (a *Auth) register() {
	http.HandleFunc("/login", a.handleLogin)
	http.HandleFunc("/logout", a.handleLogout)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Protecao CSRF: formularios so valem se vierem do proprio site
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		o = r.Header.Get("Referer")
	}
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

func (a *Auth) newSession(ctx context.Context, userID int64) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	_, err := a.pool.Exec(ctx, `
		INSERT INTO sessions (token_hash, user_id, expires_at)
		VALUES ($1, $2, now() + make_interval(days => $3))`,
		hashToken(token), userID, sessionDays)
	return token, err
}

func (a *Auth) setCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// Envolve uma rota: so deixa passar quem esta logado
func (a *Auth) require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		var userID int64
		err = a.pool.QueryRow(r.Context(), `
			SELECT user_id FROM sessions
			WHERE token_hash = $1 AND expires_at > now()`, hashToken(c.Value)).Scan(&userID)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		renderLogin(w, "", http.StatusOK)
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil || !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		email := strings.ToLower(strings.TrimSpace(r.PostForm.Get("email")))
		pw := r.PostForm.Get("password")
		ip := clientIP(r)

		if a.lim.blocked(ip) || a.lim.blocked("e:"+email) {
			renderLogin(w, "Too many attempts. Try again in 15 minutes.", http.StatusTooManyRequests)
			return
		}

		var userID int64
		var hash string
		if err := a.pool.QueryRow(r.Context(),
			`SELECT id, password_hash FROM users WHERE email = $1`, email).Scan(&userID, &hash); err != nil {
			userID = 0
			hash = dummyHash
		}
		if !checkPassword(pw, hash) || userID == 0 {
			a.lim.fail(ip)
			a.lim.fail("e:" + email)
			renderLogin(w, "Wrong email or password.", http.StatusUnauthorized)
			return
		}

		token, err := a.newSession(r.Context(), userID)
		if err != nil {
			log.Println("auth: erro criando sessao:", err)
			http.Error(w, "erro interno", http.StatusInternalServerError)
			return
		}
		a.setCookie(w, token, sessionDays*24*3600)
		http.Redirect(w, r, "/sim", http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if c, err := r.Cookie(cookieName); err == nil {
		_, _ = a.pool.Exec(r.Context(), `DELETE FROM sessions WHERE token_hash = $1`, hashToken(c.Value))
	}
	a.setCookie(w, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------- criar usuario pelo terminal ----------

func createUserCmd(ctx context.Context, pool *pgxpool.Pool, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") {
		return errors.New("e-mail invalido")
	}

	fmt.Print("Senha (minimo 12 caracteres, nao aparece na tela): ")
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return err
	}
	fmt.Print("Repita a senha: ")
	pw2, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return err
	}
	if string(pw) != string(pw2) {
		return errors.New("as senhas nao batem")
	}
	if len(pw) < 12 {
		return errors.New("senha curta demais (minimo 12 caracteres)")
	}

	hash, err := hashPassword(string(pw))
	if err != nil {
		return err
	}

	var bizID int64
	err = pool.QueryRow(ctx, `SELECT id FROM businesses WHERE twilio_number = $1`, demoNumber).Scan(&bizID)
	if err != nil {
		return errors.New("negocio demo nao existe: rode uma vez com SIMULATOR=1")
	}

	_, err = pool.Exec(ctx,
		`INSERT INTO users (business_id, email, password_hash) VALUES ($1, $2, $3)`, bizID, email, hash)
	return err
}

// ---------- pagina de login ----------

func renderLogin(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := loginTmpl.Execute(w, msg); err != nil {
		log.Println("login template:", err)
	}
}

var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ReboundRelay - Log in</title>
<style>
body{margin:0;font-family:-apple-system,system-ui,sans-serif;background:#f1f4f7;display:grid;place-items:center;min-height:100vh}
.card{background:#fff;border:1px solid #dde3e9;border-radius:14px;padding:28px;width:320px}
h1{font-size:20px;margin:0 0 4px;color:#0b3a53}
p{margin:0 0 18px;color:#6b7785;font-size:14px}
input{width:100%;padding:10px;margin-bottom:12px;border:1px solid #c5ced7;border-radius:8px;font-size:14px;box-sizing:border-box}
button{width:100%;padding:10px;border:0;border-radius:8px;background:#0b6fa4;color:#fff;font-size:15px;cursor:pointer}
.err{background:#fdecea;color:#a12a1e;padding:8px 12px;border-radius:8px;font-size:13px;margin-bottom:12px}
</style></head><body>
<form class="card" method="post" action="/login">
<h1>ReboundRelay</h1><p>Log in to your dashboard</p>
{{if .}}<div class="err">{{.}}</div>{{end}}
<input type="email" name="email" placeholder="Email" required autofocus>
<input type="password" name="password" placeholder="Password" required>
<button>Log in</button>
</form></body></html>`))