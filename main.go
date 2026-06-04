// Command softphone is the Go-side SIP user agent for the cia.mn softphone.
//
// Usage:
//
//	go run .                 register to the SIP server and stay online
//	go run . call <number>   register, then place a test call that plays audio
//
// It connects to a SIP registrar (e.g. ip-phone.mobinet.mn) using the digest
// username/password issued by the provider. A later milestone adds a WebRTC
// bridge so a browser becomes the actual phone.
//
// Configuration is read from environment variables, loaded from a local .env
// file if present (see .env.example). Credentials are never hard-coded.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/diago/testdata"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func main() {
	// Load .env (best effort; real env vars always win).
	if err := loadDotEnv(".env"); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "warning: could not read .env:", err)
	}

	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		// At debug level sipgo prints every SIP message it sends/receives,
		// which is exactly what you want when diagnosing registration.
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}

	// Optional subcommand: "call <number>" places an outbound test call.
	var callDest string
	if args := os.Args[1:]; len(args) > 0 && args[0] == "call" {
		if len(args) < 2 || args[1] == "" {
			slog.Error("usage: go run . call <number>")
			os.Exit(2)
		}
		callDest = args[1]
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := run(ctx, cfg, callDest); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("stopped", "error", err)
		os.Exit(1)
	}
	slog.Info("stopped")
}

type config struct {
	Domain    string        // SIP_DOMAIN  e.g. ip-phone.mobinet.mn
	User      string        // SIP_USER    AOR user / phone number (the From/To identity)
	AuthUser  string        // SIP_AUTH_USER  digest auth username (defaults to User; set if the provider uses a separate "auth ID")
	Pass      string        // SIP_PASS    digest password
	Port      int           // SIP_PORT    registrar port (default 5060)
	Transport string        // SIP_TRANSPORT  udp|tcp|tls (default udp)
	BindHost  string        // BIND_HOST   local IP to bind/advertise (default: auto-detected outbound IP)
	BindPort  int           // BIND_PORT   local SIP port (default 0 = OS picks a free port; set a fixed one to deploy)
	Expiry    time.Duration // SIP_EXPIRY  requested registration lifetime in seconds (default 60)

	// RTP media UDP port range. 0/0 = OS ephemeral ports (fine locally). Set a
	// fixed range to deploy behind a firewall so you can open exactly these ports.
	RTPPortStart int // RTP_PORT_START
	RTPPortEnd   int // RTP_PORT_END

	// NAT: the public address to advertise in Contact/SDP so inbound calls and
	// their media can reach us. If PublicHost is empty, it is auto-discovered via
	// STUN. On a public-IP host, set PublicHost to that IP and STUN is unnecessary.
	PublicHost string // PUBLIC_HOST  public IP to advertise in SDP (overrides STUN)
	Stun       string // STUN_SERVER  host:port for public-IP auto-discovery (empty disables)

	// ForwardTo, if set, turns inbound handling into a "press 1 to be forwarded"
	// IVR: when the caller presses 1 we bridge them to this number.
	ForwardTo string // FORWARD_TO

	// Concurrency: at most ForwardConcurrency calls are bridged to ForwardTo at
	// once; extra callers wait (hearing hold audio) in a FIFO queue up to QueueTimeout.
	ForwardConcurrency int           // FORWARD_CONCURRENCY (default 1)
	QueueTimeout       time.Duration // QUEUE_TIMEOUT seconds (default 120)

	// Audio clips (8 kHz mono 16-bit WAV). A path on disk is used if it exists;
	// otherwise the name falls back to a bundled demo clip.
	PromptFile     string // PROMPT_FILE      greeting played while waiting for the keypress
	ConnectingFile string // CONNECTING_FILE  "connecting…" played while the forward leg rings
	HoldFile       string // HOLD_FILE        hold audio while a caller waits in the forward queue
}

func loadConfig() (config, error) {
	c := config{
		Domain:    os.Getenv("SIP_DOMAIN"),
		User:      os.Getenv("SIP_USER"),
		AuthUser:  os.Getenv("SIP_AUTH_USER"),
		Pass:      os.Getenv("SIP_PASS"),
		Transport: strings.ToLower(getenvDefault("SIP_TRANSPORT", "udp")),
		BindHost:  os.Getenv("BIND_HOST"),
		Port:      getenvInt("SIP_PORT", 5060),
		BindPort:  getenvInt("BIND_PORT", 0),
		Expiry:    time.Duration(getenvInt("SIP_EXPIRY", 60)) * time.Second,

		RTPPortStart: getenvInt("RTP_PORT_START", 0),
		RTPPortEnd:   getenvInt("RTP_PORT_END", 0),

		PublicHost: os.Getenv("PUBLIC_HOST"),
		Stun:       getenvDefault("STUN_SERVER", "stun.l.google.com:19302"),

		ForwardTo:          os.Getenv("FORWARD_TO"),
		ForwardConcurrency: getenvInt("FORWARD_CONCURRENCY", 1),
		QueueTimeout:       time.Duration(getenvInt("QUEUE_TIMEOUT", 120)) * time.Second,

		PromptFile:     getenvDefault("PROMPT_FILE", "sounds/start.wav"),
		ConnectingFile: getenvDefault("CONNECTING_FILE", "sounds/calling-forward.wav"),
		HoldFile:       getenvDefault("HOLD_FILE", "sounds/waiting-queue.wav"),
	}
	if c.Domain == "" || c.User == "" || c.Pass == "" {
		return c, errors.New("SIP_DOMAIN, SIP_USER and SIP_PASS must be set (copy .env.example to .env and fill them in)")
	}
	if c.AuthUser == "" {
		c.AuthUser = c.User
	}
	if c.ForwardConcurrency < 1 {
		c.ForwardConcurrency = 1
	}
	if c.BindHost == "" {
		ip, err := outboundIP(net.JoinHostPort(c.Domain, strconv.Itoa(c.Port)))
		if err != nil {
			return c, fmt.Errorf("could not auto-detect a local IP to reach %s (set BIND_HOST in .env): %w", c.Domain, err)
		}
		c.BindHost = ip
	}
	return c, nil
}

func run(ctx context.Context, cfg config, callDest string) error {
	// The AOR we register: sip:user@domain. This becomes the request-URI and the
	// To header (the address-of-record the registrar binds our contact to).
	recipientStr := fmt.Sprintf("sip:%s@%s", cfg.User, cfg.Domain)
	if cfg.Transport != "udp" {
		recipientStr += ";transport=" + cfg.Transport
	}
	var recipient sip.Uri
	if err := sip.ParseUri(recipientStr, &recipient); err != nil {
		return fmt.Errorf("parse AOR %q: %w", recipientStr, err)
	}

	// Resolve the registrar so we send packets to a concrete IP:port (and can log
	// exactly where we're talking to). The From/To still carry the domain name.
	serverHost, err := resolveServer(cfg.Domain, cfg.Port)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", cfg.Domain, err)
	}

	// UA identity drives the From header: From = sip:<name>@<hostname>.
	// name = SIP user, hostname = SIP domain  ->  From: sip:user@domain (a proper AOR).
	// Via/Contact are taken from the transport bind below, not from this hostname.
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(cfg.User),
		sipgo.WithUserAgentHostname(cfg.Domain),
		// Mobinet's VoipSwitch sends "WWW-Authenticate: DIGEST ..." (uppercase
		// scheme); the digest library only accepts "Digest". This parser fixes it.
		sipgo.WithUserAgentParser(newSIPParser()),
	)
	if err != nil {
		return fmt.Errorf("create user agent: %w", err)
	}
	defer ua.Close()

	// Advertise a public media IP in SDP so the carrier's RTP can reach us through
	// NAT (inbound calls were connecting but the audio died because the SDP carried
	// the private IP). The IP is reliable on any NAT type; ports are handled by the
	// carrier via symmetric RTP. Source: explicit PUBLIC_HOST, else STUN. On a
	// public-IP host just set BIND_HOST to that IP and none of this is needed.
	publicIP := cfg.PublicHost
	if publicIP == "" && cfg.Stun != "" {
		if ip, _, derr := discoverPublicAddr(cfg.Stun); derr != nil {
			slog.Warn("STUN discovery failed (set PUBLIC_HOST if inbound audio is one-way)", "stun", cfg.Stun, "error", derr)
		} else {
			publicIP = ip.String()
			slog.Info("public IP via STUN", "ip", publicIP)
		}
	}

	// Constrain the RTP/RTCP UDP port range so a firewall can be opened predictably.
	// (media.RTPPortStart/End are global; 0/0 leaves it to OS ephemeral ports.)
	if cfg.RTPPortStart > 0 && cfg.RTPPortEnd > cfg.RTPPortStart {
		media.RTPPortStart = cfg.RTPPortStart
		media.RTPPortEnd = cfg.RTPPortEnd
	}

	tr := diago.Transport{
		Transport:      cfg.Transport,
		BindHost:       cfg.BindHost,
		BindPort:       cfg.BindPort,
		RewriteContact: true, // behind NAT: route in-dialog requests to the source, not the peer Contact
	}
	if ip := net.ParseIP(publicIP); ip != nil {
		tr.MediaExternalIP = ip
	}
	dg := diago.NewDiago(ua,
		diago.WithTransport(tr),
		// Mobinet sends DTMF as SIP INFO (application/dtmf-relay), which diago's
		// server side doesn't process — we intercept it ourselves.
		diago.WithServerRequestMiddleware(dtmfInfoMiddleware),
	)

	logArgs := []any{
		"aor", recipientStr,
		"auth_user", cfg.AuthUser,
		"server", serverHost,
		"transport", cfg.Transport,
		"local", net.JoinHostPort(cfg.BindHost, strconv.Itoa(cfg.BindPort)),
		"expiry", cfg.Expiry,
	}
	if tr.MediaExternalIP != nil {
		logArgs = append(logArgs, "media_public_ip", tr.MediaExternalIP.String())
	}
	if media.RTPPortStart > 0 {
		logArgs = append(logArgs, "rtp_ports", fmt.Sprintf("%d-%d/udp", media.RTPPortStart, media.RTPPortEnd))
	}
	slog.Info("registering", logArgs...)

	// Forward queue: cap concurrent forwards; extra callers wait (on hold) FIFO.
	forward.sem = make(chan struct{}, cfg.ForwardConcurrency)
	forward.timeout = cfg.QueueTimeout
	forward.holdFile = cfg.HoldFile
	forward.connectingFile = cfg.ConnectingFile

	if cfg.ForwardTo != "" {
		slog.Info("audio clips",
			"prompt", cfg.PromptFile, "prompt_from", audioSource(cfg.PromptFile),
			"connecting", cfg.ConnectingFile, "connecting_from", audioSource(cfg.ConnectingFile),
			"hold", cfg.HoldFile, "hold_from", audioSource(cfg.HoldFile))
	}

	// Answer inbound calls. With FORWARD_TO set this is a "press 1 to forward"
	// IVR; otherwise it plays a prompt and echoes.
	go func() {
		if err := dg.Serve(ctx, func(in *diago.DialogServerSession) {
			handleInbound(dg, cfg, in)
		}); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("serve loop ended", "error", err)
		}
	}()

	// dg.Register blocks (it keeps the binding refreshed), so run it in the
	// background and wait for the first success before doing anything else.
	registered := make(chan struct{}, 1)
	regDone := make(chan error, 1)
	go func() {
		err := dg.Register(ctx, recipient, diago.RegisterOptions{
			Username:  cfg.AuthUser,
			Password:  cfg.Pass,
			ProxyHost: serverHost, // send REGISTER to this resolved IP:port
			Expiry:    cfg.Expiry,
			OnRegistered: func() {
				slog.Info("REGISTERED ✓")
				select {
				case registered <- struct{}{}:
				default:
				}
			},
		})
		// On a SIP-level rejection, dump the exact exchange so the reason is visible.
		// (The request includes our Authorization header; the password is never in it.)
		var rerr *diago.RegisterResponseError
		if errors.As(err, &rerr) {
			fmt.Fprintf(os.Stderr, "\n===== SENT (our authenticated REGISTER) =====\n%s\n", rerr.RegisterReq)
			fmt.Fprintf(os.Stderr, "===== RECEIVED (server reply) =====\n%s\n", rerr.RegisterRes)
		}
		regDone <- err
	}()

	select {
	case <-registered:
	case err := <-regDone:
		if err != nil {
			return err
		}
		return errors.New("registration ended unexpectedly")
	case <-time.After(15 * time.Second):
		return errors.New("registration did not complete within 15s")
	case <-ctx.Done():
		return ctx.Err()
	}

	// Register-only mode: stay registered until interrupted.
	if callDest == "" {
		slog.Info("binding is live — press Ctrl-C to stop")
		select {
		case <-ctx.Done():
			return nil
		case err := <-regDone:
			return err
		}
	}

	// Call mode: place the outbound call, play audio, hang up.
	return placeCall(ctx, dg, cfg, callDest)
}

// placeCall dials dest@domain and, once answered, plays a test audio clip to the
// callee, then hangs up. This exercises the full SIP+RTP path the browser will
// later reuse: INVITE (with digest auth), SDP/codec negotiation, and outbound RTP.
func placeCall(ctx context.Context, dg *diago.Diago, cfg config, dest string) error {
	calleeStr := fmt.Sprintf("sip:%s@%s", dest, cfg.Domain)
	var callee sip.Uri
	if err := sip.ParseUri(calleeStr, &callee); err != nil {
		return fmt.Errorf("parse callee %q: %w", calleeStr, err)
	}
	slog.Info("calling", "to", calleeStr)

	// Allow up to 60s for the callee to answer.
	dialCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	sess, err := dg.Invite(dialCtx, callee, diago.InviteOptions{
		Username: cfg.AuthUser,
		Password: cfg.Pass,
		Headers:  []sip.Header{}, // diago requires this to be non-nil
		OnResponse: func(res *sip.Response) error {
			slog.Info("call progress", "response", res.StartLine())
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("call not answered: %w", err)
	}
	defer sess.Close()
	slog.Info("ANSWERED ✓ — playing test audio to the callee")

	// Send BYE before closing.
	defer func() {
		hctx, hcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer hcancel()
		if err := sess.Hangup(hctx); err != nil {
			slog.Warn("hangup", "error", err)
		}
	}()

	wav, err := testdata.OpenFile("demo-echotest.wav")
	if err != nil {
		return fmt.Errorf("open test audio: %w", err)
	}
	defer wav.Close()

	pb, err := sess.PlaybackCreate()
	if err != nil {
		return fmt.Errorf("create playback: %w", err)
	}
	if _, err := pb.Play(wav, "audio/wav"); err != nil {
		return fmt.Errorf("play audio: %w", err)
	}
	slog.Info("playback finished — hanging up")
	return nil
}

// --- DTMF over SIP INFO ---------------------------------------------------
// Mobinet/VoipSwitch sends DTMF as SIP INFO (application/dtmf-relay), which
// diago's server side answers with 406. We intercept INFO in a request
// middleware, parse the digit, and route it to the per-call channel that the
// forward handler waits on.

var dtmfHub = struct {
	mu sync.Mutex
	m  map[string]chan rune
}{m: make(map[string]chan rune)}

func dtmfRegister(callID string) chan rune {
	ch := make(chan rune, 8)
	dtmfHub.mu.Lock()
	dtmfHub.m[callID] = ch
	dtmfHub.mu.Unlock()
	return ch
}

func dtmfUnregister(callID string) {
	dtmfHub.mu.Lock()
	delete(dtmfHub.m, callID)
	dtmfHub.mu.Unlock()
}

func dtmfDeliver(callID string, d rune) {
	dtmfHub.mu.Lock()
	ch := dtmfHub.m[callID]
	dtmfHub.mu.Unlock()
	if ch != nil {
		select {
		case ch <- d:
		default:
		}
	}
}

// dtmfInfoMiddleware handles DTMF delivered as SIP INFO and passes every other
// request straight through to diago.
func dtmfInfoMiddleware(next sipgo.RequestHandler) sipgo.RequestHandler {
	return func(req *sip.Request, tx sip.ServerTransaction) {
		if req.Method == sip.INFO {
			if d, ok := parseInfoDTMF(req); ok {
				slog.Info("DTMF received (SIP INFO)", "digit", string(d))
				dtmfDeliver(req.CallID().Value(), d)
				_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
				return
			}
		}
		next(req, tx)
	}
}

// parseInfoDTMF extracts a DTMF digit from a SIP INFO body — either
// application/dtmf-relay ("Signal=1\r\nDuration=...") or application/dtmf ("1").
func parseInfoDTMF(req *sip.Request) (rune, bool) {
	ct := ""
	if h := req.ContentType(); h != nil {
		ct = strings.ToLower(h.Value())
	}
	body := strings.TrimSpace(string(req.Body()))
	if body == "" {
		return 0, false
	}
	if strings.Contains(ct, "dtmf-relay") {
		for _, line := range strings.Split(body, "\n") {
			if v, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(line)), "signal="); ok {
				if v = strings.TrimSpace(v); v != "" {
					return rune(v[0]), true
				}
			}
		}
		return 0, false
	}
	if strings.Contains(ct, "dtmf") {
		return rune(body[0]), true
	}
	return 0, false
}

// handleInbound answers an incoming call. With FORWARD_TO set it waits for the
// caller to press 1 and then bridges them to that number (call forwarding);
// otherwise it plays a prompt and echoes the caller's audio back.
func handleInbound(dg *diago.Diago, cfg config, in *diago.DialogServerSession) {
	slog.Info("incoming call", "from", in.InviteRequest.From().Value(), "dialog", in.ID)

	in.Trying()  // 100 Trying
	in.Ringing() // 180 Ringing
	if err := in.Answer(); err != nil {
		slog.Error("answer failed", "error", err)
		return
	}

	if cfg.ForwardTo == "" {
		slog.Info("ANSWERED ✓ — no FORWARD_TO set; playing prompt then echo")
		playPromptAndEcho(in, cfg.PromptFile)
		return
	}

	slog.Info("ANSWERED ✓ — playing prompt, waiting for caller to press 1", "forward_to", cfg.ForwardTo)

	// Mobinet delivers DTMF as SIP INFO; the middleware feeds digits to this channel.
	callID := in.InviteRequest.CallID().Value()
	digits := dtmfRegister(callID)
	defer dtmfUnregister(callID)

	// Play the prompt on a loop to cue the caller and keep the call's media alive
	// while we wait. Stop it before doing anything else with the media.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); loopPlay(in, cfg.PromptFile, stop) }()

	matched := waitForForwardDigit(in, digits, '1', 20*time.Second)
	close(stop)
	<-done

	if !matched {
		slog.Info("no '1' pressed within timeout — hanging up")
		return
	}

	// Cap concurrent forwards; hold extra callers in a FIFO queue until a slot frees.
	if !acquireForwardSlot(in) {
		slog.Info("queue timed out or caller left before a slot was free", "dialog", in.ID)
		return
	}
	defer func() { <-forward.sem }()

	if err := forwardCall(dg, cfg, in); err != nil {
		slog.Error("forward failed", "error", err)
	}
}

// openAudio opens a WAV clip: a file on disk if the path exists, else a bundled
// clip by that name, else a guaranteed bundled fallback so callers never hear
// pure silence. The caller must Close the returned reader.
func openAudio(name string) (io.ReadCloser, error) {
	if f, err := os.Open(name); err == nil {
		return f, nil
	}
	if f, err := testdata.OpenFile(name); err == nil {
		return f, nil
	}
	slog.Warn("audio file not found — using bundled fallback clip", "file", name)
	f, err := testdata.OpenFile("demo-echotest.wav")
	return f, err
}

// audioSource reports where a clip will load from, for the startup log.
func audioSource(name string) string {
	if _, err := os.Stat(name); err == nil {
		return "disk"
	}
	if f, err := testdata.OpenFile(name); err == nil {
		_ = f.Close()
		return "bundled"
	}
	return "MISSING(fallback)"
}

// loopPlay plays a WAV clip repeatedly until stop is closed, keeping outbound RTP
// flowing (NAT latch / hold music) and giving the caller something to hear.
func loopPlay(in *diago.DialogServerSession, file string, stop <-chan struct{}) {
	pb, err := in.PlaybackCreate()
	if err != nil {
		slog.Warn("create playback", "error", err)
		return
	}
	for {
		select {
		case <-stop:
			return
		default:
		}
		f, err := openAudio(file)
		if err != nil {
			slog.Warn("open audio", "file", file, "error", err)
			return
		}
		_, err = pb.Play(&stoppableReader{r: f, stop: stop}, "audio/wav")
		f.Close()
		if err != nil {
			// e.g. the WAV isn't 8 kHz mono 16-bit — stop rather than busy-loop.
			slog.Warn("play audio (must be 8kHz mono 16-bit WAV)", "file", file, "error", err)
			return
		}
	}
}

// stoppableReader makes a blocking Play return (EOF) as soon as stop is closed.
type stoppableReader struct {
	r    io.Reader
	stop <-chan struct{}
}

func (s *stoppableReader) Read(p []byte) (int, error) {
	select {
	case <-s.stop:
		return 0, io.EOF
	default:
		return s.r.Read(p)
	}
}

// waitForForwardDigit waits for `want` on the per-call DTMF channel (fed by the
// SIP INFO middleware), returning false on timeout or if the caller hangs up.
func waitForForwardDigit(in *diago.DialogServerSession, digits <-chan rune, want rune, timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case d := <-digits:
			if d == want {
				return true
			}
		case <-t.C:
			return false
		case <-in.Context().Done():
			return false
		}
	}
}

// forward limits concurrent forwards and holds extra callers in a FIFO queue.
// A buffered slot in sem == one active forward; Go wakes blocked senders in order.
var forward struct {
	sem            chan struct{}
	timeout        time.Duration
	holdFile       string
	connectingFile string
}

// acquireForwardSlot takes a forward slot. If all are busy it holds the caller
// with audio and waits (FIFO) for one to free, returning false if the caller
// hangs up or the queue wait exceeds QueueTimeout. Release with `<-forward.sem`.
func acquireForwardSlot(in *diago.DialogServerSession) bool {
	select {
	case forward.sem <- struct{}{}:
		return true // a slot was immediately free
	default:
	}

	slog.Info("forward slots full — holding caller in queue",
		"dialog", in.ID, "active", len(forward.sem), "capacity", cap(forward.sem))

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); loopPlay(in, forward.holdFile, stop) }()
	defer func() { close(stop); <-done }()

	t := time.NewTimer(forward.timeout)
	defer t.Stop()
	select {
	case forward.sem <- struct{}{}:
		slog.Info("slot freed — connecting queued caller", "dialog", in.ID)
		return true
	case <-t.C:
		return false
	case <-in.Context().Done():
		return false
	}
}

// forwardCall connects the (already answered) caller to cfg.ForwardTo as a
// back-to-back user agent: it plays a "connecting…" announcement to the caller
// while the outbound leg rings, then bridges the two answered legs so audio flows.
func forwardCall(dg *diago.Diago, cfg config, in *diago.DialogServerSession) error {
	dest := fmt.Sprintf("sip:%s@%s", cfg.ForwardTo, cfg.Domain)
	var recipient sip.Uri
	if err := sip.ParseUri(dest, &recipient); err != nil {
		return fmt.Errorf("parse forward target %q: %w", dest, err)
	}
	slog.Info("forwarding", "to", dest)

	inCtx := in.Context()
	dialCtx, cancel := context.WithTimeout(inCtx, 60*time.Second)
	defer cancel()

	// Play "connecting…" to the caller while the far end rings (so they don't hear
	// silence). Stopped before bridging so we never double-write the caller's media.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); loopPlay(in, forward.connectingFile, stop) }()

	// Present our own account as caller-id so VoipSwitch authorizes the leg, and
	// pass the caller as Originator so the outbound negotiates the SAME codec
	// (the bridge can't transcode).
	from := &sip.FromHeader{
		DisplayName: cfg.User,
		Address:     sip.Uri{Scheme: "sip", User: cfg.User, Host: cfg.Domain},
		Params:      sip.NewParams(),
	}
	out, err := dg.Invite(dialCtx, recipient, diago.InviteOptions{
		Originator: in,
		Username:   cfg.AuthUser,
		Password:   cfg.Pass,
		Headers:    []sip.Header{from},
		OnResponse: func(res *sip.Response) error {
			slog.Info("forward leg", "response", res.StartLine())
			return nil
		},
	})
	close(stop)
	<-done // stop the announcement before media bridging begins
	if err != nil {
		return fmt.Errorf("outbound leg to %s failed: %w", cfg.ForwardTo, err)
	}
	defer out.Close()
	slog.Info("BRIDGED ✓ — caller connected to forward target", "to", cfg.ForwardTo)

	// Bridge the two answered legs (relay starts when the second is added).
	bridge := diago.NewBridge()
	if err := bridge.AddDialogSession(in); err != nil {
		return fmt.Errorf("bridge add caller: %w", err)
	}
	if err := bridge.AddDialogSession(out); err != nil {
		return fmt.Errorf("bridge add target: %w", err)
	}

	outCtx := out.Context()
	defer in.Hangup(inCtx)
	defer out.Hangup(outCtx)
	select {
	case <-inCtx.Done():
		slog.Info("caller hung up")
	case <-outCtx.Done():
		slog.Info("forward target hung up")
	}
	return nil
}

// playPromptAndEcho plays the echo-test prompt, then echoes the caller's audio
// back until they hang up (the fallback when no FORWARD_TO is configured).
func playPromptAndEcho(in *diago.DialogServerSession, file string) {
	if wav, err := openAudio(file); err != nil {
		slog.Warn("open prompt", "error", err)
	} else {
		defer wav.Close()
		if pb, err := in.PlaybackCreate(); err != nil {
			slog.Warn("create playback", "error", err)
		} else if _, err := pb.Play(wav, "audio/wav"); err != nil {
			slog.Warn("play prompt", "error", err)
		}
	}

	r, err := in.AudioReader()
	if err != nil {
		slog.Error("audio reader", "error", err)
		return
	}
	w, err := in.AudioWriter()
	if err != nil {
		slog.Error("audio writer", "error", err)
		return
	}
	if _, err := media.Copy(r, w); err != nil && !errors.Is(err, io.EOF) {
		slog.Warn("echo ended", "error", err)
	}
	slog.Info("inbound call finished", "dialog", in.ID)
}

// newSIPParser returns a SIP parser that normalizes the digest auth scheme.
//
// Mobinet's platform (VoipSwitch) answers with "WWW-Authenticate: DIGEST ..."
// using an uppercase scheme token. sipgo's digest library only accepts the
// canonical "Digest" and rejects anything else, so registration fails before
// auth is even computed. We register parsers for the two auth headers that
// rewrite the leading token to "Digest" at parse time, leaving the rest of the
// registration flow (handled by diago) unchanged.
func newSIPParser() *sip.Parser {
	def := sip.DefaultHeadersParser()
	parsers := make(map[string]sip.HeaderParser, len(def)+2)
	for name, p := range def {
		parsers[name] = p
	}
	parsers["www-authenticate"] = normalizeDigestScheme("WWW-Authenticate")
	parsers["proxy-authenticate"] = normalizeDigestScheme("Proxy-Authenticate")
	return sip.NewParser(sip.WithHeadersParsers(parsers))
}

// normalizeDigestScheme rewrites a leading auth scheme token of any case (e.g.
// "DIGEST ") to the canonical "Digest " and stores the value under canonicalName.
// It returns (header, nil) deliberately: returning an error would trigger
// sipgo's comma-splitting path and shred the comma-separated challenge.
func normalizeDigestScheme(canonicalName string) sip.HeaderParser {
	return func(_ []byte, data string) (sip.Header, error) {
		const canon = "Digest "
		if len(data) >= len(canon) && strings.EqualFold(data[:len(canon)], canon) {
			data = canon + data[len(canon):]
		}
		return sip.NewHeader(canonicalName, data), nil
	}
}

// resolveServer turns a SIP domain into a concrete host:port to send packets to.
func resolveServer(domain string, port int) (string, error) {
	ips, err := net.LookupHost(domain)
	if err != nil {
		return "", err
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("no A/AAAA records for %s", domain)
	}
	return net.JoinHostPort(ips[0], strconv.Itoa(port)), nil
}

// stunMagicCookie is the fixed value all STUN messages carry (RFC 5389).
const stunMagicCookie = 0x2112A442

// discoverPublicAddr performs a STUN Binding request and returns the public
// (NAT-mapped) IP the server saw. Only the IP is used by callers — it is reliable
// on any NAT type, whereas the mapped port is per-destination on symmetric NAT and
// not reused for SIP, so inbound relies on the carrier's NAT handling for ports.
func discoverPublicAddr(stunServer string) (net.IP, int, error) {
	raddr, err := net.ResolveUDPAddr("udp4", stunServer)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve STUN server %q: %w", stunServer, err)
	}
	conn, err := net.DialUDP("udp4", nil, raddr) // ephemeral local port
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()

	// Binding Request: type=0x0001, length=0, magic cookie, random 12-byte txn ID.
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:], 0x0001)
	binary.BigEndian.PutUint32(req[4:], stunMagicCookie)
	if _, err := rand.Read(req[8:20]); err != nil {
		return nil, 0, err
	}

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(req); err != nil {
		return nil, 0, err
	}
	resp := make([]byte, 512)
	n, err := conn.Read(resp)
	if err != nil {
		return nil, 0, fmt.Errorf("no STUN reply: %w", err)
	}
	return parseSTUNMappedAddr(resp[:n])
}

// parseSTUNMappedAddr extracts the (XOR-)MAPPED-ADDRESS from a STUN response.
func parseSTUNMappedAddr(msg []byte) (net.IP, int, error) {
	if len(msg) < 20 {
		return nil, 0, errors.New("short STUN response")
	}
	attrs := msg[20:]
	for len(attrs) >= 4 {
		atype := binary.BigEndian.Uint16(attrs[0:])
		alen := int(binary.BigEndian.Uint16(attrs[2:]))
		if 4+alen > len(attrs) {
			break
		}
		val := attrs[4 : 4+alen]
		// 0x0020 XOR-MAPPED-ADDRESS (preferred), 0x0001 MAPPED-ADDRESS (legacy).
		if (atype == 0x0020 || atype == 0x0001) && len(val) >= 8 && val[1] == 0x01 {
			port := binary.BigEndian.Uint16(val[2:])
			ip := net.IP(append([]byte(nil), val[4:8]...))
			if atype == 0x0020 { // XOR-decode against the magic cookie
				port ^= uint16(stunMagicCookie >> 16)
				var cookie [4]byte
				binary.BigEndian.PutUint32(cookie[:], stunMagicCookie)
				for i := range ip {
					ip[i] ^= cookie[i]
				}
			}
			return ip, int(port), nil
		}
		adv := 4 + alen
		if pad := alen % 4; pad != 0 { // attributes are padded to 4 bytes
			adv += 4 - pad
		}
		attrs = attrs[adv:]
	}
	return nil, 0, errors.New("no mapped address in STUN response")
}

// outboundIP reports the local IP the kernel would use to reach target.
// No packets are sent — connecting a UDP socket just resolves the route.
func outboundIP(target string) (string, error) {
	conn, err := net.Dial("udp", target)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return "", err
	}
	return host, nil
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// loadDotEnv loads KEY=VALUE lines from path into the process environment without
// overriding variables that are already set. Blank lines and #-comment lines are
// ignored, an inline "# comment" after a value is stripped, and a quoted value is
// taken literally (including any '#').
func loadDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, parseEnvValue(raw))
		}
	}
	return nil
}

// parseEnvValue trims a .env value: a quoted value is returned literally; an
// unquoted value has any inline comment (a '#' at the start or after whitespace)
// stripped. A '#' that is part of the value (e.g. inside a password) is kept.
func parseEnvValue(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	if v[0] == '"' || v[0] == '\'' {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : 1+end]
		}
		return v[1:]
	}
	if v[0] == '#' {
		return ""
	}
	for i := 1; i < len(v); i++ {
		if v[i] == '#' && (v[i-1] == ' ' || v[i-1] == '\t') {
			return strings.TrimSpace(v[:i])
		}
	}
	return v
}
