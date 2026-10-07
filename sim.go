package main

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Numeros 555-01xx sao ficticios (reservados), nunca pertencem a ninguem.
const demoNumber = "+15555550100"

type Sim struct {
	pool  *pgxpool.Pool
	bizID int64
}

type simContact struct {
	ID       int64
	Phone    string
	OptedOut bool
}

type simMessage struct {
	Direction string
	Body      string
	At        string
}

type simPage struct {
	BizName  string
	Contacts []simContact
	Selected *simContact
	SelID    int64
	Messages []simMessage
	Note     string
}

func newSim(ctx context.Context, pool *pgxpool.Pool) (*Sim, error) {
	_, err := pool.Exec(ctx, `
		INSERT INTO businesses (name, twilio_number, forward_to)
		VALUES ('Gulf Coast Plumbing', $1, '+15555550101')
		ON CONFLICT (twilio_number) DO NOTHING`, demoNumber)
	if err != nil {
		return nil, err
	}
	var id int64
	err = pool.QueryRow(ctx, `SELECT id FROM businesses WHERE twilio_number = $1`, demoNumber).Scan(&id)
	if err != nil {
		return nil, err
	}
	return &Sim{pool: pool, bizID: id}, nil
}

func (s *Sim) register() {
	http.HandleFunc("/sim", s.handlePage)
	http.HandleFunc("/sim/missed", s.post(s.handleMissed))
	http.HandleFunc("/sim/reply", s.post(s.handleReply))
	http.HandleFunc("/sim/rename", s.post(s.handleRename))
	http.HandleFunc("/sim/reset", s.post(s.handleReset))
}

func (s *Sim) post(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		next(w, r)
	}
}

func back(w http.ResponseWriter, r *http.Request, contactID int64, note string) {
	q := url.Values{}
	if contactID > 0 {
		q.Set("c", strconv.FormatInt(contactID, 10))
	}
	if note != "" {
		q.Set("n", note)
	}
	u := "/sim"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

func normalizePhone(s string) (string, bool) {
	var digits strings.Builder
	for _, ch := range s {
		if ch >= '0' && ch <= '9' {
			digits.WriteRune(ch)
		}
	}
	d := digits.String()
	if len(d) == 10 {
		d = "1" + d
	}
	if len(d) != 11 || d[0] != '1' {
		return "", false
	}
	return "+" + d, true
}

func (s *Sim) addMessage(ctx context.Context, contactID int64, direction, body string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO messages (business_id, contact_id, direction, body)
		VALUES ($1, $2, $3, $4)`, s.bizID, contactID, direction, body)
	return err
}

func (s *Sim) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	var page simPage

	if err := s.pool.QueryRow(ctx, `SELECT name FROM businesses WHERE id = $1`, s.bizID).Scan(&page.BizName); err != nil {
		log.Println("sim: erro lendo negocio:", err)
		http.Error(w, "erro interno", http.StatusInternalServerError)
		return
	}

	rows, err := s.pool.Query(ctx,
		`SELECT id, phone, opted_out FROM contacts WHERE business_id = $1 ORDER BY id DESC`, s.bizID)
	if err != nil {
		log.Println("sim: erro lendo contatos:", err)
		http.Error(w, "erro interno", http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var c simContact
		if err := rows.Scan(&c.ID, &c.Phone, &c.OptedOut); err != nil {
			rows.Close()
			http.Error(w, "erro interno", http.StatusInternalServerError)
			return
		}
		page.Contacts = append(page.Contacts, c)
	}
	rows.Close()

	selID, _ := strconv.ParseInt(r.URL.Query().Get("c"), 10, 64)
	for i := range page.Contacts {
		if page.Contacts[i].ID == selID {
			page.Selected = &page.Contacts[i]
		}
	}
	if page.Selected == nil && len(page.Contacts) > 0 {
		page.Selected = &page.Contacts[0]
	}

	if page.Selected != nil {
		page.SelID = page.Selected.ID
		mrows, err := s.pool.Query(ctx, `
			SELECT direction, body, created_at FROM messages
			WHERE business_id = $1 AND contact_id = $2
			ORDER BY created_at, id`, s.bizID, page.Selected.ID)
		if err != nil {
			http.Error(w, "erro interno", http.StatusInternalServerError)
			return
		}
		for mrows.Next() {
			var m simMessage
			var t time.Time
			if err := mrows.Scan(&m.Direction, &m.Body, &t); err != nil {
				mrows.Close()
				http.Error(w, "erro interno", http.StatusInternalServerError)
				return
			}
			m.At = t.Local().Format("3:04 PM")
			page.Messages = append(page.Messages, m)
		}
		mrows.Close()
	}

	switch r.URL.Query().Get("n") {
	case "bad":
		page.Note = "Invalid number. Use the format +12515550142."
	case "recent":
		page.Note = "Anti-spam: we already texted this number in the last 10 minutes. Use another number or click Reset demo."
	case "optout":
		page.Note = "This number replied STOP. We never text it again."
	}

	if err := simTmpl.Execute(w, page); err != nil {
		log.Println("sim: erro no template:", err)
	}
}

func (s *Sim) handleMissed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	phone, ok := normalizePhone(r.PostForm.Get("phone"))
	if !ok {
		back(w, r, 0, "bad")
		return
	}

	var name string
	if err := s.pool.QueryRow(ctx, `SELECT name FROM businesses WHERE id = $1`, s.bizID).Scan(&name); err != nil {
		log.Println("sim:", err)
		http.Error(w, "erro interno", http.StatusInternalServerError)
		return
	}

	var contactID int64
	var optedOut bool
	err := s.pool.QueryRow(ctx, `
		INSERT INTO contacts (business_id, phone) VALUES ($1, $2)
		ON CONFLICT (business_id, phone) DO UPDATE SET phone = EXCLUDED.phone
		RETURNING id, opted_out`, s.bizID, phone).Scan(&contactID, &optedOut)
	if err != nil {
		log.Println("sim:", err)
		http.Error(w, "erro interno", http.StatusInternalServerError)
		return
	}
	if optedOut {
		back(w, r, contactID, "optout")
		return
	}

	var recent bool
	_ = s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM messages
			WHERE business_id = $1 AND contact_id = $2
			  AND direction = 'outbound'
			  AND created_at > now() - interval '10 minutes')`, s.bizID, contactID).Scan(&recent)
	if recent {
		back(w, r, contactID, "recent")
		return
	}

	body := fmt.Sprintf("Hi, this is %s. Sorry we missed your call! How can we help you? Reply STOP to opt out.", name)
	if err := s.addMessage(ctx, contactID, "outbound", body); err != nil {
		log.Println("sim:", err)
		http.Error(w, "erro interno", http.StatusInternalServerError)
		return
	}
	back(w, r, contactID, "")
}

func (s *Sim) handleReply(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cid, _ := strconv.ParseInt(r.PostForm.Get("c"), 10, 64)
	body := strings.TrimSpace(r.PostForm.Get("body"))
	if cid == 0 || body == "" || len(body) > 500 {
		back(w, r, cid, "")
		return
	}

	// Isolamento: o contato precisa pertencer a ESTE negocio
	var optedOut bool
	err := s.pool.QueryRow(ctx,
		`SELECT opted_out FROM contacts WHERE id = $1 AND business_id = $2`, cid, s.bizID).Scan(&optedOut)
	if err != nil {
		back(w, r, 0, "")
		return
	}

	if err := s.addMessage(ctx, cid, "inbound", body); err != nil {
		log.Println("sim:", err)
		http.Error(w, "erro interno", http.StatusInternalServerError)
		return
	}

	switch strings.ToUpper(body) {
	case "STOP", "STOPALL", "UNSUBSCRIBE", "CANCEL", "END", "QUIT":
		_, _ = s.pool.Exec(ctx, `UPDATE contacts SET opted_out = true WHERE id = $1 AND business_id = $2`, cid, s.bizID)
		_ = s.addMessage(ctx, cid, "outbound", "You have been unsubscribed and will not receive more messages. Reply START to resubscribe.")
	case "START", "UNSTOP":
		_, _ = s.pool.Exec(ctx, `UPDATE contacts SET opted_out = false WHERE id = $1 AND business_id = $2`, cid, s.bizID)
		_ = s.addMessage(ctx, cid, "outbound", "You have been resubscribed.")
	}
	back(w, r, cid, "")
}

func (s *Sim) handleRename(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if name == "" || len(name) > 60 {
		back(w, r, 0, "")
		return
	}
	_, err := s.pool.Exec(r.Context(), `UPDATE businesses SET name = $1 WHERE id = $2`, name, s.bizID)
	if err != nil {
		log.Println("sim:", err)
	}
	back(w, r, 0, "")
}

func (s *Sim) handleReset(w http.ResponseWriter, r *http.Request) {
	// messages some em cascata junto com contacts
	_, err := s.pool.Exec(r.Context(), `DELETE FROM contacts WHERE business_id = $1`, s.bizID)
	if err != nil {
		log.Println("sim:", err)
	}
	back(w, r, 0, "")
}

var simTmpl = template.Must(template.New("sim").Parse(simHTML))

const simHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ReboundRelay - Live Demo</title>
<style>
*{box-sizing:border-box}
body{margin:0;font-family:-apple-system,system-ui,"Segoe UI",sans-serif;background:#f1f4f7;color:#18222d}
header{background:#0b3a53;color:#fff;padding:14px 24px;display:flex;justify-content:space-between;align-items:center;flex-wrap:wrap;gap:8px}
header h1{font-size:18px;margin:0}
.tag{font-size:12px;opacity:.8}
.bar{display:flex;gap:16px;flex-wrap:wrap;padding:16px 24px;background:#fff;border-bottom:1px solid #dde3e9}
.bar form{display:flex;gap:8px;align-items:center}
input[type=text]{padding:8px 10px;border:1px solid #c5ced7;border-radius:8px;font-size:14px}
button{padding:8px 14px;border:0;border-radius:8px;background:#0b6fa4;color:#fff;font-size:14px;cursor:pointer}
button.red{background:#c0392b}
button.gray{background:#6b7785}
.note{margin:12px 24px 0;padding:10px 14px;background:#fff4d6;border:1px solid #f0d58a;border-radius:8px;font-size:14px}
main{display:grid;grid-template-columns:340px 1fr;gap:24px;padding:24px;align-items:start}
@media(max-width:800px){main{grid-template-columns:1fr}}
.phone{width:300px;margin:0 auto;border:10px solid #1b1f24;border-radius:36px;background:#fff;overflow:hidden}
.phone .top{background:#f6f6f6;padding:12px;text-align:center;font-size:13px;border-bottom:1px solid #e3e3e3}
.phone .scr{height:420px;overflow-y:auto;padding:12px;display:flex;flex-direction:column;gap:8px;background:#fff}
.b{max-width:80%;padding:8px 12px;border-radius:16px;font-size:14px;line-height:1.35}
.recv{background:#e9e9eb;align-self:flex-start}
.sent{background:#1b84ff;color:#fff;align-self:flex-end}
.label{text-align:center;font-size:13px;color:#6b7785;margin-top:10px}
.panel{background:#fff;border:1px solid #dde3e9;border-radius:14px;overflow:hidden}
.panel h2{margin:0;padding:14px 18px;font-size:16px;border-bottom:1px solid #eef1f4}
.cols{display:grid;grid-template-columns:200px 1fr}
.list a{display:block;padding:12px 16px;border-bottom:1px solid #eef1f4;color:inherit;text-decoration:none;font-size:14px}
.list a.on{background:#e8f3fa;font-weight:600}
.thread{padding:16px;min-height:300px;display:flex;flex-direction:column;gap:8px}
.reply{display:flex;gap:8px;width:300px;margin:12px auto 0}
.reply input{flex:1;min-width:0}
.empty{padding:24px;color:#6b7785;font-size:14px}
.missed{background:#fdecea;color:#a12a1e;padding:8px 12px;border-radius:8px;font-size:13px}
</style>
</head>
<body>
<header>
  <h1>ReboundRelay <span class="tag">- Never lose a customer to a missed call</span></h1>
  <span class="tag">Demo mode - no real texts are sent</span>
</header>

<div class="bar">
  <form method="post" action="/sim/rename">
    <input type="text" name="name" value="{{.BizName}}" maxlength="60" aria-label="Business name">
    <button class="gray">Set business name</button>
  </form>
  <form method="post" action="/sim/missed">
    <input type="text" name="phone" value="+12515550142" aria-label="Caller phone">
    <button>Simulate missed call</button>
  </form>
  <form method="post" action="/sim/reset">
    <button class="red">Reset demo</button>
  </form>
</div>

{{if .Note}}<div class="note">{{.Note}}</div>{{end}}

<main>
  <section>
    <div class="phone">
      <div class="top">Messages - {{.BizName}}</div>
      <div class="scr">
        {{range .Messages}}<div class="b {{if eq .Direction "outbound"}}recv{{else}}sent{{end}}">{{.Body}}</div>{{end}}
      </div>
    </div>
    <div class="label">Customer's phone</div>
    {{if .Selected}}
    <form class="reply" method="post" action="/sim/reply">
      <input type="hidden" name="c" value="{{.Selected.ID}}">
      <input type="text" name="body" placeholder="Reply as the customer (try STOP)" maxlength="500">
      <button>Send</button>
    </form>
    {{end}}
  </section>

  <section class="panel">
    <h2>Owner inbox</h2>
    {{if .Contacts}}
    <div class="cols">
      <div class="list">
        {{range .Contacts}}<a href="/sim?c={{.ID}}" class="{{if eq .ID $.SelID}}on{{end}}">{{.Phone}}{{if .OptedOut}} (opted out){{end}}</a>{{end}}
      </div>
      <div class="thread">
        {{if .Selected}}<div class="missed">Missed call from {{.Selected.Phone}}</div>{{end}}
        {{range .Messages}}<div class="b {{if eq .Direction "outbound"}}sent{{else}}recv{{end}}">{{.Body}}<div style="font-size:11px;opacity:.7;margin-top:4px">{{.At}}</div></div>{{end}}
      </div>
    </div>
    {{else}}
    <div class="empty">No conversations yet. Click "Simulate missed call" to see the magic.</div>
    {{end}}
  </section>
</main>
</body>
</html>`