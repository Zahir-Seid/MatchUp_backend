package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// resendEndpoint is Resend's transactional send API.
const resendEndpoint = "https://api.resend.com/emails"

// ResendSender sends transactional email through Resend.
//
// Hand-rolled over net/http rather than pulled in as an SDK: one POST with a
// bearer token is the entire integration, and this way the retry and error
// classification stay ours.
type ResendSender struct {
	apiKey string
	from   string
	client *http.Client
}

// NewResendSender builds an email sender. from is a full From header, e.g.
// "MatchUp <no-reply@matchup.et>".
func NewResendSender(apiKey, from string) (*ResendSender, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errors.New("resend: API key is empty")
	}
	from = strings.TrimSpace(from)
	if from == "" {
		return nil, errors.New("resend: from address is empty")
	}
	return &ResendSender{
		apiKey: apiKey,
		from:   from,
		client: &http.Client{Timeout: 20 * time.Second},
	}, nil
}

type resendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html,omitempty"`
	Text    string   `json:"text,omitempty"`
}

// resendError is the API's error envelope.
type resendError struct {
	StatusCode int    `json:"statusCode"`
	Name       string `json:"name"`
	Message    string `json:"message"`
}

// ErrEmailRejected marks a send that failed for a reason retrying cannot fix: a
// malformed address, an unverified sending domain, a body Resend refuses.
var ErrEmailRejected = errors.New("email rejected")

// Send posts one email. A 4xx other than 429 is wrapped in ErrEmailRejected so
// the caller can tell "this will never work" from "try again later"; everything
// else is returned plain and therefore retried.
func (s *ResendSender) Send(ctx context.Context, msg EmailMessage) error {
	to := strings.TrimSpace(msg.To)
	if to == "" {
		return fmt.Errorf("%w: no recipient", ErrEmailRejected)
	}

	body, err := json.Marshal(resendRequest{
		From:    s.from,
		To:      []string{to},
		Subject: msg.Subject,
		HTML:    msg.HTML,
		Text:    msg.Text,
	})
	if err != nil {
		return fmt.Errorf("resend: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resendEndpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("resend: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("resend: send: %w", err)
	}
	defer resp.Body.Close()

	// Read a bounded amount: enough for the error envelope, not so much that a
	// misbehaving response can exhaust memory.
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	detail := describeResendError(payload)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode >= 500:
		return fmt.Errorf("resend: %s: %s", resp.Status, detail)
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		// A bad key is a deployment problem, not a per-message one: keep it
		// retryable so fixing the key drains the queue.
		return fmt.Errorf("resend: credentials rejected (%s): %s", resp.Status, detail)
	default:
		return fmt.Errorf("%w: resend returned %s: %s", ErrEmailRejected, resp.Status, detail)
	}
}

// describeResendError pulls the human message out of the error envelope,
// falling back to the raw body when it is not the shape we expect.
func describeResendError(payload []byte) string {
	if len(payload) == 0 {
		return "no response body"
	}
	var apiErr resendError
	if err := json.Unmarshal(payload, &apiErr); err == nil && apiErr.Message != "" {
		if apiErr.Name != "" {
			return apiErr.Name + ": " + apiErr.Message
		}
		return apiErr.Message
	}
	return strings.TrimSpace(string(payload))
}
