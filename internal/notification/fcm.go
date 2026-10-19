package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"google.golang.org/api/option"
)

// fcmMaxTokensPerCall is FCM's per-request ceiling for SendEach. One user's
// devices never come close, but the batch is split rather than rejected.
const fcmMaxTokensPerCall = 500

// FCMSender delivers push notifications through Firebase Cloud Messaging.
type FCMSender struct {
	client *messaging.Client
}

// NewFCMSender builds a push sender from a service-account JSON credential.
//
// The credential is passed as a string rather than a file path because that is
// how it arrives on Railway: one environment variable holding the whole JSON
// document, never a file committed to the image.
func NewFCMSender(ctx context.Context, serviceAccountJSON string) (*FCMSender, error) {
	serviceAccountJSON = strings.TrimSpace(serviceAccountJSON)
	if serviceAccountJSON == "" {
		return nil, errors.New("fcm: service account JSON is empty")
	}
	app, err := firebase.NewApp(ctx, nil, option.WithCredentialsJSON([]byte(serviceAccountJSON)))
	if err != nil {
		return nil, fmt.Errorf("fcm: init app: %w", err)
	}
	client, err := app.Messaging(ctx)
	if err != nil {
		return nil, fmt.Errorf("fcm: init messaging: %w", err)
	}
	return &FCMSender{client: client}, nil
}

// Send pushes one message to every token, reporting the tokens FCM says are
// permanently gone.
//
// A per-token failure is not an error for the delivery as a whole: one stale
// phone must not keep the notification queued forever when the user's other
// devices got it. Only a failure that affects every token is returned, since
// that is the shape of an outage or a bad credential.
func (s *FCMSender) Send(ctx context.Context, tokens []string, msg PushMessage) ([]string, error) {
	if len(tokens) == 0 {
		return nil, nil
	}

	var dead []string
	var failures []error
	succeeded := 0

	for start := 0; start < len(tokens); start += fcmMaxTokensPerCall {
		end := min(start+fcmMaxTokensPerCall, len(tokens))
		chunk := tokens[start:end]

		messages := make([]*messaging.Message, 0, len(chunk))
		for _, token := range chunk {
			messages = append(messages, &messaging.Message{
				// Token is deprecated in favour of Fid upstream, but a
				// registration token is what the Flutter client hands us and
				// what the field still accepts.
				Token:        token,
				Notification: &messaging.Notification{Title: msg.Title, Body: msg.Body},
				Data:         msg.Data,
				Android: &messaging.AndroidConfig{
					Priority: "high",
				},
				APNS: &messaging.APNSConfig{
					Payload: &messaging.APNSPayload{
						Aps: &messaging.Aps{Sound: "default"},
					},
				},
			})
		}

		batch, err := s.client.SendEach(ctx, messages)
		if err != nil {
			// The whole call failed: nothing in this chunk was attempted
			// individually, so it is one retryable failure.
			failures = append(failures, err)
			continue
		}
		for i, resp := range batch.Responses {
			switch {
			case resp.Success:
				succeeded++
			case messaging.IsUnregistered(resp.Error), messaging.IsRegistrationTokenNotRegistered(resp.Error):
				// The app was uninstalled or the token was rotated. Drop it
				// rather than retry it forever (SDD 11.2).
				dead = append(dead, chunk[i])
			case messaging.IsInvalidArgument(resp.Error):
				// A malformed token can never become valid.
				dead = append(dead, chunk[i])
			default:
				failures = append(failures, fmt.Errorf("token %d: %w", start+i, resp.Error))
			}
		}
	}

	if succeeded == 0 && len(failures) > 0 {
		return dead, fmt.Errorf("fcm: no device accepted the message: %w", errors.Join(failures...))
	}
	return dead, nil
}
