package auth

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"
)

// OAuthServer handles the OAuth callback
type OAuthServer struct {
	port         int
	server       *http.Server
	tokenChannel chan string
}

// NewOAuthServer creates a new OAuth callback server
func NewOAuthServer(port int) *OAuthServer {
	return &OAuthServer{
		port:         port,
		tokenChannel: make(chan string, 1),
	}
}

// Start starts the OAuth callback server
func (s *OAuthServer) Start() error {
	mux := http.NewServeMux()

	// Callback endpoint
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if token == "" {
			http.Error(w, "Missing token", http.StatusBadRequest)
			return
		}

		// Send token to channel
		select {
		case s.tokenChannel <- token:
			// Success page
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`
<!DOCTYPE html>
<html>
<head>
	<title>PocketVibe Authentication</title>
	<style>
		body {
			font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
			display: flex;
			justify-content: center;
			align-items: center;
			height: 100vh;
			margin: 0;
			background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
		}
		.container {
			background: white;
			padding: 40px;
			border-radius: 12px;
			box-shadow: 0 10px 40px rgba(0,0,0,0.2);
			text-align: center;
			max-width: 400px;
		}
		h1 { color: #333; margin-bottom: 20px; }
		p { color: #666; margin-bottom: 30px; line-height: 1.6; }
		.checkmark {
			font-size: 64px;
			color: #4CAF50;
			margin-bottom: 20px;
		}
	</style>
</head>
<body>
	<div class="container">
		<div class="checkmark">✓</div>
		<h1>Authentication Successful!</h1>
		<p>You can now close this window and return to the desktop app.</p>
		<p style="font-size: 12px; color: #999;">PocketVibe daemon is now connected to your account.</p>
	</div>
</body>
</html>
			`))
		default:
			http.Error(w, "Token channel full", http.StatusInternalServerError)
		}
	})

	s.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}

	// Start server in background
	go func() {
		log.Printf("🔐 OAuth callback server listening on http://localhost:%d", s.port)
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("OAuth server error: %v", err)
		}
	}()

	return nil
}

// WaitForToken waits for the OAuth callback to receive a token
func (s *OAuthServer) WaitForToken(timeout time.Duration) (string, error) {
	select {
	case token := <-s.tokenChannel:
		return token, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("timeout waiting for OAuth callback")
	}
}

// Stop stops the OAuth callback server
func (s *OAuthServer) Stop() error {
	if s.server == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return s.server.Shutdown(ctx)
}
