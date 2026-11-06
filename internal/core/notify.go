package core

// This file defines a transport-neutral message model so the service and
// auto-stop logic can post alerts without depending on Discord.

import (
	"context"
	"sync"
)

type Kind int

const (
	Alerts Kind = iota // global #alerts, or the app's own alerts channel
	Digest             // #daily-cost
)

// Target says where a message goes. For Alerts, a non-empty App routes to
// that app's alerts channel when one is configured.
type Target struct {
	Kind Kind
	App  string
}

type Style int

const (
	Info Style = iota
	Good
	Warn
	Bad
)

type Field struct {
	Name   string
	Value  string
	Inline bool
}

type Button struct {
	Label    string
	CustomID string
	Danger   bool
}

type Message struct {
	Title        string
	Body         string
	Style        Style
	Fields       []Field
	Buttons      []Button
	MentionOwner bool
	Footer       string
}

type Notifier interface {
	Send(ctx context.Context, t Target, m Message) error
}

// Recorder is a Notifier that keeps messages in memory, for tests.
type Recorder struct {
	mu   sync.Mutex
	Sent []Sent
}

type Sent struct {
	Target  Target
	Message Message
}

func (r *Recorder) Send(_ context.Context, t Target, m Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Sent = append(r.Sent, Sent{t, m})
	return nil
}

// Messages returns a copy of everything sent so far.
func (r *Recorder) Messages() []Sent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Sent(nil), r.Sent...)
}

// Reset clears recorded messages.
func (r *Recorder) Reset() {
	r.mu.Lock()
	r.Sent = nil
	r.mu.Unlock()
}
