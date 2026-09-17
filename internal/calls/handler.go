package calls

import (
	"cloud.google.com/go/firestore"
	"encoding/json"
	"errors"
	"github.com/sslim7/nature-was/internal/auth"
	"github.com/sslim7/nature-was/internal/httpx"
	"github.com/sslim7/nature-was/internal/recipients"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"io"
	"net/http"
)

func validID(id string) bool { return recipients.ValidateID(id) }
func Register(mux *http.ServeMux, fs *firestore.Client, guard func(http.Handler) http.Handler) {
	register(mux, &Store{fs}, guard)
}
func register(mux *http.ServeMux, store repository, guard func(http.Handler) http.Handler) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		uid := auth.UserID(r.Context())
		if uid == "" {
			httpx.WriteError(w, 401, httpx.CodeUnauthorized, "로그인이 필요해요")
			return
		}
		id := r.PathValue("id")
		if id != "" && !validID(id) {
			httpx.WriteError(w, 400, httpx.CodeValidationFailed, "통화 ID를 확인해 주세요")
			return
		}
		var value any
		var err error
		switch {
		case r.Method == "PUT":
			var in Record
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
			dec.DisallowUnknownFields()
			err = dec.Decode(&in)
			if err == nil {
				var extra any
				if dec.Decode(&extra) != io.EOF {
					err = invalid
				}
			}
			var p *payload
			if err == nil {
				p, err = prepare(&in, id)
			}
			if err != nil {
				// 크기 초과(413)와 형식 오류(400)를 나눈다. 둘을 400 하나로 뭉치면
				// 클라이언트가 "다시 보내면 되는 요청" 인지 판단할 수 없다.
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) || errors.Is(err, errTooLarge) {
					httpx.WriteError(w, 413, "CALL_TOO_LARGE", "분석 데이터가 너무 커요")
					return
				}
				httpx.WriteError(w, 400, httpx.CodeValidationFailed, "분석 데이터 형식을 확인해 주세요")
				return
			}
			// 응답은 목록용 요약이다. 방금 받은 본문을 통째로 되돌려 주지 않는다.
			value, err = store.Save(r.Context(), uid, p)
		case id != "":
			value, err = store.Get(r.Context(), uid, id)
		default:
			// 이름 필터는 앱이 한다. 서버는 최신순 페이징만 하고 limit 은 받지 않는다.
			cursor := r.URL.Query().Get("cursor")
			if len(cursor) > 2048 {
				err = ErrCursor
			} else {
				value, err = store.List(r.Context(), uid, cursor)
			}
		}
		if err != nil {
			switch {
			case errors.Is(err, ErrConflict):
				httpx.WriteError(w, 409, "CALL_CONFLICT", "같은 ID의 다른 분석이 이미 저장되어 있어요")
			case errors.Is(err, ErrCursor):
				httpx.WriteError(w, 400, httpx.CodeValidationFailed, "조회 조건을 확인해 주세요")
			case status.Code(err) == codes.NotFound:
				httpx.WriteError(w, 404, "CALL_NOT_FOUND", "통화를 찾을 수 없어요")
			default:
				httpx.WriteError(w, 500, httpx.CodeInternal, "분석 결과를 저장하거나 불러오지 못했어요")
			}
			return
		}
		httpx.WriteJSON(w, 200, value)
	}
	mux.Handle("GET /calls", guard(http.HandlerFunc(handler)))
	mux.Handle("GET /calls/{id}", guard(http.HandlerFunc(handler)))
	mux.Handle("PUT /calls/{id}", guard(http.HandlerFunc(handler)))
}
