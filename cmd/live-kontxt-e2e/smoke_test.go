/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aramase/kontxt/pkg/authn"
	"github.com/aramase/kontxt/pkg/keys"
	kontxttoken "github.com/aramase/kontxt/pkg/token"
	pkgtts "github.com/aramase/kontxt/pkg/tts"
	sdkverify "github.com/aramase/kontxt/sdk/verify"
)

func TestSmokeAgainstKontxt(t *testing.T) {
	fixture := newSmokeFixture(t, "")
	client := newSmokeClient(fixture.opts)
	ctx, cancel := context.WithTimeout(context.Background(), fixture.opts.timeout)
	defer cancel()
	if err := client.run(ctx, fixture.subject); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	for _, check := range []string{"access_token", "replacement", "unauthenticated", "allowed-list", "missing-scope", "wrong-namespace", "downstream", "create", "poll", "delete", "absent"} {
		if fixture.observed[check] == 0 {
			t.Errorf("missing smoke check %s", check)
		}
	}
	if fixture.exists {
		t.Fatal("test Task still exists after cleanup")
	}
}

func TestSmokeFailuresAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		fault       string
		wantError   string
		wantCleanup bool
	}{
		{"tts-error", "root token exchange", false},
		{"replacement-identity", "replacement token changed", false},
		{"broaden-allowed", "scope broadening rejection", false},
		{"downstream-identity", "downstream verification", false},
		{"list-tctx-leak", "private context", false},
		{"list-rctx-leak", "private context", false},
		{"create-conflict", "test Task create", false},
		{"create-disconnect", "test Task create", true},
		{"create-disconnect-absent", "test Task create", false},
		{"create-disconnect-identity", "test Task cleanup", false},
		{"create-server-error", "test Task create", true},
		{"create-json", "Task response is invalid JSON", true},
		{"create-leak", "Task JSON contains a raw credential", true},
		{"get-leak", "Task JSON contains a raw credential", true},
		{"create-tctx-leak", "private context", true},
		{"create-rctx-leak", "private context", true},
		{"get-tctx-leak", "private context", true},
		{"get-rctx-leak", "private context", true},
		{"cleanup-tctx-leak", "private context", true},
		{"cleanup-rctx-leak", "private context", true},
		{"requester-identity", "requestedBy does not match", true},
		{"transaction-identity", "transaction metadata does not match", true},
		{"task-failed", "test Task failed", true},
		{"poll-timeout", "context deadline exceeded", true},
		{"cleanup-error", "test Task cleanup", true},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			fixture := newSmokeFixture(t, tc.fault)
			client := newSmokeClient(fixture.opts)
			ctx, cancel := context.WithTimeout(context.Background(), fixture.opts.timeout)
			defer cancel()
			err := client.run(ctx, fixture.subject)
			if err == nil {
				t.Fatal("smoke unexpectedly passed")
			}
			for _, secret := range append(client.secrets, testPrivateContext) {
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Fatal("smoke error exposed a credential or private response data")
				}
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error did not identify the failed check: %v", err)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if got := fixture.observed["delete"] > 0; got != tc.wantCleanup {
				t.Fatalf("cleanup attempted = %t, want %t", got, tc.wantCleanup)
			}
			if tc.wantCleanup && tc.fault != "cleanup-error" && fixture.exists {
				t.Fatal("test Task still exists after failure cleanup")
			}
		})
	}
}

func TestSmokeExchangeRequiresStrictResponse(t *testing.T) {
	for _, body := range []string{
		`{"access_token":"` + testPrivateContext + `","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"N_A"}`,
		`{"access_token":"` + testPrivateContext + `","issued_token_type":"urn:ietf:params:oauth:token-type:txn_token","token_type":"Bearer"}`,
		`{"issued_token_type":"urn:ietf:params:oauth:token-type:txn_token","token_type":"N_A"}`,
		`{"access_token":"` + testPrivateContext + `","issued_token_type":"urn:ietf:params:oauth:token-type:txn_token","token_type":"N_A"}`,
		testPrivateContext,
	} {
		t.Run("invalid-response", func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			client := newSmokeClient(smokeOptions{ttsEndpoint: server.URL, jwksURL: server.URL, audience: "orka"})
			_, _, err := client.exchange(context.Background(), exchangeRequest{
				subject: "synthetic-subject-token", subjectType: kontxttoken.SubjectTokenTypeAccessToken, scope: smokeRootScope,
			}, http.StatusOK)
			if err == nil {
				t.Fatal("invalid token exchange response was accepted")
			}
			if strings.Contains(err.Error(), testPrivateContext) {
				t.Fatal("token exchange error exposed response data")
			}
		})
	}
}

func TestSmokeDoesNotForwardCredentialsOnRedirect(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := newSmokeClient(smokeOptions{})
	if _, err := client.request(context.Background(), http.MethodPost, server.URL,
		"synthetic-transaction-token", "application/x-www-form-urlencoded", []byte("subject_token=synthetic-subject-token"), http.StatusOK); err == nil {
		t.Fatal("redirect was accepted")
	}
	if redirected.Load() {
		t.Fatal("credentials were sent to a redirect target")
	}
}

func TestCheckTransaction(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile string
		txn     string
		args    []string
		wantErr bool
	}{
		{"matching", "transaction-token", "test-transaction", []string{"test-transaction"}, false},
		{"wrong-profile", "unknown-profile", "test-transaction", []string{"test-transaction"}, true},
		{"missing-id", "transaction-token", "", []string{"test-transaction"}, true},
		{"blank-id", "transaction-token", " \n", []string{"test-transaction"}, true},
		{"wrong-id", "transaction-token", "another-transaction", []string{"test-transaction"}, true},
		{"missing-expected-id", "transaction-token", "test-transaction", nil, true},
		{"empty-expected-id", "transaction-token", "test-transaction", []string{""}, true},
		{"blank-expected-id", "transaction-token", "test-transaction", []string{" \n"}, true},
		{"extra-arguments", "transaction-token", "test-transaction", []string{"test-transaction", testPrivateContext}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORKA_TRANSACTION_PROFILE", tc.profile)
			t.Setenv("ORKA_TRANSACTION_ID", tc.txn)
			if err := runCheckTransaction(tc.args); (err != nil) != tc.wantErr {
				t.Fatalf("check-transaction error = %v, want error %t", err, tc.wantErr)
			}
		})
	}
	t.Setenv("ORKA_TRANSACTION_PROFILE", "transaction-token")
	t.Setenv("ORKA_TRANSACTION_ID", "test-transaction")
	if err := runCheckTransaction([]string{testPrivateContext}); err == nil || strings.Contains(err.Error(), testPrivateContext) {
		t.Fatal("check-transaction should reject a mismatched ID without echoing it")
	}
}

func TestRunSmokeRequiresSubjectToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subject-token")
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runSmoke([]string{
		"--tts-endpoint", "http://127.0.0.1:1/token_endpoint", "--jwks-url", "http://127.0.0.1:1/jwks",
		"--orka-url", "http://127.0.0.1:1", "--subject-token-file", path, "--task-image", "helper:test",
	})
	if err == nil || !strings.Contains(err.Error(), "subject token file is empty") {
		t.Fatalf("empty subject token was not rejected: %v", err)
	}
}

func TestVerifyTokenOutputOmitsCredentialsAndContext(t *testing.T) {
	fixture := newSmokeFixture(t, "")
	client := newSmokeClient(fixture.opts)
	token, _, err := client.exchange(context.Background(), exchangeRequest{
		subject: fixture.subject, subjectType: kontxttoken.SubjectTokenTypeAccessToken, scope: smokeRootScope,
		details: map[string]any{"private": testPrivateContext}, requesterContext: map[string]any{"private": testPrivateContext},
	}, http.StatusOK)
	if err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(t.TempDir(), "transaction-token")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(t.TempDir(), "verify-output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	originalStdout := os.Stdout
	os.Stdout = output
	defer func() { os.Stdout = originalStdout }()
	if err := runVerifyToken([]string{"--token-file", tokenPath, "--jwks-url", fixture.opts.jwksURL, "--audience", fixture.opts.audience}); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{token, fixture.subject, testPrivateContext, `"tctx"`, `"rctx"`} {
		if strings.Contains(string(data), forbidden) {
			t.Fatal("verify-token output exposed credentials or context")
		}
	}
	var summary struct {
		Verified bool `json:"verified"`
	}
	if json.Unmarshal(data, &summary) != nil || !summary.Verified {
		t.Fatal("verify-token did not report successful verification")
	}
}

const testPrivateContext = "private-context-must-never-appear-in-output"

type smokeFixture struct {
	opts     smokeOptions
	subject  string
	mu       sync.Mutex
	observed map[string]int
	exists   bool
	task     smokeTask
}

// The token service is Kontxt itself. Only the external subject authenticator and
// Orka's HTTP API are fixtures, so token signing, replacement, and SDK verification
// exercise the pinned Kontxt version rather than a reimplementation.
func newSmokeFixture(t *testing.T, fault string) *smokeFixture {
	t.Helper()
	keyManager, err := keys.NewManager(2048, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	fixture := &smokeFixture{
		opts: smokeOptions{
			ttsEndpoint: server.URL + "/token_endpoint", jwksURL: server.URL + "/.well-known/jwks.json",
			audience: "orka", orkaURL: server.URL, namespace: "orka-system", taskImage: "helper:test",
			downstreamURL: server.URL + "/verify", timeout: 5 * time.Second, pollInterval: time.Millisecond,
		},
		subject:  "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://subject.example.test"}`)) + ".synthetic-signature",
		observed: map[string]int{},
	}
	if fault == "poll-timeout" {
		fixture.opts.timeout = 500 * time.Millisecond
	}
	verifier := sdkverify.New(fixture.opts.jwksURL, fixture.opts.audience)
	issuer := pkgtts.NewHandler(authn.NewRouter([]authn.Authenticator{fixtureAuthenticator{fixture.subject}}),
		keyManager, server.URL, fixture.opts.audience, time.Hour)
	issuer.SetVerifier(verifier)
	mux.Handle("/.well-known/jwks.json", keyManager.JWKSHandler())
	mux.HandleFunc("/token_endpoint", func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil {
			t.Error("TTS request was not form encoded")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.FormValue("audience") != fixture.opts.audience || r.Header.Get("Authorization") != "" {
			t.Error("TTS request used unexpected authentication or audience")
		}
		fixture.mu.Lock()
		if r.FormValue("subject_token_type") == kontxttoken.SubjectTokenTypeAccessToken {
			fixture.observed["access_token"]++
		} else if r.FormValue("subject_token_type") == kontxttoken.SubjectTokenTypeTxnToken {
			fixture.observed["replacement"]++
		} else {
			t.Error("TTS received an unexpected subject token type")
		}
		fixture.mu.Unlock()
		if fault == "tts-error" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, fixture.subject+testPrivateContext)
			return
		}
		if fault == "broaden-allowed" && r.FormValue("subject_token_type") == kontxttoken.SubjectTokenTypeTxnToken && r.FormValue("scope") == smokeRootScope {
			w.WriteHeader(http.StatusOK)
			return
		}
		if fault == "replacement-identity" && r.FormValue("subject_token_type") == kontxttoken.SubjectTokenTypeTxnToken {
			claims, err := verifier.Verify(r.Context(), r.FormValue("subject_token"))
			if err != nil {
				t.Error("fixture could not verify the parent token")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			claims.TransactionID = testPrivateContext
			claims.Scope = r.FormValue("scope")
			key, kid := keyManager.SigningKey()
			encoded, err := kontxttoken.New(*claims, key, kid, time.Hour)
			if err != nil {
				t.Error("fixture could not sign the replacement token")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(pkgtts.TokenExchangeResponse{
				AccessToken: encoded, IssuedTokenType: kontxttoken.RequestedTokenType, TokenType: "N_A",
			})
			return
		}
		issuer.ServeHTTP(w, r)
	})
	mux.HandleFunc("/verify", func(w http.ResponseWriter, r *http.Request) {
		claims, err := verifier.Verify(r.Context(), r.Header.Get(kontxttoken.HeaderName))
		if err != nil || r.Method != http.MethodPost || claims.Scope != smokeDownstreamScope {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fixture.mu.Lock()
		fixture.observed["downstream"]++
		fixture.mu.Unlock()
		if fault == "downstream-identity" {
			claims.TransactionID = testPrivateContext
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "txn": claims.TransactionID, "scope": claims.Scope})
	})
	handleTasks := func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		if r.Header.Get("Authorization") != "" || r.URL.Query().Get("namespace") != fixture.opts.namespace {
			t.Error("Orka request used unexpected authentication or namespace")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get(kontxttoken.HeaderName) == "" {
			fixture.observed["unauthenticated"]++
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		claims, err := verifier.Verify(r.Context(), r.Header.Get(kontxttoken.HeaderName))
		if err != nil {
			t.Error("Orka received an invalid transaction token")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if claims.TransactionContext["namespace"] != fixture.opts.namespace {
			fixture.observed["wrong-namespace"]++
			w.WriteHeader(http.StatusForbidden)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/")
		if r.URL.Path == "/api/v1/tasks" {
			name = ""
		}
		requiredScope := "orka:tasks:get"
		switch {
		case r.Method == http.MethodPost:
			requiredScope = "orka:tasks:create"
		case r.Method == http.MethodDelete:
			requiredScope = "orka:tasks:delete"
		case name == "":
			requiredScope = "orka:tasks:list"
		}
		if !slices.Contains(strings.Fields(claims.Scope), requiredScope) {
			fixture.observed["missing-scope"]++
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch {
		case r.Method == http.MethodGet && name == "":
			fixture.observed["allowed-list"]++
			if fault == "list-tctx-leak" || fault == "list-rctx-leak" {
				task := fixtureTask("previous-task", fixture.opts.namespace, claims)
				_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{fixtureTaskWithContext(task, claims, strings.Split(fault, "-")[1])}})
			} else {
				_, _ = io.WriteString(w, `{"items":[]}`)
			}
		case r.Method == http.MethodPost && name == "":
			var req struct {
				Name      string   `json:"name"`
				Namespace string   `json:"namespace"`
				Type      string   `json:"type"`
				Image     string   `json:"image"`
				Command   []string `json:"command"`
				Timeout   string   `json:"timeout"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if decoder.Decode(&req) != nil || req.Namespace != fixture.opts.namespace || req.Type != "container" ||
				req.Image != fixture.opts.taskImage || !slices.Equal(req.Command, []string{"/live-kontxt-e2e", "check-transaction", claims.TransactionID}) ||
				req.Timeout != fixture.opts.timeout.String() || !strings.HasPrefix(req.Name, "kontxt-smoke-") {
				t.Error("create request does not match Orka's public Task API")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if fault == "create-conflict" {
				w.WriteHeader(http.StatusConflict)
				return
			}
			fixture.observed["create"]++
			fixture.exists = fault != "create-disconnect-absent"
			fixture.task = fixtureTask(req.Name, fixture.opts.namespace, claims)
			if fault == "requester-identity" {
				fixture.task.Spec.RequestedBy.Subject = testPrivateContext
			}
			if fault == "transaction-identity" || fault == "create-disconnect-identity" {
				fixture.task.Spec.Transaction.ID = testPrivateContext
			}
			if strings.HasPrefix(fault, "create-disconnect") {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			if fault == "create-server-error" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			if fault == "create-json" {
				_, _ = io.WriteString(w, testPrivateContext)
			} else if fault == "create-leak" {
				_ = json.NewEncoder(w).Encode(map[string]any{"credential": fixture.subject})
			} else if fault == "create-tctx-leak" || fault == "create-rctx-leak" {
				_ = json.NewEncoder(w).Encode(fixtureTaskWithContext(fixture.task, claims, strings.Split(fault, "-")[1]))
			} else {
				_ = json.NewEncoder(w).Encode(fixture.task)
			}
		case r.Method == http.MethodGet && fixture.observed["delete"] > 0 && fixture.observed["cleanup-context"] == 0 &&
			(fault == "cleanup-tctx-leak" || fault == "cleanup-rctx-leak"):
			fixture.observed["cleanup-context"]++
			_ = json.NewEncoder(w).Encode(fixtureTaskWithContext(fixture.task, claims, strings.Split(fault, "-")[1]))
		case name != fixture.task.Metadata.Name || !fixture.exists:
			fixture.observed["absent"]++
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet:
			fixture.observed["poll"]++
			fixture.task.Status.Phase = "Running"
			if fault == "poll-timeout" {
				<-r.Context().Done()
				return
			}
			if fixture.observed["poll"] > 1 {
				fixture.task.Status.Phase = "Succeeded"
			}
			if fault == "task-failed" {
				fixture.task.Status.Phase = "Failed"
			}
			if fault == "get-leak" {
				_ = json.NewEncoder(w).Encode(map[string]any{"credential": r.Header.Get(kontxttoken.HeaderName)})
			} else if fault == "get-tctx-leak" || fault == "get-rctx-leak" {
				_ = json.NewEncoder(w).Encode(fixtureTaskWithContext(fixture.task, claims, strings.Split(fault, "-")[1]))
			} else {
				_ = json.NewEncoder(w).Encode(fixture.task)
			}
		case r.Method == http.MethodDelete:
			fixture.observed["delete"]++
			if fault == "cleanup-error" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, fixture.subject+testPrivateContext)
				return
			}
			fixture.exists = false
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Error("unexpected request to Orka fixture")
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
	mux.HandleFunc("/api/v1/tasks", handleTasks)
	mux.HandleFunc("/api/v1/tasks/", handleTasks)
	return fixture
}

func fixtureTaskWithContext(task smokeTask, claims *kontxttoken.Claims, field string) map[string]any {
	context := claims.TransactionContext
	if field == "rctx" {
		context = claims.RequesterContext
	}
	return map[string]any{
		"metadata": task.Metadata,
		"spec": map[string]any{
			"requestedBy": task.Spec.RequestedBy,
			"transaction": task.Spec.Transaction,
			field:         context,
		},
		"status": task.Status,
	}
}

func fixtureTask(name, namespace string, claims *kontxttoken.Claims) smokeTask {
	var task smokeTask
	task.Metadata.Name = name
	task.Metadata.Namespace = namespace
	task.Spec.RequestedBy.Subject = claims.Subject
	task.Spec.RequestedBy.Issuer = claims.Issuer
	task.Spec.RequestedBy.Roles = strings.Fields(claims.Scope)
	tx := &task.Spec.Transaction
	tx.Profile = "transaction-token"
	tx.ID = claims.TransactionID
	tx.Issuer = claims.Issuer
	tx.Subject = claims.Subject
	tx.Audience = []string{claims.Audience}
	tx.RequestingWorkload = claims.RequestingWorkload
	tx.Scope = claims.Scope
	tx.Scopes = strings.Fields(claims.Scope)
	tx.ContextDigest = smokeContextDigest(claims.TransactionContext)
	tx.RequesterContextDigest = smokeContextDigest(claims.RequesterContext)
	tx.Context = map[string]string{"namespace": namespace, "taskType": "container", "e2e": "kontxt-smoke"}
	task.Status.Phase = "Pending"
	return task
}

type fixtureAuthenticator struct{ subjectToken string }

func (fixtureAuthenticator) Matches(issuer string) bool {
	return issuer == "https://subject.example.test"
}

func (a fixtureAuthenticator) Authenticate(_ context.Context, token string) (*authn.SubjectInfo, error) {
	if token != a.subjectToken {
		return nil, errors.New("unknown test subject token")
	}
	return &authn.SubjectInfo{Subject: "system:serviceaccount:orka-system:smoke"}, nil
}
