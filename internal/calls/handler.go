package calls

import (
	"cloud.google.com/go/firestore"
	"cloud.google.com/go/storage"
	"context"
	"encoding/json"
	"errors"
	"github.com/sslim7/nature-was/internal/auth"
	"github.com/sslim7/nature-was/internal/callai"
	"github.com/sslim7/nature-was/internal/httpx"
	"github.com/sslim7/nature-was/internal/recipients"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"io"
	"log"
	"net/http"
	"strconv"
)

func validID(id string) bool { return recipients.ValidateID(id) }

// Options 는 **서버 통화분석 파이프라인**을 켜기 위한 배선이다.
//
// 🔴 비어 있으면 오디오·tick 라우트를 아예 걸지 않고 기존 기기 업로드 경로만 연다
// (internal/admin 이 ADMIN_JWT_SECRET 없을 때 하는 것과 같은 관례). AI 설정 하나 때문에
// SMS·로그인까지 죽을 이유가 없다.
type Options struct {
	Storage       *storage.Client
	Bucket        string // CALL_AUDIO_BUCKET
	RetentionDays int    // CALL_AUDIO_RETENTION_DAYS (응답·문서용 정보일 뿐, 삭제는 GCS 수명주기가 한다)
	Transcriber   callai.Transcriber
	Analyzer      callai.Analyzer
	ASRProvider   string
	LLMProvider   string
	Language      string
	TickAudience  string // CALL_TICK_AUDIENCE — Cloud Run 서비스 URL(경로를 붙이지 않는다)
	TickCaller    string // CALL_TICK_CALLER — 허용할 Scheduler 서비스 계정 이메일
	TickToken     string // CALL_TICK_TOKEN — 로컬·테스트 전용 공유 비밀
	TickBatch     int    // CALL_TICK_BATCH (기본 5)
}

func Register(mux *http.ServeMux, fs *firestore.Client, guard func(http.Handler) http.Handler, opts Options) {
	store := &Store{fs}
	// 상세 응답을 풍부하게 만드는 것은 파이프라인이 켜졌을 때뿐이다. registerPipeline 이
	// 자기가 만든 audioHandler 를 돌려주고, 그것을 GET 라우트에 얹는다.
	register(mux, store, guard, registerPipeline(mux, &fsJobs{fs}, store, guard, opts))
}

// registerPipeline 은 설정이 갖춰졌을 때만 서버 파이프라인 라우트를 건다.
func registerPipeline(mux *http.ServeMux, jobs jobRepo, store recordStore, guard func(http.Handler) http.Handler, opts Options) detailEnricher {
	if opts.Bucket == "" || opts.Storage == nil || opts.Transcriber == nil || opts.Analyzer == nil {
		log.Println("calls: CALL_AUDIO_BUCKET 또는 AI 공급자 설정이 없다 — 서버 통화분석 라우트를 켜지 않는다")
		return nil
	}
	audio := newGCSAudio(opts.Storage, opts.Bucket)
	h := &audioHandler{jobs: jobs, records: store, audio: audio, retentionDays: opts.RetentionDays}
	h.register(mux, guard)

	// 🔴 tick 호출자 설정이 없으면 라우트를 걸지 않는다. Cloud Run 이
	// allow_unauthenticated 라 설정 실수 하나가 곧 무방비 라우트이고, 그 라우트를 때리는
	// 요청마다 외부 ASR/LLM 요금이 나간다.
	if opts.TickCaller == "" && opts.TickToken == "" {
		log.Println("calls: CALL_TICK_CALLER 가 없다 — tick 라우트를 켜지 않는다(파이프라인이 돌지 않는다)")
		return h.enrichDetail
	}
	batch := opts.TickBatch
	if batch <= 0 {
		batch = 5
	}
	t := &tickHandler{
		auth: &tickAuth{validate: defaultTokenValidator, audience: opts.TickAudience, caller: opts.TickCaller, shared: opts.TickToken},
		pipe: &pipeline{
			jobs: jobs, records: store, audio: audio,
			asr: opts.Transcriber, llm: opts.Analyzer,
			asrProvider: opts.ASRProvider, llmProvider: opts.LLMProvider, language: opts.Language,
		},
		batch: batch,
	}
	// 🔴 userguard 로 감싸지 않는다. 이 경로는 사용자 JWT 와 무관하고 호출자는 사람이 아니다.
	mux.HandleFunc("POST /internal/calls/tick", t.serve)
	return h.enrichDetail
}

// detailEnricher 는 GET /calls/{id} 응답에만 작업 문서 정보(stage/job_state/audio_url)를 얹는다.
type detailEnricher func(ctx context.Context, uid string, r *Record)

// register 의 enrich 가 가변인자인 이유는 **기존 호출부와 테스트를 그대로 두기 위해서**다.
// 서버 파이프라인이 꺼져 있으면(버킷 미설정) 얹을 것이 없고, 그때도 기기 업로드 경로는
// 전과 똑같이 동작해야 한다.
func register(mux *http.ServeMux, store repository, guard func(http.Handler) http.Handler, enrich ...detailEnricher) {
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
			var rec Record
			rec, err = store.Get(r.Context(), uid, id)
			if err == nil && len(enrich) > 0 && enrich[0] != nil {
				// 🔴 **상세에서만** 작업 문서를 읽는다. 목록에서 30건마다 읽으면 조회 비용이
				// 30배가 되고, 목록 화면은 재생 버튼도 단계 표시도 쓰지 않는다.
				enrich[0](r.Context(), uid, &rec)
			}
			value = rec
		default:
			// q 는 「이름 또는 전화번호 뒷자리」다. 거르기를 앱에 맡기면 무한 스크롤로
			// 아직 받아오지 않은 통화가 검색에 안 걸린다 — 그래서 서버가 찾는다.
			// 값 범위 검증(limit 1~100, 검색어 길이, 커서)은 Store.List 한 곳에서 한다.
			limit := defaultPageSize
			if v := r.URL.Query().Get("limit"); v != "" {
				n, e := strconv.Atoi(v)
				if e != nil {
					err = ErrCursor
					break
				}
				limit = n
			}
			value, err = store.List(r.Context(), uid, r.URL.Query().Get("q"), limit, r.URL.Query().Get("cursor"))
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
