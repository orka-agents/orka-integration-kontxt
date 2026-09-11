/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	kontxttoken "github.com/aramase/kontxt/pkg/token"
	pkgtts "github.com/aramase/kontxt/pkg/tts"
	sdkverify "github.com/aramase/kontxt/sdk/verify"
)

const (
	smokeRootScope       = "orka:tasks:create orka:tasks:get orka:tasks:list orka:tasks:delete orka:tools:http"
	smokeChildScope      = "orka:tasks:get orka:tools:http"
	smokeDownstreamScope = "orka:tools:http"
	maxSmokeResponseSize = 4 << 20
)

type smokeOptions struct {
	ttsEndpoint      string
	jwksURL          string
	audience         string
	orkaURL          string
	namespace        string
	subjectTokenFile string
	taskImage        string
	downstreamURL    string
	timeout          time.Duration
	pollInterval     time.Duration
}

func runSmoke(args []string) error {
	var opts smokeOptions
	fs := flag.NewFlagSet("smoke", flag.ExitOnError)
	fs.StringVar(&opts.ttsEndpoint, "tts-endpoint", "", "full TTS token endpoint URL")
	fs.StringVar(&opts.jwksURL, "jwks-url", "", "TTS JWKS URL")
	fs.StringVar(&opts.audience, "audience", "orka", "expected TxToken audience")
	fs.StringVar(&opts.orkaURL, "orka-url", "", "Orka base URL, without /api/v1")
	fs.StringVar(&opts.namespace, "namespace", "orka-system", "namespace for the test Task")
	fs.StringVar(&opts.subjectTokenFile, "subject-token-file", "", "projected ServiceAccount token file")
	fs.StringVar(&opts.taskImage, "task-image", "", "image containing /live-kontxt-e2e")
	fs.StringVar(&opts.downstreamURL, "downstream-url", "", "optional full downstream /verify URL")
	fs.DurationVar(&opts.timeout, "timeout", 5*time.Minute, "smoke timeout, excluding final Task cleanup")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("smoke accepts flags only")
	}
	if err := opts.validate(); err != nil {
		return err
	}
	subjectData, err := os.ReadFile(opts.subjectTokenFile)
	if err != nil {
		return errors.New("reading subject token file failed")
	}
	subject := strings.TrimSpace(string(subjectData))
	if subject == "" {
		return errors.New("subject token file is empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	client := newSmokeClient(opts)
	if err := client.run(ctx, subject); err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, "Orka/Kontxt smoke passed")
	return err
}

func (o smokeOptions) validate() error {
	for _, field := range []struct{ name, value string }{
		{"tts-endpoint", o.ttsEndpoint}, {"jwks-url", o.jwksURL},
		{"orka-url", o.orkaURL}, {"downstream-url", o.downstreamURL},
	} {
		if field.name == "downstream-url" && field.value == "" {
			continue
		}
		u, err := url.Parse(field.value)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("--%s must be an HTTP(S) URL without credentials, query, or fragment", field.name)
		}
	}
	for _, field := range []struct{ name, value string }{
		{"audience", o.audience}, {"namespace", o.namespace},
		{"subject-token-file", o.subjectTokenFile}, {"task-image", o.taskImage},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("--%s is required", field.name)
		}
	}
	if o.timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	return nil
}

func runCheckTransaction(args []string) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return errors.New("check-transaction requires the expected transaction ID")
	}
	if os.Getenv("ORKA_TRANSACTION_PROFILE") != "transaction-token" {
		return errors.New("ORKA_TRANSACTION_PROFILE must be transaction-token")
	}
	if os.Getenv("ORKA_TRANSACTION_ID") != args[0] {
		return errors.New("ORKA_TRANSACTION_ID does not match the issued transaction")
	}
	_, err := fmt.Fprintln(os.Stdout, "transaction metadata verified")
	return err
}

type smokeClient struct {
	opts     smokeOptions
	http     *http.Client
	verifier *sdkverify.Verifier
	secrets  []string
}

func newSmokeClient(opts smokeOptions) *smokeClient {
	if opts.pollInterval <= 0 {
		opts.pollInterval = time.Second
	}
	return &smokeClient{
		opts: opts,
		http: &http.Client{
			Timeout: 10 * time.Second,
			// Txn-Token is a custom header; the default redirect policy can forward it.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		verifier: sdkverify.New(opts.jwksURL, opts.audience),
	}
}

func (s *smokeClient) run(ctx context.Context, subject string) (runErr error) {
	// This synthetic value must be signed but never appear in Task responses.
	privateContext := "kontxt-smoke-private-" + rand.Text()
	s.secrets = append(s.secrets, subject, privateContext)
	details := map[string]any{"namespace": s.opts.namespace, "taskType": "container", "e2e": "kontxt-smoke", "private": privateContext}
	rctx := map[string]any{"source": "kontxt-smoke", "private": privateContext}
	root, rootClaims, err := s.exchange(ctx, exchangeRequest{
		subject: subject, subjectType: kontxttoken.SubjectTokenTypeAccessToken,
		scope: smokeRootScope, details: details, requesterContext: rctx,
	}, http.StatusOK)
	if err != nil {
		return fmt.Errorf("root token exchange: %w", err)
	}
	if !reflect.DeepEqual(rootClaims.TransactionContext, details) || !reflect.DeepEqual(rootClaims.RequesterContext, rctx) {
		return errors.New("root token exchange did not preserve the requested context")
	}
	listURL := s.taskURL("") + "&limit=1"
	if _, err := s.request(ctx, http.MethodGet, listURL, "", "", nil, http.StatusUnauthorized); err != nil {
		return fmt.Errorf("unauthenticated task list: %w", err)
	}
	if resp, err := s.request(ctx, http.MethodGet, listURL, root, "", nil, http.StatusOK); err != nil {
		return fmt.Errorf("authorized task list: %w", err)
	} else if err := s.checkRedaction(resp.body); err != nil {
		return err
	}
	child, childClaims, err := s.exchange(ctx, exchangeRequest{
		subject: root, subjectType: kontxttoken.SubjectTokenTypeTxnToken, scope: smokeChildScope,
	}, http.StatusOK)
	if err != nil {
		return fmt.Errorf("child token exchange: %w", err)
	}
	if err := checkReplacement(rootClaims, childClaims); err != nil {
		return fmt.Errorf("child token exchange: %w", err)
	}
	if _, err := s.request(ctx, http.MethodGet, listURL, child, "", nil, http.StatusForbidden); err != nil {
		return fmt.Errorf("missing list scope: %w", err)
	}
	wrongDetails := map[string]any{"namespace": s.opts.namespace + "-denied", "taskType": "container", "e2e": "kontxt-smoke", "private": privateContext}
	wrongNamespace, wrongClaims, err := s.exchange(ctx, exchangeRequest{
		subject: subject, subjectType: kontxttoken.SubjectTokenTypeAccessToken,
		scope: smokeRootScope, details: wrongDetails, requesterContext: rctx,
	}, http.StatusOK)
	if err != nil {
		return fmt.Errorf("namespace test token exchange: %w", err)
	}
	if !reflect.DeepEqual(wrongClaims.TransactionContext, wrongDetails) {
		return errors.New("namespace test token exchange did not preserve the requested context")
	}
	if _, err := s.request(ctx, http.MethodGet, listURL, wrongNamespace, "", nil, http.StatusForbidden); err != nil {
		return fmt.Errorf("mismatched namespace context: %w", err)
	}
	narrowed, narrowedClaims, err := s.exchange(ctx, exchangeRequest{
		subject: child, subjectType: kontxttoken.SubjectTokenTypeTxnToken, scope: smokeDownstreamScope,
	}, http.StatusOK)
	if err != nil {
		return fmt.Errorf("downstream token exchange: %w", err)
	}
	if err := checkReplacement(childClaims, narrowedClaims); err != nil {
		return fmt.Errorf("downstream token exchange: %w", err)
	}
	if _, _, err := s.exchange(ctx, exchangeRequest{
		subject: narrowed, subjectType: kontxttoken.SubjectTokenTypeTxnToken, scope: smokeRootScope,
	}, http.StatusForbidden); err != nil {
		return fmt.Errorf("scope broadening rejection: %w", err)
	}
	if s.opts.downstreamURL != "" {
		resp, err := s.request(ctx, http.MethodPost, s.opts.downstreamURL, narrowed, "", nil, http.StatusOK)
		if err != nil {
			return fmt.Errorf("downstream verification: %w", err)
		}
		var result struct {
			Accepted bool   `json:"accepted"`
			Txn      string `json:"txn"`
			Scope    string `json:"scope"`
		}
		if json.Unmarshal(resp.body, &result) != nil || !result.Accepted ||
			result.Txn != rootClaims.TransactionID || !sameScopes(result.Scope, smokeDownstreamScope) {
			return errors.New("downstream verification did not confirm the narrowed transaction")
		}
	}
	name := "kontxt-smoke-" + strings.ToLower(rand.Text()[:12])
	body, err := json.Marshal(map[string]any{
		"name": name, "namespace": s.opts.namespace, "type": "container", "image": s.opts.taskImage,
		"command": []string{"/live-kontxt-e2e", "check-transaction", rootClaims.TransactionID}, "timeout": s.opts.timeout.String(),
	})
	if err != nil {
		return errors.New("encoding the test Task failed")
	}
	created, err := s.request(ctx, http.MethodPost, s.taskURL(""), root, "application/json", body, http.StatusCreated)
	if created.status == http.StatusCreated || created.status == 0 || created.status >= http.StatusInternalServerError {
		// A lost response or server error may follow a committed Task. Cleanup
		// uses its own deadline even after the smoke context expires.
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := s.cleanupTask(cleanupCtx, name, root, rootClaims, created.status == http.StatusCreated); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("test Task cleanup: %w", err))
			}
		}()
	}
	if err != nil {
		return fmt.Errorf("test Task create: %w", err)
	}
	if _, err := s.checkTask(created.body, name, rootClaims); err != nil {
		return fmt.Errorf("created Task: %w", err)
	}
	for {
		resp, err := s.request(ctx, http.MethodGet, s.taskURL(name), root, "", nil, http.StatusOK)
		if err != nil {
			return fmt.Errorf("test Task poll: %w", err)
		}
		task, err := s.checkTask(resp.body, name, rootClaims)
		if err != nil {
			return fmt.Errorf("polled Task: %w", err)
		}
		switch task.Status.Phase {
		case "Succeeded":
			return nil
		case "Failed", "Cancelled":
			return errors.New("test Task failed or was cancelled")
		case "", "Pending", "Running", "Finalizing":
		default:
			return errors.New("test Task returned an unexpected phase")
		}
		if err := s.wait(ctx); err != nil {
			return fmt.Errorf("waiting for test Task: %w", err)
		}
	}
}

type exchangeRequest struct {
	subject          string
	subjectType      string
	scope            string
	details          map[string]any
	requesterContext map[string]any
}

func (s *smokeClient) exchange(ctx context.Context, input exchangeRequest, wantStatus int) (string, *kontxttoken.Claims, error) {
	form := url.Values{
		"grant_type": {kontxttoken.GrantType}, "requested_token_type": {kontxttoken.RequestedTokenType},
		"subject_token": {input.subject}, "subject_token_type": {input.subjectType},
		"scope": {input.scope}, "audience": {s.opts.audience},
	}
	for name, value := range map[string]map[string]any{"request_details": input.details, "request_context": input.requesterContext} {
		if value != nil {
			encoded, err := json.Marshal(value)
			if err != nil {
				return "", nil, errors.New("encoding token exchange context failed")
			}
			form.Set(name, string(encoded))
		}
	}
	resp, err := s.request(ctx, http.MethodPost, s.opts.ttsEndpoint, "", "application/x-www-form-urlencoded", []byte(form.Encode()), wantStatus)
	if err != nil {
		return "", nil, err
	}
	if wantStatus != http.StatusOK {
		return "", nil, nil
	}
	var exchanged pkgtts.TokenExchangeResponse
	if json.Unmarshal(resp.body, &exchanged) != nil {
		return "", nil, errors.New("TTS returned invalid JSON")
	}
	if exchanged.AccessToken == "" || exchanged.IssuedTokenType != kontxttoken.RequestedTokenType || exchanged.TokenType != "N_A" {
		return "", nil, errors.New("TTS response must contain an access_token with issued_token_type txn_token and token_type N_A")
	}
	s.secrets = append(s.secrets, exchanged.AccessToken)
	claims, err := s.verifier.Verify(ctx, exchanged.AccessToken)
	if err != nil {
		// SDK errors can include untrusted JWT headers or endpoint response data.
		return "", nil, errors.New("TTS returned an unverifiable transaction token")
	}
	if claims.Issuer == "" || claims.Subject == "" || claims.TransactionID == "" || claims.RequestingWorkload == "" ||
		claims.Audience != s.opts.audience || claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt ||
		!sameScopes(claims.Scope, input.scope) {
		return "", nil, errors.New("TTS returned incomplete claims or an unexpected scope")
	}
	return exchanged.AccessToken, claims, nil
}

func checkReplacement(parent, child *kontxttoken.Claims) error {
	if child.TransactionID != parent.TransactionID || child.Subject != parent.Subject ||
		child.Issuer != parent.Issuer || child.Audience != parent.Audience ||
		!reflect.DeepEqual(child.TransactionContext, parent.TransactionContext) ||
		!reflect.DeepEqual(child.RequesterContext, parent.RequesterContext) {
		return errors.New("replacement token changed the transaction identity or context")
	}
	for _, scope := range strings.Fields(child.Scope) {
		if !slices.Contains(strings.Fields(parent.Scope), scope) {
			return errors.New("replacement token broadened the parent scope")
		}
	}
	return nil
}

type smokeResponse struct {
	status int
	body   []byte
}

func (s *smokeClient) request(ctx context.Context, method, endpoint, credential, contentType string, body []byte, wantStatus ...int) (smokeResponse, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return smokeResponse{}, errors.New("constructing HTTP request failed")
	}
	if credential != "" {
		request.Header.Set(kontxttoken.HeaderName, credential)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	resp, err := s.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return smokeResponse{}, ctx.Err()
		}
		return smokeResponse{}, errors.New("HTTP request failed")
	}
	defer resp.Body.Close()
	out := smokeResponse{status: resp.StatusCode}
	if !slices.Contains(wantStatus, resp.StatusCode) {
		return out, fmt.Errorf("HTTP status %d, want %v", resp.StatusCode, wantStatus)
	}
	// Error bodies may echo credentials. Expected denial checks only need the status.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, nil
	}
	out.body, err = io.ReadAll(io.LimitReader(resp.Body, maxSmokeResponseSize+1))
	if err != nil {
		return out, errors.New("reading HTTP response failed")
	}
	if len(out.body) > maxSmokeResponseSize {
		return out, errors.New("HTTP response exceeded the size limit")
	}
	return out, nil
}

func (s *smokeClient) taskURL(name string) string {
	path := strings.TrimRight(s.opts.orkaURL, "/") + "/api/v1/tasks"
	if name != "" {
		path += "/" + url.PathEscape(name)
	}
	return path + "?" + url.Values{"namespace": {s.opts.namespace}}.Encode()
}

func (s *smokeClient) cleanupTask(ctx context.Context, name, credential string, claims *kontxttoken.Claims, createConfirmed bool) error {
	if !createConfirmed {
		resp, err := s.request(ctx, http.MethodGet, s.taskURL(name), credential, "", nil, http.StatusOK, http.StatusNotFound)
		if err != nil || resp.status == http.StatusNotFound {
			return err
		}
		// Confirm that an uncertain create belongs to this transaction before deleting it.
		if _, err := s.checkTask(resp.body, name, claims); err != nil {
			return fmt.Errorf("unconfirmed Task identity: %w", err)
		}
	}
	resp, err := s.request(ctx, http.MethodDelete, s.taskURL(name), credential, "", nil, http.StatusNoContent, http.StatusNotFound)
	if err != nil || resp.status == http.StatusNotFound {
		return err
	}
	for {
		resp, err := s.request(ctx, http.MethodGet, s.taskURL(name), credential, "", nil, http.StatusOK, http.StatusNotFound)
		if err != nil || resp.status == http.StatusNotFound {
			return err
		}
		if err := s.checkRedaction(resp.body); err != nil {
			return err
		}
		if err := s.wait(ctx); err != nil {
			return err
		}
	}
}

func (s *smokeClient) wait(ctx context.Context) error {
	timer := time.NewTimer(s.opts.pollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type smokeTask struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		RequestedBy struct {
			Subject string   `json:"subject"`
			Issuer  string   `json:"issuer"`
			Roles   []string `json:"roles"`
		} `json:"requestedBy"`
		Transaction struct {
			Profile                string            `json:"profile"`
			ID                     string            `json:"id"`
			Issuer                 string            `json:"issuer"`
			Subject                string            `json:"subject"`
			Audience               []string          `json:"audience"`
			RequestingWorkload     string            `json:"requestingWorkload"`
			Scope                  string            `json:"scope"`
			Scopes                 []string          `json:"scopes"`
			ContextDigest          string            `json:"contextDigest"`
			RequesterContextDigest string            `json:"requesterContextDigest"`
			Context                map[string]string `json:"context"`
		} `json:"transaction"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

func (s *smokeClient) checkTask(body []byte, name string, claims *kontxttoken.Claims) (*smokeTask, error) {
	if err := s.checkRedaction(body); err != nil {
		return nil, err
	}
	var task smokeTask
	if json.Unmarshal(body, &task) != nil {
		return nil, errors.New("Task response is invalid JSON")
	}
	if task.Metadata.Name != name || task.Metadata.Namespace != s.opts.namespace {
		return nil, errors.New("Task response has an unexpected name or namespace")
	}
	requester := task.Spec.RequestedBy
	if requester.Subject != claims.Subject || requester.Issuer != claims.Issuer ||
		!sameScopes(strings.Join(requester.Roles, " "), claims.Scope) {
		return nil, errors.New("Task requestedBy does not match the verified requester")
	}
	tx := task.Spec.Transaction
	if tx.Profile != "transaction-token" || tx.ID != claims.TransactionID ||
		tx.Issuer != claims.Issuer || tx.Subject != claims.Subject ||
		len(tx.Audience) != 1 || tx.Audience[0] != claims.Audience ||
		tx.RequestingWorkload != claims.RequestingWorkload || !sameScopes(tx.Scope, claims.Scope) ||
		!sameScopes(strings.Join(tx.Scopes, " "), claims.Scope) ||
		tx.ContextDigest != smokeContextDigest(claims.TransactionContext) ||
		tx.RequesterContextDigest != smokeContextDigest(claims.RequesterContext) ||
		tx.Context["namespace"] != s.opts.namespace || tx.Context["taskType"] != "container" {
		return nil, errors.New("Task transaction metadata does not match the verified token")
	}
	return &task, nil
}

func (s *smokeClient) checkRedaction(body []byte) error {
	for _, secret := range s.secrets {
		if secret != "" && bytes.Contains(body, []byte(secret)) {
			return errors.New("Task JSON contains a raw credential or private context")
		}
	}
	return nil
}

func smokeContextDigest(value map[string]any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sameScopes(a, b string) bool {
	left, right := strings.Fields(a), strings.Fields(b)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(slices.Compact(left), slices.Compact(right))
}
