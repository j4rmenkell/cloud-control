package core

// Display helpers shared by alerts and bot replies.

import (
	"fmt"
	"time"

	"cloud-control/internal/provider"
)

var symbols = map[string]string{"USD": "$", "EUR": "€", "GBP": "£", "PHP": "₱", "JPY": "¥"}

// Money renders an amount, marking estimates: "≈ $1.20 (estimated)".
func Money(amount float64, currency string, estimated bool) string {
	s := Amount(amount, currency)
	if estimated {
		return "≈ " + s + " (estimated)"
	}
	return s
}

// Amount renders an amount without the estimate marker.
func Amount(amount float64, currency string) string {
	if sym, ok := symbols[currency]; ok {
		return fmt.Sprintf("%s%.2f", sym, amount)
	}
	return fmt.Sprintf("%.2f %s", amount, currency)
}

func HealthEmoji(s provider.HealthState) string {
	switch s {
	case provider.Healthy:
		return "🟢"
	case provider.Degraded:
		return "🟡"
	case provider.Down:
		return "🔴"
	case provider.Stopped:
		return "⏸️"
	}
	return "⚪"
}

// Bar renders a 10-cell progress bar for a percentage (capped at 100%).
func Bar(percent float64) string {
	n := int(percent / 10)
	n = max(0, min(n, 10))
	out := ""
	for i := range 10 {
		if i < n {
			out += "█"
		} else {
			out += "░"
		}
	}
	return out
}

// Discord renders <t:unix:R> as a relative time in the viewer's locale.
func RelTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return fmt.Sprintf("<t:%d:R>", t.Unix())
}

func AbsTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return fmt.Sprintf("<t:%d:f>", t.Unix())
}
