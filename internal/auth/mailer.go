package auth

import (
	"context"
	"log/slog"

	"github.com/resend/resend-go/v2"
)

// ResendMailer sends through Resend.
type ResendMailer struct {
	client *resend.Client
	from   string
}

// NewResendMailer constructs a mailer.
func NewResendMailer(apiKey, from string) *ResendMailer {
	return &ResendMailer{client: resend.NewClient(apiKey), from: from}
}

// Send implements Mailer.
func (m *ResendMailer) Send(ctx context.Context, to, subject, text, html string) error {
	_, err := m.client.Emails.SendWithContext(ctx, &resend.SendEmailRequest{
		From: m.from, To: []string{to}, Subject: subject, Text: text, Html: html,
	})
	return err
}

// LogMailer prints links to the log instead of sending. Dev only.
type LogMailer struct{ Log *slog.Logger }

// Send implements Mailer.
func (m LogMailer) Send(_ context.Context, to, subject, text, _ string) error {
	log := m.Log
	if log == nil {
		log = slog.Default()
	}
	log.Info("MAIL (dev mode, not sent)", "to", to, "subject", subject, "body", text)
	return nil
}
