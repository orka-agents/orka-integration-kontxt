/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aramase/kontxt/pkg/authn"
	"github.com/aramase/kontxt/pkg/keys"
	kontxttoken "github.com/aramase/kontxt/pkg/token"
	pkgtts "github.com/aramase/kontxt/pkg/tts"
	sdkverify "github.com/aramase/kontxt/sdk/verify"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) < 2 {
		usageAndExit("missing subcommand")
	}

	var err error
	switch os.Args[1] {
	case "tts-server":
		err = runTTSServer(os.Args[2:])
	case "verify-token":
		err = runVerifyToken(os.Args[2:])
	case "downstream-verifier":
		err = runDownstreamVerifier(os.Args[2:])
	case "smoke":
		err = runSmoke(os.Args[2:])
	case "check-transaction":
		err = runCheckTransaction(os.Args[2:])
	default:
		usageAndExit(fmt.Sprintf("unknown subcommand %q", os.Args[1]))
	}
	if err != nil {
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}

func usageAndExit(msg string) {
	_, _ = fmt.Fprintf(
		os.Stderr,
		"%s\n\nusage: %s <tts-server|verify-token|downstream-verifier|smoke|check-transaction> [flags]\n",
		msg,
		os.Args[0],
	)
	os.Exit(2)
}

func runTTSServer(args []string) error {
	fs := flag.NewFlagSet("tts-server", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "address to listen on")
	issuer := fs.String("issuer", "", "TxToken issuer")
	trustDomain := fs.String("trust-domain", "", "TxToken audience/trust domain")
	subjectIssuer := fs.String("subject-issuer", "", "OIDC subject token issuer")
	subjectAudience := fs.String("subject-audience", "", "comma-separated OIDC subject token audiences")
	subjectDiscoveryURL := fs.String("subject-discovery-url", "", "optional OIDC discovery URL override")
	replacementJWKSURL := fs.String(
		"replacement-jwks-url",
		"",
		"JWKS URL used to verify TxToken replacement subject tokens",
	)
	tokenLifetime := fs.Duration("token-lifetime", 15*time.Minute, "issued TxToken lifetime")
	keySize := fs.Int("key-size", 2048, "RSA signing key size")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*issuer) == "" {
		return errors.New("--issuer is required")
	}
	if strings.TrimSpace(*trustDomain) == "" {
		return errors.New("--trust-domain is required")
	}
	if strings.TrimSpace(*subjectIssuer) == "" {
		return errors.New("--subject-issuer is required")
	}
	if strings.TrimSpace(*subjectAudience) == "" {
		return errors.New("--subject-audience is required")
	}
	if *tokenLifetime <= 0 {
		return errors.New("--token-lifetime must be positive")
	}

	authenticator, err := authn.NewOIDCAuthenticator(authn.AuthenticatorConfig{
		Issuer: authn.IssuerConfig{
			URL:                 strings.TrimSpace(*subjectIssuer),
			DiscoveryURL:        strings.TrimSpace(*subjectDiscoveryURL),
			Audiences:           splitCSV(*subjectAudience),
			AudienceMatchPolicy: "MatchAny",
		},
		ClaimMappings: authn.ClaimMappings{
			Subject: authn.ClaimOrExpression{Claim: "sub"},
		},
	})
	if err != nil {
		return fmt.Errorf("creating OIDC authenticator: %w", err)
	}

	keyManager, err := keys.NewManager(*keySize, *tokenLifetime)
	if err != nil {
		return fmt.Errorf("creating key manager: %w", err)
	}
	handler := pkgtts.NewHandler(
		authn.NewRouter([]authn.Authenticator{authenticator}),
		keyManager,
		strings.TrimSpace(*issuer),
		strings.TrimSpace(*trustDomain),
		*tokenLifetime,
	)
	if strings.TrimSpace(*replacementJWKSURL) != "" {
		handler.SetVerifier(sdkverify.New(strings.TrimSpace(*replacementJWKSURL), strings.TrimSpace(*trustDomain)))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/token_endpoint", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handler.ServeHTTP(w, r)
	})
	mux.Handle("/.well-known/jwks.json", keyManager.JWKSHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf(
		"kontxt TTS live e2e server listening on %s issuer=%s trustDomain=%s replacement=%t",
		*addr,
		*issuer,
		*trustDomain,
		strings.TrimSpace(*replacementJWKSURL) != "",
	)
	return serveHTTP(*addr, mux)
}

func runVerifyToken(args []string) error {
	fs := flag.NewFlagSet("verify-token", flag.ExitOnError)
	tokenValue := fs.String("token", "", "TxToken value")
	tokenFile := fs.String("token-file", "", "path to TxToken file")
	jwksURL := fs.String("jwks-url", "", "JWKS URL")
	audience := fs.String("audience", "", "expected TxToken audience")
	expectTxn := fs.String("expect-txn", "", "expected transaction ID")
	expectScope := fs.String("expect-scope", "", "expected scope")
	if err := fs.Parse(args); err != nil {
		return err
	}
	claims, err := verifyToken(*tokenValue, *tokenFile, *jwksURL, *audience)
	if err != nil {
		return err
	}
	if *expectTxn != "" && claims.TransactionID != *expectTxn {
		return errors.New("verified transaction ID did not match --expect-txn")
	}
	if *expectScope != "" && claims.Scope != *expectScope {
		return errors.New("verified scope did not match --expect-scope")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"verified": true,
		"txn":      claims.TransactionID,
		"scope":    claims.Scope,
	})
}

func runDownstreamVerifier(args []string) error {
	fs := flag.NewFlagSet("downstream-verifier", flag.ExitOnError)
	addr := fs.String("addr", ":8081", "address to listen on")
	jwksURL := fs.String("jwks-url", "", "JWKS URL")
	audience := fs.String("audience", "", "expected TxToken audience")
	expectTxn := fs.String("expect-txn", "", "expected transaction ID")
	expectScope := fs.String("expect-scope", "", "expected scope")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*jwksURL) == "" {
		return errors.New("--jwks-url is required")
	}
	if strings.TrimSpace(*audience) == "" {
		return errors.New("--audience is required")
	}
	verifier := sdkverify.New(strings.TrimSpace(*jwksURL), strings.TrimSpace(*audience))

	mux := http.NewServeMux()
	mux.HandleFunc("/verify", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		claims, err := verifier.Verify(r.Context(), r.Header.Get(kontxttoken.HeaderName))
		if err != nil {
			http.Error(w, "invalid TxToken", http.StatusUnauthorized)
			return
		}
		if *expectTxn != "" && claims.TransactionID != *expectTxn {
			http.Error(w, "unexpected transaction", http.StatusForbidden)
			return
		}
		if *expectScope != "" && claims.Scope != *expectScope {
			http.Error(w, "unexpected scope", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted": true,
			"txn":      claims.TransactionID,
			"scope":    claims.Scope,
		})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	log.Printf("kontxt downstream verifier listening on %s", *addr)
	return serveHTTP(*addr, mux)
}

func serveHTTP(addr string, handler http.Handler) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return server.ListenAndServe()
}

func verifyToken(tokenValue, tokenFile, jwksURL, audience string) (*kontxttoken.Claims, error) {
	tokenValue = strings.TrimSpace(tokenValue)
	if tokenValue == "" && strings.TrimSpace(tokenFile) != "" {
		data, err := os.ReadFile(strings.TrimSpace(tokenFile))
		if err != nil {
			return nil, fmt.Errorf("reading token file: %w", err)
		}
		tokenValue = strings.TrimSpace(string(data))
	}
	if tokenValue == "" {
		return nil, errors.New("--token or --token-file is required")
	}
	if strings.TrimSpace(jwksURL) == "" {
		return nil, errors.New("--jwks-url is required")
	}
	if strings.TrimSpace(audience) == "" {
		return nil, errors.New("--audience is required")
	}
	claims, err := sdkverify.New(
		strings.TrimSpace(jwksURL),
		strings.TrimSpace(audience),
	).Verify(context.Background(), tokenValue)
	if err != nil {
		return nil, errors.New("verifying TxToken failed")
	}
	return claims, nil
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
