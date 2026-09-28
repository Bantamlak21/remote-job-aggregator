package notification

import (
	"context"
	"log/slog"
)

// Log prints messages instead of sending them. Development only: cmd/aggregator
// picks it whenever TELEGRAM_BOT_TOKEN is unset, the same way mailer.Log is
// picked for AUTH_MAILER=log. Never point its output at a shared, retained or
// centrally shipped log stream in production -- there is no live secret in a
// notification (unlike a sign-in link), but a chat id is still personal data.
type Log struct{ Logger *slog.Logger }

// Send logs the message.
func (l Log) Send(_ context.Context, m Message) error {
	l.Logger.Info("telegram notification (not sent: log notifier)", "chat_id", m.ChatID, "text", m.Text)
	return nil
}
