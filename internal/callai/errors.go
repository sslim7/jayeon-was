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

// CodeProviderTimeout 은 **공급자가 우리가 준 시간 안에 답하지 못했다**는 코드다.
//
// 🔴 이 코드가 붙은 에러는 KindRetryable 이고, 호출부는 이것을 **진짜 실패**로 다뤄야 한다
// (시도 횟수를 소모하고 백오프를 건다). 「우리 쪽 예산이 끊긴 것」과는 다른 사건이다 —
// 그쪽은 아예 *Error 가 아니라 맨 ctx 에러로 올라온다. 둘을 같은 것으로 읽으면
// 2026-09-18 처럼 멀쩡한 통화가 재시도 없이 버려지거나, 반대로 답하지 않는 공급자를
// 상한 없이 계속 부르게 된다.
const CodeProviderTimeout = "ProviderTimeout"

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
// callai.Error 가 아니면(순수 네트워크 실패 등) 재시도 대상으로 본다.
//
// 🔴 ctx 에러가 false 인 것은 **「재시도하지 마라」가 아니라 「이건 공급자 실패가 아니다」**
// 라는 뜻이다. 여기까지 맨 ctx 에러가 올라왔다면 호출부 자신이 건 데드라인이 끊긴 것이고,
// 그 판단은 호출부만 할 수 있다(예산 소진인지 요청 취소인지). 🔴 호출부는 이 함수의
// false 를 「확정 실패」로 읽으면 안 된다 — ctx 에러를 **먼저** 걸러 낸 뒤에 물어야 한다.
// 그 순서를 지키지 않아서 2026-09-18 에 25분짜리 통화가 ANALYSIS_FAILED 로 확정됐다.
// 공급자 자신의 타임아웃은 여기 오기 전에 *Error(KindProviderTimeout)로 감싸여 true 가 된다.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	// 🔴 **분류가 먼저다.** *Error 로 감싸인 에러는 그 Kind 가 유일한 근거다.
	// 순서를 뒤집어 ctx 검사를 먼저 하면, 공급자 타임아웃을 KindRetryable 로 감싸 놓아도
	// 그 안에 든 context.DeadlineExceeded 가 먼저 걸려 **재시도 불가로 뒤집힌다** —
	// 감싼 의미가 통째로 사라지고 2026-09-18 사고가 그대로 되풀이된다.
	var e *Error
	if errors.As(err, &e) {
		return e.Retryable()
	}
	// 여기까지 온 맨 ctx 에러는 호출부 자신이 건 데드라인이다(위 주석 참고).
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
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

// StatusOf 는 공급자가 돌려준 HTTP status 를 꺼낸다. 없으면 0.
// 🔴 실패 원인을 로그에서 읽으려면 분류(Kind)만으로는 모자란다 — 429 와 503 과 400 은
// 대응이 전부 다른데 Kind 로는 뭉개진다.
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// MessageOf 는 공급자가 돌려준 사람이 읽을 메시지를 꺼낸다.
//
// 🔴 이 값에는 **우리가 보낸 입력이 되비쳐 들어올 수 있다**(공급자가 처리 못 한 입력을
// 인용하는 경우가 있다). 공급자 파일이 300 rune 으로 한 번 자르지만, 로그에 넣는 쪽도
// 자기 상한을 한 번 더 걸어야 한다 — 통화 내용이 로그에 남는 경로는 하나도 허용되지 않는다.
func MessageOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Message
	}
	return ""
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
