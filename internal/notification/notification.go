// Package notification delivers keyword-match alerts about new jobs: over
// Telegram in production, and to the log in development (mirrors
// internal/mailer's Mailer/Log/SMTP split for the same reason -- never a
// live chat id in a shared log sink).
package notification

import "context"

// Message is one notification: a chat to deliver to and the text to send.
type Message struct {
	ChatID int64
	Text   string
}

// Notifier sends messages.
type Notifier interface {
	Send(ctx context.Context, m Message) error
}

// Candidate is a job the scheduler found this tick, the minimum a matcher
// needs: its id (to record in notified_jobs) and its title (to match
// against a keyword). Nothing about accounts, storage or delivery lives
// here -- see internal/account.Service.NotifyNewJobs, which owns the
// per-account match+send+record loop and takes a Notifier and a []Candidate.
type Candidate struct {
	JobID int64
	Title string
}
