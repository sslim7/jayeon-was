package callai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Kind 는 호출부가 재시도 여부를 판단하는 유일한 근거다.
// 공급자마다 코드 문자열이 다르므로 분류는 공급자 파일이 하고, internal/calls 는 Kind 만 본다.
type Kind int

const (
	// KindRetryable 는 잠시 뒤 같은 요청을 다시 보내면 성공할 수 있다(429, 5xx, 네트워크).
	KindRetryable Kind = iota
	// KindPermanent 는 같은 요청을 몇 번 보내도 결과가 같다(400 형식 오류, 401, 404 모델 없음, 403).
	KindPermanent
	// KindContentFiltered 는 공급자 콘텐츠 필터가 입력·출력을 거부한 경우다.
	// 재시도해도 같으므로 확정 실패지만, 사용자에게 보여 줄 문구가 달라야 해서 따로 둔다.
	KindContentFiltered
	// KindInputUnavailable 는 공급자가 우리가 준 오디오를 받아 가지 못한 경우다.
	// 오디오 참조가 만료됐거나 잘못됐다는 뜻이라, 폴링을 이어가는 대신 처음부터 다시 올려야 한다.
	KindInputUnavailable
)

func (k Kind) String() string {
	switch k {
	case KindRetryable:
		return "RETRYABLE"
	case KindContentFiltered:
		return "CONTENT_FILTERED"
	case KindInputUnavailable:
		return "INPUT_UNAVAILABLE"
	default:
		return "PERMANENT"
	}
}

// Error 는 공급자 응답을 우리 말로 옮긴 것이다.
//
// 🔴 Message 에 통화 원문이 섞여 들어오지 않게 하는 것은 공급자 파일의 책임이다.
// 이 값은 로그와 작업 문서에 남는다.
type Error struct {
	Kind      Kind
	Code      string // 공급자 코드. 예: "Throttling.RateQuota", "DataInspectionFailed"
	Status    int    // HTTP status. 없으면 0.
	RequestID string
	Message   string
	Err       error
}

func (e *Error) Error() string {
	b := strings.Builder{}
	fmt.Fprintf(&b, "callai: %s", e.Kind)
	if e.Code != "" {
		fmt.Fprintf(&b, " code=%s", e.Code)
	}
	if e.Status != 0 {
		fmt.Fprintf(&b, " status=%d", e.Status)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " request_id=%s", e.RequestID)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable 는 잠시 뒤 다시 시도할 가치가 있는지 알려준다.
func (e *Error) Retryable() bool { return e.Kind == KindRetryable }

// Retryable 는 error 가 무엇이든 재시도 가치를 판단한다.
// callai.Error 가 아니면(순수 네트워크·타임아웃 등) 재시도 대상으로 본다 —
// 다만 호출부가 취소한 경우는 재시도가 아니라 중단이다.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Retryable()
	}
	return true
}

// KindOf 는 분류를 꺼낸다. callai.Error 가 아니면 재시도 대상으로 본다.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindRetryable
}

// CodeOf 는 작업 문서에 남길 짧은 코드를 만든다. 공급자 코드가 없으면 분류 이름을 쓴다.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		if e.Code != "" {
			return e.Code
		}
		return e.Kind.String()
	}
	return KindRetryable.String()
}

// RequestIDOf 는 공급자 문의에 필요한 request_id 를 꺼낸다. 없으면 빈 문자열.
func RequestIDOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.RequestID
	}
	return ""
}

// classifyHTTP 는 HTTP status 와 공급자 코드로 분류를 정한다.
// Alibaba Model Studio 의 실제 코드 체계를 기준으로 삼되, 다른 공급자도 status 만으로
// 그럭저럭 맞게 떨어지도록 기본값을 잡았다.
func classifyHTTP(status int, code string) Kind {
	switch {
	case code == "DataInspectionFailed":
		// 콘텐츠 필터. 400 으로 오지만 형식 오류와 구분해야 사용자 안내가 달라진다.
		return KindContentFiltered
	case strings.HasPrefix(code, "InputDownloadFailed"), strings.HasSuffix(code, "InputDownloadFailed"):
		return KindInputUnavailable
	case strings.HasPrefix(code, "Throttling"):
		return KindRetryable
	}
	switch {
	case status == http.StatusTooManyRequests:
		return KindRetryable
	case status >= 500:
		return KindRetryable
	case status == http.StatusUnauthorized, status == http.StatusForbidden, status == http.StatusNotFound:
		return KindPermanent
	case status >= 400:
		return KindPermanent
	}
	return KindRetryable
}
