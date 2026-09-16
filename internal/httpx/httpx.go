// Package httpx 는 openapi.yaml 의 공통 에러 모델(`components.schemas.Error`)에 맞춘
// JSON 응답 헬퍼를 모아 둔다. 웹 프레임워크를 쓰지 않으므로 이 정도 얇은 층만 둔다.
package httpx

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
)

// 공통 에러 코드. openapi.yaml `components.schemas.Error.code` 에 들어가는 값이다.
const (
	CodeValidationFailed = "VALIDATION_FAILED"
	CodeUnauthorized     = "UNAUTHORIZED"
	CodeInternal         = "INTERNAL_ERROR"
)

// Error 는 openapi.yaml 의 Error 스키마와 1:1 대응한다.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// WriteJSON 은 v 를 JSON 으로 직렬화해 status 와 함께 내보낸다.
// v 가 nil 이면 바디 없이 상태 코드만 쓴다.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if v == nil {
		w.WriteHeader(status)
		return
	}

	// 직렬화 실패 시 헤더가 이미 나간 뒤라 되돌릴 수 없으므로 먼저 버퍼에 담는다.
	body, err := json.Marshal(v)
	if err != nil {
		log.Printf("httpx: JSON 직렬화 실패: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"code":"INTERNAL_ERROR","message":"서버 오류가 생겼어요"}`))
		return
	}

	w.WriteHeader(status)
	w.Write(body)
}

// WriteError 는 Error 스키마 그대로의 바디를 내보낸다.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	noStore(w)
	WriteJSON(w, status, Error{Code: code, Message: message})
}

// WriteErrorDetails 는 필드별 위반 내역 등 부가 정보를 함께 내보낸다.
func WriteErrorDetails(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	noStore(w)
	WriteJSON(w, status, Error{Code: code, Message: message, Details: details})
}

// noStore 는 **오류 응답이 캐시에 남지 않게 한다.**
//
// 앞단(호스팅·CDN)이 우리가 아무 말도 안 하면 자기 기본값으로 10분을 캐시한다.
// 오류는 그 사이에 없어질 수 있는 것이라(권한이 붙고, 잠금이 풀리고, 없던 문서가 생긴다)
// 캐시에 남으면 **이미 고쳐진 문제를 10분 동안 계속 보여 준다.** 실제로 존재하지 않는
// 코드의 404 가 캐시돼 있던 것을 확인하고 넣은 장치다.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

// maxBodyBytes 는 요청 바디 상한이다. 인증 요청은 모두 작은 JSON 이다.
const maxBodyBytes = 64 << 10

// DecodeBody 는 요청 바디를 v 로 디코딩하고 실패하면 에러를 돌려준다.
// 응답까지 직접 쓰는 DecodeJSON 과 달리 호출자가 에러 응답을 결정한다.
func DecodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	return dec.Decode(v)
}

// DecodeJSON 은 요청 바디를 dst 로 디코딩한다.
// 실패하면 400 VALIDATION_FAILED 를 직접 응답하고 false 를 돌려준다.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	if err := dec.Decode(dst); err != nil {
		WriteError(w, http.StatusBadRequest, CodeValidationFailed, "요청 값이 올바르지 않아요")
		return false
	}
	return true
}
