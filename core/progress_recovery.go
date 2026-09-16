package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"syscall"
	"time"
)

type progressRequestIDKey struct{}

// ProgressRequestID lets preview senders deduplicate creation retries after a
// lost acknowledgement. It changes only when a known unavailable card is rebuilt.
func ProgressRequestID(ctx context.Context) string {
	id, _ := ctx.Value(progressRequestIDKey{}).(string)
	return id
}

func classifyProgressError(err error) MessageErrorKind {
	var classified MessageErrorClassifier
	if errors.As(err, &classified) && classified.MessageErrorKind() != MessageErrorUnknown {
		return classified.MessageErrorKind()
	}
	var rejected ContentRejectedError
	if errors.As(err, &rejected) && rejected.ContentRejected() {
		return MessageErrorContentRejected
	}
	if errors.Is(err, ErrNotSupported) || errors.Is(err, context.Canceled) {
		return MessageErrorPermanent
	}
	var network net.Error
	if errors.As(err, &network) && (network.Timeout() || network.Temporary()) {
		return MessageErrorTransient
	}
	for _, transient := range []error{context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, syscall.EPIPE, syscall.ECONNREFUSED} {
		if errors.Is(err, transient) {
			return MessageErrorTransient
		}
	}
	return MessageErrorUnknown
}

// Buffer the newest progress while a temporary outage lasts. No retry worker
// outlives the turn: later events and Finalize drive bounded, throttled attempts.
func (w *compactProgressWriter) deferPublish(kind MessageErrorKind, err error) {
	w.failures++
	w.lastUpdateAt = time.Now()
	slog.Warn("progress writer: delivery deferred", "platform", w.platform.Name(), "kind", kind, "attempt", w.failures, "error", err)
	if w.ctx.Err() != nil {
		w.failed = true
		return
	}
	if kind == MessageErrorPermanent || kind == MessageErrorTargetUnavailable ||
		(kind == MessageErrorUnknown && w.failures >= 3) {
		w.failed = true
		w.notifyUnavailable()
		return
	}
	// A rejected redacted frame may become acceptable on the very next event;
	// retain its existing platform throttle without replaying rejected details.
	if kind != MessageErrorContentRejected {
		delay := time.Duration(min(250*(1<<min(w.failures-1, 7)), 30000)) * time.Millisecond
		if delay < w.minUpdateInterval {
			delay = w.minUpdateInterval
		}
		w.retryAt = w.lastUpdateAt.Add(delay)
	}
	if w.failures >= 3 || w.state != ProgressCardStateRunning {
		w.notifyUnavailable()
	}
}

// A single localized notice is the only standalone fallback. Never include
// the failed payload or API error; neither is necessarily safe for the chat.
func (w *compactProgressWriter) notifyUnavailable() {
	if w.notified || w.ctx.Err() != nil {
		return
	}
	w.notified = true
	ctx, cancel := w.withAPITimeout()
	defer cancel()
	if err := w.platform.Send(ctx, w.replyCtx, NewI18n(w.lang).T(MsgProgressUnavailable)); err != nil {
		slog.Warn("progress writer: status notice failed", "platform", w.platform.Name(), "error", err)
	}
}
