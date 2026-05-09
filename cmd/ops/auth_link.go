package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"
)

const (
	// defaultCallbackPort is the local HTTP port the CLI listens on for the
	// Tesla OAuth redirect. This port must be registered as an allowed
	// redirect URI on the Tesla Fleet API app: http://localhost:<port>/callback.
	defaultCallbackPort = 8765

	// defaultOAuthTimeout bounds how long the CLI waits for the user to
	// complete the browser flow before giving up.
	defaultOAuthTimeout = 2 * time.Minute

	// teslaOAuthAuthorizeURL is Tesla's OAuth2 authorize endpoint.
	teslaOAuthAuthorizeURL = "https://auth.tesla.com/oauth2/v3/authorize"

	// teslaOAuthTokenURL is Tesla's OAuth2 token endpoint (same one used
	// by refresh_token grant).
	teslaOAuthTokenURL = "https://auth.tesla.com/oauth2/v3/token" //#nosec G101 -- public OAuth endpoint URL, not a credential

	// defaultTeslaOAuthScopes is the Fleet API scope set needed by the
	// ops CLI (read telemetry + push fleet config commands).
	defaultTeslaOAuthScopes = "openid offline_access vehicle_device_data vehicle_cmds vehicle_charging_cmds"
)

// authLinkOutput is the JSON shape printed on success.
type authLinkOutput struct {
	UserID    string `json:"userId"`
	ExpiresAt string `json:"expiresAt"`
	Message   string `json:"message"`
}

// runAuthLink drives the full Tesla OAuth2 authorization_code + PKCE flow:
// spins up a localhost callback server, opens the Tesla authorize URL in
// the developer's browser, exchanges the returned code for fresh tokens,
// and persists them to the Account row.
func runAuthLink(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("auth link", flag.ContinueOnError)
	userID := fs.String("user-id", "", "MyRoboTaxi user id (Prisma cuid)")
	port := fs.Int("port", defaultCallbackPort, "local HTTP port for the OAuth callback (must match the redirect URI registered on the Tesla Fleet API app)")
	scopes := fs.String("scopes", defaultTeslaOAuthScopes, "space-separated OAuth scopes to request")
	timeout := fs.Duration("timeout", defaultOAuthTimeout, "how long to wait for the browser flow to complete")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireFlag("user-id", *userID); err != nil {
		return err
	}

	clientID := os.Getenv("AUTH_TESLA_ID")
	clientSecret := os.Getenv("AUTH_TESLA_SECRET")
	if clientID == "" || clientSecret == "" {
		return fmt.Errorf("AUTH_TESLA_ID and AUTH_TESLA_SECRET must be set to link a Tesla account")
	}

	logger := newLogger()
	db, err := openDB(ctx, logger)
	if err != nil {
		return err
	}
	defer db.Close()

	pkce, err := newPKCE()
	if err != nil {
		return fmt.Errorf("generate pkce: %w", err)
	}
	state, err := randomURLSafeString(24)
	if err != nil {
		return fmt.Errorf("generate state: %w", err)
	}

	redirectURI := fmt.Sprintf("http://localhost:%d/callback", *port)
	authorizeURL := buildAuthorizeURL(clientID, redirectURI, *scopes, state, pkce.challenge)

	code, err := runCallbackServer(ctx, logger, *port, state, authorizeURL, *timeout)
	if err != nil {
		return err
	}

	tok, err := exchangeCodeForToken(ctx, logger, clientID, clientSecret, redirectURI, code, pkce.verifier)
	if err != nil {
		return fmt.Errorf("exchange code for token: %w", err)
	}

	accountRepo, err := newAccountRepo(db)
	if err != nil {
		return err
	}
	expiresAt := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	if err := accountRepo.UpdateTeslaToken(ctx, *userID, tok.AccessToken, tok.RefreshToken, expiresAt.Unix()); err != nil {
		return fmt.Errorf("persist tesla token: %w", err)
	}

	logger.Info("tesla account linked",
		slog.String("user_id", *userID),
		slog.Time("expires_at", expiresAt),
	)

	return writeJSON(os.Stdout, authLinkOutput{
		UserID:    *userID,
		ExpiresAt: formatExpiry(expiresAt),
		Message:   "Tesla account linked — run `ops auth token` to verify",
	})
}

// runCallbackServer starts a one-shot HTTP server on 127.0.0.1:<port>,
// opens the authorize URL in the user's browser, and blocks until either
// the callback fires with a matching state+code, the context is cancelled,
// or the timeout elapses. Returns the authorization code on success.
func runCallbackServer(
	ctx context.Context,
	logger *slog.Logger,
	port int,
	expectedState, authorizeURL string,
	timeout time.Duration,
) (string, error) {
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", fmt.Errorf("listen on port %d (is it registered as a redirect URI on Tesla?): %w", port, err)
	}

	result := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", callbackHandler(expectedState, result))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "ops: waiting on /callback", http.StatusNotFound)
	})

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn("callback server error", slog.Any("error", err))
		}
	}()

	fmt.Fprintf(os.Stderr, "\nOpening Tesla login in your browser…\nIf nothing opens, visit this URL manually:\n\n  %s\n\nWaiting for callback on %s …\n\n", authorizeURL, listener.Addr())
	openBrowser(ctx, logger, authorizeURL)

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	select {
	case <-waitCtx.Done():
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("timed out after %s waiting for Tesla OAuth callback", timeout)
		}
		return "", fmt.Errorf("oauth flow cancelled: %w", waitCtx.Err())
	case r := <-result:
		if r.err != nil {
			return "", r.err
		}
		return r.code, nil
	}
}

// callbackResult carries the outcome of a single callback request back to
// the waiting runCallbackServer goroutine.
type callbackResult struct {
	code string
	err  error
}

// callbackHandler returns an http.HandlerFunc that validates the state
// parameter, surfaces Tesla-reported errors, and forwards the auth code
// through the result channel. The page the user sees reflects the outcome.
// Non-blocking sends protect against duplicate callbacks (double-click,
// refresh) holding handler goroutines open forever.
func callbackHandler(expectedState string, result chan<- callbackResult) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		if errCode := q.Get("error"); errCode != "" {
			msg := fmt.Sprintf("Tesla rejected the authorization: %s — %s", errCode, q.Get("error_description"))
			respondCallback(w, http.StatusBadRequest, "OAuth failed", msg)
			trySendResult(result, callbackResult{err: fmt.Errorf("tesla oauth error: %s: %s", errCode, q.Get("error_description"))})
			return
		}

		if gotState := q.Get("state"); gotState != expectedState {
			respondCallback(w, http.StatusBadRequest, "State mismatch", "The OAuth state parameter did not match. Possible CSRF — retry the command.")
			trySendResult(result, callbackResult{err: errors.New("oauth state mismatch")})
			return
		}

		code := q.Get("code")
		if code == "" {
			respondCallback(w, http.StatusBadRequest, "Missing code", "Tesla did not return an authorization code.")
			trySendResult(result, callbackResult{err: errors.New("tesla returned no authorization code")})
			return
		}

		respondCallback(w, http.StatusOK, "Tesla account linked", "You can close this tab and return to the terminal.")
		trySendResult(result, callbackResult{code: code})
	}
}

// trySendResult forwards r to the buffered channel without blocking. A
// second concurrent callback (e.g. browser retry) is silently dropped
// rather than holding its handler goroutine open.
func trySendResult(ch chan<- callbackResult, r callbackResult) {
	select {
	case ch <- r:
	default:
	}
}

// respondCallback writes a tiny HTML page confirming the outcome to the
// user's browser. title/body are HTML-escaped before interpolation.
func respondCallback(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	safeTitle := html.EscapeString(title)
	safeBody := html.EscapeString(body)
	fmt.Fprintf(w,
		`<!doctype html><html><head><title>%[1]s</title><meta charset="utf-8"><style>body{font:16px/1.4 system-ui,sans-serif;max-width:480px;margin:48px auto;padding:0 16px;color:#111}h1{font-size:20px}</style></head><body><h1>%[1]s</h1><p>%[2]s</p></body></html>`,
		safeTitle, safeBody,
	)
}

// openBrowser tries to open browserURL in the user's default browser using
// the platform-appropriate command. On failure it logs a warning; the
// user is already told to open the URL manually in the stderr banner.
//
// The URL passed here is the authorize URL built by buildAuthorizeURL
// from static constants plus CLI flags and env vars — not arbitrary
// user input — so gosec G204 (subprocess launched with variable) is
// suppressed on each exec call.
func openBrowser(ctx context.Context, logger *slog.Logger, browserURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", browserURL) //#nosec G204 -- browserURL is the CLI-built Tesla authorize URL
	case "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", browserURL) //#nosec G204 -- browserURL is the CLI-built Tesla authorize URL
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", browserURL) //#nosec G204 -- browserURL is the CLI-built Tesla authorize URL
	default:
		logger.Warn("unsupported platform for auto-open — open the URL manually",
			slog.String("platform", runtime.GOOS),
		)
		return
	}
	if err := cmd.Start(); err != nil {
		logger.Warn("failed to auto-open browser", slog.Any("error", err))
	}
}
