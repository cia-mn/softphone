// SMS via Mobinet's web portal (phone.mobinet.mn). Mobinet does NOT deliver SMS
// over SIP MESSAGE — sending is a portal action: log in for a PHPSESSID cookie,
// then POST the SMS form. This client mirrors that browser flow.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

// smsSend sends an SMS; nil when SMS is not configured. Read by POST /sms.
var smsSend func(ctx context.Context, to, text string) (status int, response string, err error)

type portalSMS struct {
	base string
	user string
	pass string
	hc   *http.Client
	mu   sync.Mutex
	in   bool // whether we currently hold a logged-in session
}

func newPortalSMS(base, user, pass string) *portalSMS {
	jar, _ := cookiejar.New(nil)
	return &portalSMS{
		base: strings.TrimRight(base, "/"),
		user: user,
		pass: pass,
		hc: &http.Client{
			Jar:     jar,
			Timeout: 20 * time.Second,
			// Don't auto-follow redirects: the portal answers an expired session
			// with a 302 to the login page, which we detect and re-authenticate.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (p *portalSMS) hasSession() bool {
	u, err := url.Parse(p.base)
	if err != nil {
		return false
	}
	for _, c := range p.hc.Jar.Cookies(u) {
		if c.Name == "PHPSESSID" && c.Value != "" {
			return true
		}
	}
	return false
}

func (p *portalSMS) login(ctx context.Context) error {
	form := url.Values{"name": {p.user}, "password": {p.pass}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/includes/login.php", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if !p.hasSession() {
		return fmt.Errorf("portal login failed (check SMS_USER/SMS_PASS)")
	}
	p.in = true
	return nil
}

func (p *portalSMS) postSMS(ctx context.Context, to, text string) (int, string, error) {
	form := url.Values{
		"numbers": {to},
		"body":    {text},
		"sdate":   {"Яг одоо"}, // "right now" — send immediately
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/pages/user_sms_send_ajax.php", strings.NewReader(form.Encode()))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := p.hc.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return resp.StatusCode, strings.TrimSpace(string(b)), nil
}

// send logs in if needed, posts the SMS, and re-authenticates once if the session
// expired (the portal bounces an expired session to the login page).
func (p *portalSMS) send(ctx context.Context, to, text string) (int, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.in {
		if err := p.login(ctx); err != nil {
			return 0, "", err
		}
	}
	status, body, err := p.postSMS(ctx, to, text)
	if err != nil {
		return 0, "", err
	}
	if status == http.StatusFound || status == http.StatusUnauthorized || status == http.StatusForbidden {
		p.in = false
		if err := p.login(ctx); err != nil {
			return 0, "", err
		}
		if status, body, err = p.postSMS(ctx, to, text); err != nil {
			return 0, "", err
		}
	}
	return status, snippet(body), nil
}

func snippet(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
