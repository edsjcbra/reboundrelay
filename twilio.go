package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type App struct {
	pool       *pgxpool.Pool
	authToken  string
	accountSID string
	baseURL    string
	http       *http.Client
}

func newApp(pool *pgxpool.Pool) *App {
	a := &App{
		pool:       pool,
		authToken:  os.Getenv("TWILIO_AUTH_TOKEN"),
		accountSID: os.Getenv("TWILIO_ACCOUNT_SID"),
		baseURL:    strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"),
		http:       &http.Client{Timeout: 10 * time.Second},
	}
	if a.authToken == "" || a.accountSID == "" || a.baseURL == "" {
		log.Fatal("faltam TWILIO_AUTH_TOKEN, TWILIO_ACCOUNT_SID ou PUBLIC_BASE_URL no .env")
	}
	return a
}

func (a *App) register() {
	http.HandleFunc("/twilio/voice", a.twilioOnly(a.handleVoice))
	http.HandleFunc("/twilio/dial-status", a.twilioOnly(a.handleDialStatus))
}

// Confere se a requisicao veio mesmo da Twilio (HMAC-SHA1 do URL + parametros)
func (a *App) validSignature(r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		return false
	}
	keys := make([]string, 0, len(r.PostForm))
	for k := range r.PostForm {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString(a.baseURL + r.URL.RequestURI())
	for _, k := range keys {
		for _, v := range r.PostForm[k] {
			sb.WriteString(k)
			sb.WriteString(v)
		}
	}

	mac := hmac.New(sha1.New, []byte(a.authToken))
	mac.Write([]byte(sb.String()))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	got := r.Header.Get("X-Twilio-Signature")
	return subtle.ConstantTimeCompare([]byte(expected), []byte(got)) == 1
}

func (a *App) twilioOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !a.validSignature(r) {
			log.Println("assinatura invalida, requisicao rejeitada")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func writeXML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/xml")
	fmt.Fprint(w, body)
}

// Chamada entrando: toca o celular do dono por 20 segundos
func (a *App) handleVoice(w http.ResponseWriter, r *http.Request) {
	to := r.PostForm.Get("To")

	var forwardTo string
	err := a.pool.QueryRow(r.Context(),
		`SELECT forward_to FROM businesses WHERE twilio_number = $1`, to).Scan(&forwardTo)
	if err != nil {
		writeXML(w, `<Response><Say>Sorry, this number is not in service.</Say><Hangup/></Response>`)
		return
	}

	writeXML(w, fmt.Sprintf(
		`<Response><Dial timeout="20" action="/twilio/dial-status"><Number>%s</Number></Dial></Response>`,
		html.EscapeString(forwardTo)))
}

// Terminou a tentativa de ligar para o dono: se nao atendeu, manda o SMS
func (a *App) handleDialStatus(w http.ResponseWriter, r *http.Request) {
	defer writeXML(w, `<Response/>`)

	if r.PostForm.Get("DialCallStatus") == "completed" {
		return // o dono atendeu, nada a fazer
	}

	caller := r.PostForm.Get("From")
	twilioNumber := r.PostForm.Get("To")
	ctx := r.Context()

	var bizID int64
	var bizName string
	err := a.pool.QueryRow(ctx,
		`SELECT id, name FROM businesses WHERE twilio_number = $1`, twilioNumber).Scan(&bizID, &bizName)
	if err != nil {
		log.Println("negocio nao encontrado:", err)
		return
	}

	var contactID int64
	var optedOut bool
	err = a.pool.QueryRow(ctx, `
		INSERT INTO contacts (business_id, phone) VALUES ($1, $2)
		ON CONFLICT (business_id, phone) DO UPDATE SET phone = EXCLUDED.phone
		RETURNING id, opted_out`, bizID, caller).Scan(&contactID, &optedOut)
	if err != nil {
		log.Println("erro ao salvar contato:", err)
		return
	}
	if optedOut {
		return // a pessoa mandou STOP antes, nunca mais mandamos
	}

	// Evita mandar varios SMS se a pessoa ligar varias vezes seguidas
	var recent bool
	_ = a.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM messages
			WHERE business_id = $1 AND contact_id = $2
			  AND direction = 'outbound'
			  AND created_at > now() - interval '10 minutes')`, bizID, contactID).Scan(&recent)
	if recent {
		return
	}

	body := fmt.Sprintf("Hi, this is %s. Sorry we missed your call! How can we help you? Reply STOP to opt out.", bizName)
	sid, err := a.sendSMS(ctx, twilioNumber, caller, body)
	if err != nil {
		log.Println("erro ao enviar SMS:", err)
		return
	}

	_, err = a.pool.Exec(ctx, `
		INSERT INTO messages (business_id, contact_id, direction, body, twilio_sid)
		VALUES ($1, $2, 'outbound', $3, $4)`, bizID, contactID, body, sid)
	if err != nil {
		log.Println("erro ao salvar mensagem:", err)
	}
	log.Println("SMS de ligacao perdida enviado, sid:", sid)
}

func (a *App) sendSMS(ctx context.Context, from, to, body string) (string, error) {
	form := url.Values{"From": {from}, "To": {to}, "Body": {body}}
	endpoint := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Messages.json", a.accountSID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(a.accountSID, a.authToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out struct {
		SID     string `json:"sid"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("twilio respondeu %d: %s", resp.StatusCode, out.Message)
	}
	return out.SID, nil
}