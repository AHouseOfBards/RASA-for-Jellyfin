// Package logging provides RASA's structured log, with redaction applied
// before anything reaches disk.
//
// Two audiences, never the same string (SPEC.md §15). The file holds
// everything a maintainer needs: structured, timestamped, correlated by run id
// and tagged by phase. The wizard renders its own curated stream from Events —
// the log file is not what the UI displays.
package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"sync"
)

// EventKind classifies a user-visible progress event.
type EventKind int

const (
	// EventStep is work starting: "Waiting for DNS — up to 2 min".
	EventStep EventKind = iota
	// EventOK is a step that completed.
	EventOK
	// EventWarn succeeded but something will bite later. Warnings must also
	// be written to the recovery file, because RASA will not exist when they
	// matter (SPEC.md §6, §15).
	EventWarn
	// EventFail is a step that could not complete.
	EventFail
)

func (k EventKind) String() string {
	switch k {
	case EventStep:
		return "step"
	case EventOK:
		return "ok"
	case EventWarn:
		return "warn"
	case EventFail:
		return "fail"
	}
	return "unknown"
}

// Event is a curated, user-facing progress line. Text must already be in plain
// language: Events are shown verbatim in the wizard.
type Event struct {
	Kind  EventKind
	Text  string
	Phase string
}

// Logger is RASA's logger. The embedded *slog.Logger carries the run id and
// current phase on every line.
type Logger struct {
	*slog.Logger

	red    *Redactor
	runID  string
	phase  string
	events chan Event

	// sink is shared by every Logger derived via WithPhase, so a derived
	// Logger can be copied by value without duplicating the mutex.
	sink *sink
}

type sink struct {
	mu     sync.Mutex
	closer io.Closer
}

// Options configures New.
type Options struct {
	// Writer receives structured JSON lines. Required.
	Writer io.Writer
	// Closer, if set, is closed by Close.
	Closer io.Closer
	// Level defaults to slog.LevelInfo.
	Level slog.Level
	// Redactor defaults to a new one with address redaction enabled.
	Redactor *Redactor
	// EventBuffer sizes the Events channel. Zero disables events.
	EventBuffer int

	// MaxBytes is the size at which Open rolls the file. Zero uses
	// DefaultMaxBytes; it is ignored by New, which does not own the writer.
	MaxBytes int64
	// Keep is how many rolled files Open retains. Zero uses DefaultKeep;
	// a negative value keeps none.
	Keep int
}

// New builds a Logger. Every line it writes passes through the Redactor first.
func New(opts Options) *Logger {
	red := opts.Redactor
	if red == nil {
		red = NewRedactor()
	}
	base := slog.NewJSONHandler(opts.Writer, &slog.HandlerOptions{Level: opts.Level})
	h := redactHandler{inner: base, red: red}

	runID := newRunID()
	l := &Logger{
		Logger: slog.New(h).With(slog.String("run_id", runID)),
		red:    red,
		runID:  runID,
		sink:   &sink{closer: opts.Closer},
	}
	if opts.EventBuffer > 0 {
		l.events = make(chan Event, opts.EventBuffer)
	}
	return l
}

// Open appends to path and returns a Logger writing to it, rolling the file
// once it passes Options.MaxBytes.
//
// The log is appended to rather than truncated: a user may run RASA several
// times, and the run id is what separates those runs in the bundle. Rolling is
// what keeps that from being unbounded — see rotate.go for why one of these
// logs is written by something that never stops.
func Open(path string, opts Options) (*Logger, error) {
	max, keep := opts.MaxBytes, opts.Keep
	if max == 0 {
		max = DefaultMaxBytes
	}
	if keep == 0 {
		keep = DefaultKeep
	}
	f, err := openRotating(path, max, keep)
	if err != nil {
		return nil, err
	}
	opts.Writer = f
	opts.Closer = f
	return New(opts), nil
}

// Redactor returns the redactor, so callers can register secrets the moment
// they enter the process.
func (l *Logger) Redactor() *Redactor { return l.red }

// RunID identifies this setup attempt. It appears on every line and is what
// makes a bundle containing four runs readable.
func (l *Logger) RunID() string { return l.runID }

// Events is the curated stream for the wizard. Nil unless EventBuffer was set.
func (l *Logger) Events() <-chan Event { return l.events }

// WithPhase returns a Logger tagged with a setup phase, so an interrupted run
// shows exactly where it stopped.
func (l *Logger) WithPhase(phase string) *Logger {
	c := *l
	c.Logger = l.Logger.With(slog.String("phase", phase))
	c.phase = phase
	return &c
}

// Phase reports the current phase tag.
func (l *Logger) Phase() string { return l.phase }

// Step records work starting, both to the log and to the wizard.
func (l *Logger) Step(text string, args ...any) {
	l.emit(EventStep, text)
	l.Info(text, args...)
}

// OK records a completed step.
func (l *Logger) OK(text string, args ...any) {
	l.emit(EventOK, text)
	l.Info(text, args...)
}

// Warned records a warning for the user. The caller is responsible for also
// persisting it to the recovery file — a warning shown once is gone by the
// time it matters.
func (l *Logger) Warned(text string, args ...any) {
	l.emit(EventWarn, text)
	l.Warn(text, args...)
}

// Failed records a user-visible failure. Pass the user-facing message here;
// technical detail belongs in args, where redaction still applies.
func (l *Logger) Failed(text string, args ...any) {
	l.emit(EventFail, text)
	l.Error(text, args...)
}

// Decision records why a branch was taken, not merely which one.
//
// SPEC.md §15: a single line such as "chose Mode A: router WAN matches
// observed address, UPnP granted permanent lease on 443" answers most support
// questions on its own.
func (l *Logger) Decision(what, chose, because string, args ...any) {
	all := append([]any{
		slog.String("decision", what),
		slog.String("chose", chose),
		slog.String("because", because),
	}, args...)
	l.Info("decision: "+what+" = "+chose, all...)
}

func (l *Logger) emit(k EventKind, text string) {
	if l.events == nil {
		return
	}
	ev := Event{Kind: k, Text: l.red.Redact(text), Phase: l.phase}
	select {
	case l.events <- ev:
	default: // never block setup on a slow or absent UI reader
	}
}

// Close closes the underlying file, if any. Safe to call on any Logger derived
// via WithPhase, and safe to call more than once.
func (l *Logger) Close() error {
	l.sink.mu.Lock()
	defer l.sink.mu.Unlock()
	if l.sink.closer == nil {
		return nil
	}
	err := l.sink.closer.Close()
	l.sink.closer = nil
	return err
}

// Discard returns a Logger that writes nowhere. For tests.
func Discard() *Logger { return New(Options{Writer: io.Discard}) }

func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// redactHandler applies redaction to the message and every attribute before
// delegating. Placing it at the handler level means there is no logging call
// that can bypass it.
type redactHandler struct {
	inner slog.Handler
	red   *Redactor
}

func (h redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h redactHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, h.red.Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(h.redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, nr)
}

func (h redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = h.redactAttr(a)
	}
	return redactHandler{inner: h.inner.WithAttrs(out), red: h.red}
}

func (h redactHandler) WithGroup(name string) slog.Handler {
	return redactHandler{inner: h.inner.WithGroup(name), red: h.red}
}

func (h redactHandler) redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.red.Redact(v.String()))
	case slog.KindGroup:
		src := v.Group()
		out := make([]slog.Attr, len(src))
		for i, g := range src {
			out[i] = h.redactAttr(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	case slog.KindAny:
		// Errors are the most common way a raw API response — and therefore a
		// secret — reaches a log line.
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, h.red.Redact(err.Error()))
		}
		if s, ok := v.Any().(interface{ String() string }); ok {
			return slog.String(a.Key, h.red.Redact(s.String()))
		}
		return a
	default:
		return a
	}
}
