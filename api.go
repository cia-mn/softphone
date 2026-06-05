// HTTP control API for the softphone, built with Chi + Huma v2 (OpenAPI 3.1).
// Interactive docs are served at /docs and the spec at /openapi.json.
package main

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

var startedAt = time.Now()

// apiState is the small bit of live state the API reports; the SIP side updates it.
var apiState struct {
	mu         sync.Mutex
	registered bool
	account    string
	server     string
}

func setRegistered(registered bool, account, server string) {
	apiState.mu.Lock()
	apiState.registered = registered
	apiState.account = account
	apiState.server = server
	apiState.mu.Unlock()
}

// startAPIServer runs the HTTP API until ctx is cancelled, then shuts down gracefully.
// Operations that declare Security require an "Authorization: Bearer <apiKey>" header.
func startAPIServer(ctx context.Context, addr, apiKey string) error {
	router := chi.NewMux()
	router.Use(middleware.Recoverer)

	config := huma.DefaultConfig("Softphone API", "0.1.0")
	config.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearer": {Type: "http", Scheme: "bearer", BearerFormat: "API key"},
	}
	api := humachi.New(router, config)

	// Enforce the API key on any operation that declares Security (e.g. /sms, /status).
	api.UseMiddleware(func(hctx huma.Context, next func(huma.Context)) {
		if len(hctx.Operation().Security) == 0 {
			next(hctx) // public endpoint (e.g. /health, docs)
			return
		}
		if apiKey == "" {
			_ = huma.WriteErr(api, hctx, http.StatusServiceUnavailable, "API authentication not configured (set API_KEY)")
			return
		}
		if !validBearer(hctx.Header("Authorization"), apiKey) {
			_ = huma.WriteErr(api, hctx, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next(hctx)
	})

	registerAPIRoutes(api)

	srv := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()

	slog.Info("HTTP API listening", "addr", addr, "docs", "http://"+addr+"/docs")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// validBearer reports whether the Authorization header carries the expected key.
func validBearer(header, key string) bool {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	got := strings.TrimSpace(header[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1
}

func registerAPIRoutes(api huma.API) {
	// secured marks an operation as requiring the bearer API key.
	secured := func(o *huma.Operation) {
		o.Security = []map[string][]string{{"bearer": {}}}
	}

	huma.Get(api, "/health", func(ctx context.Context, _ *struct{}) (*HealthOutput, error) {
		out := &HealthOutput{}
		out.Body.Status = "ok"
		return out, nil
	})

	huma.Get(api, "/status", func(ctx context.Context, _ *struct{}) (*StatusOutput, error) {
		apiState.mu.Lock()
		defer apiState.mu.Unlock()
		out := &StatusOutput{}
		out.Body.Registered = apiState.registered
		out.Body.Account = apiState.account
		out.Body.Server = apiState.server
		out.Body.UptimeSeconds = int64(time.Since(startedAt).Seconds())
		return out, nil
	}, secured)

	huma.Post(api, "/sms", func(ctx context.Context, in *SMSInput) (*SMSOutput, error) {
		if smsSend == nil {
			return nil, huma.Error503ServiceUnavailable("SMS not available (SIP not ready)")
		}
		status, response, err := smsSend(ctx, in.Body.To, in.Body.Text)
		if err != nil {
			return nil, huma.Error502BadGateway("failed to send SMS", err)
		}
		out := &SMSOutput{}
		out.Body.Status = status
		out.Body.Response = response
		out.Body.Accepted = status == 200
		return out, nil
	}, secured)
}

// SMSInput is the POST /sms request body.
type SMSInput struct {
	Body struct {
		To   string `json:"to" doc:"Destination number, as Mobinet expects it" example:"99112233" minLength:"3" maxLength:"20"`
		Text string `json:"text" doc:"Message text" example:"Hello from the softphone" minLength:"1" maxLength:"1000"`
	}
}

// SMSOutput reports what Mobinet's web portal replied.
type SMSOutput struct {
	Body struct {
		Accepted bool   `json:"accepted" doc:"True if the portal accepted the request (HTTP 200)"`
		Status   int    `json:"status" doc:"Portal HTTP status code"`
		Response string `json:"response" doc:"Portal response body (truncated)"`
	}
}

// HealthOutput is the liveness response.
type HealthOutput struct {
	Body struct {
		Status string `json:"status" example:"ok" doc:"Always \"ok\" when the service is up"`
	}
}

// StatusOutput reports the SIP registration state.
type StatusOutput struct {
	Body struct {
		Registered    bool   `json:"registered" doc:"Whether the SIP account is currently registered"`
		Account       string `json:"account,omitempty" doc:"SIP account (number) in use"`
		Server        string `json:"server,omitempty" doc:"SIP registrar address"`
		UptimeSeconds int64  `json:"uptime_seconds" doc:"Seconds since the service started"`
	}
}
