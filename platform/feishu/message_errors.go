package feishu

import (
	"fmt"
	"net/http"

	"github.com/chenhg5/cc-connect/core"
)

func feishuMessageErrorKind(code int) core.MessageErrorKind {
	switch code {
	case 2200, 230020, 99991400:
		return core.MessageErrorTransient
	case 230028:
		return core.MessageErrorContentRejected
	case 230011:
		return core.MessageErrorTargetUnavailable
	case 230027, 230099:
		return core.MessageErrorPermanent
	default:
		return core.MessageErrorUnknown
	}
}

func (e *feishuMessageAPIError) MessageErrorKind() core.MessageErrorKind {
	return feishuMessageErrorKind(e.code)
}

func (e *feishuCardAPIError) MessageErrorKind() core.MessageErrorKind {
	if e == nil {
		return core.MessageErrorUnknown
	}
	return feishuMessageErrorKind(e.Code)
}

type messageHTTPError struct {
	operation string
	status    int
}

func (e *messageHTTPError) Error() string {
	return fmt.Sprintf("feishu: %s: HTTP status %d", e.operation, e.status)
}

func (e *messageHTTPError) MessageErrorKind() core.MessageErrorKind {
	switch {
	case e.status == http.StatusNotFound || e.status == http.StatusGone:
		return core.MessageErrorTargetUnavailable
	case e.status == 0 || e.status == http.StatusRequestTimeout || e.status == http.StatusTooManyRequests || e.status >= 500:
		return core.MessageErrorTransient
	case e.status >= 400 && e.status < 500:
		return core.MessageErrorPermanent
	default:
		return core.MessageErrorUnknown
	}
}
