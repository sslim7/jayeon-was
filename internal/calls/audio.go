package calls

import (
	"context"
	"errors"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/sslim7/nature-was/internal/auth"
	"github.com/sslim7/nature-was/internal/httpx"
	"github.com/sslim7/nature-was/internal/recipients"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// audio.go 는 **오디오 바이트가 서버를 통과하지 않는** 업로드 경로다.
//
//	앱 → POST /calls/{id}/audio/upload-url   (서명 URL 발급, 작업 문서 생성)
//	앱 → PUT  <서명 URL>                      (앱 → GCS 직접. 서버는 관여하지 않는다)
//	앱 → POST /calls/{id}/audio/complete     (객체 확인 후 큐잉)
//	앱 → GET  /calls/{id}                     (진행 상태 폴링)
//
// 🔴 서버가 오디오를 중계하면 30분 통화 28 MB 가 인스턴스 메모리를 그대로 먹고, Cloud Run
// 요청 타임아웃(60초) 안에 느린 회선의 업로드가 끝나지 않는다. 중계는 하지 않는다.

const (
	// maxAudioBytes 는 업로드 상한이다. 30분 통화가 대략 28 MB 라 100 MB 면 몇 시간짜리도 들어간다.
	maxAudioBytes = 100 << 20
	// signedURLTTL 은 서명 URL 수명이다.
	//
	// 🔴 15분이 아니라 60분인 이유: 100 MB 를 느린 회선으로 올리는 동안 URL 이 만료되면
	// 업로드가 중간에 401 로 죽는데, **그 실패는 브라우저에서만 보이고 서버 로그에는
	// 아무것도 남지 않는다.** 원인을 찾을 단서가 없는 실패를 만들지 않는다.
	signedURLTTL = 60 * time.Minute
	// audioURLTTL 은 재생용 서명 GET URL 수명이다. 응답에 실려 나가는 값이라 짧게 둔다 —
	// 앱은 상세를 열 때마다 새로 받는다.
	audioURLTTL = 15 * time.Minute
)

// audioContentTypes 는 허용 MIME 과 객체 확장자다.
//
// 확장자를 붙이는 이유는 일부 공급자가 파일 이름 확장자로 형식을 판별하기 때문이다.
var audioContentTypes = map[string]string{
	"audio/m4a":   ".m4a",
	"audio/mp4":   ".m4a",
	"audio/aac":   ".aac",
	"audio/mpeg":  ".mp3",
	"audio/wav":   ".wav",
	"audio/x-wav": ".wav",
	"audio/ogg":   ".ogg",
	"audio/webm":  ".webm",
	"audio/amr":   ".amr",
	"audio/3gpp":  ".3gp",
}

// audioObject 는 오디오 객체 경로다.
//
// 🔴 **결정적(deterministic) 경로다.** 랜덤 이름을 쓰면 업로드 재시도마다 고아 객체가
// 생기고 버킷 보관기간(366일) 내내 요금이 나간다. 같은 경로면 재시도가 그냥 덮어쓴다.
// 그래서 upload-url 은 complete 전이라면 몇 번이든 다시 부를 수 있다.
func audioObject(uid, callID, ext string) string {
	return path.Join("calls", uid, callID, "audio") + ext
}

type uploadURLRequest struct {
	ContentType string   `json:"content_type"`
	Size        int64    `json:"size"`
	Contact     Contact  `json:"contact"`
	Call        Metadata `json:"call"`
}

type uploadURLResponse struct {
	CallID    string            `json:"call_id"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	Object    string            `json:"object"`
	ExpiresAt string            `json:"expires_at"`
	MaxBytes  int64             `json:"max_bytes"`
	// RetentionDays 는 원본 오디오 보관 일수다. **삭제는 GCS 수명주기 정책이 한다** —
	// 서버는 앱에 알려 주기만 한다. 이 기간이 지나면 has_audio 가 false 가 되지만
	// 전사문·분석은 Firestore 에 그대로 남는다(실패가 아니다).
	RetentionDays int `json:"retention_days,omitempty"`
}

// audioHandler 는 오디오 3종 엔드포인트를 담는다. CALL_AUDIO_BUCKET 이 없으면 아예 만들지 않는다.
type audioHandler struct {
	jobs    jobRepo
	records recordStore
	audio   audioStore
	now     func() time.Time
	// retentionDays 는 버킷 수명주기 정책의 보관 일수다. **삭제는 GCS 가 한다** —
	// 서버는 앱에 알려 주기 위한 정보로만 들고 있다.
	retentionDays int
}

func (h *audioHandler) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// load 는 작업을 읽고 소유자를 확인한다.
//
// 🔴 남의 통화 ID 는 403 이 아니라 **404** 다. 403 은 「그 ID 가 존재한다」는 사실을
// 알려 주는 것과 같다 — 통화 ID 는 앱이 정하는 값이라 남의 것을 찍어 볼 수 있다.
func (h *audioHandler) load(ctx context.Context, uid, id string) (*job, error) {
	j, err := h.jobs.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if j.UID != uid {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return j, nil
}

func (h *audioHandler) uploadURL(w http.ResponseWriter, r *http.Request, uid, id string) {
	var in uploadURLRequest
	if err := httpx.DecodeBody(r, &in); err != nil {
		httpx.WriteError(w, 400, httpx.CodeValidationFailed, "요청 값을 확인해 주세요")
		return
	}
	ext, ok := audioContentTypes[strings.ToLower(strings.TrimSpace(in.ContentType))]
	if !ok || in.Size < 1 || in.Size > maxAudioBytes {
		httpx.WriteError(w, 400, httpx.CodeValidationFailed, "지원하지 않는 녹음 파일이에요")
		return
	}
	contentType := strings.ToLower(strings.TrimSpace(in.ContentType))
	meta, err := validateAudioMeta(&in, id)
	if err != nil {
		httpx.WriteError(w, 400, httpx.CodeValidationFailed, "통화 정보를 확인해 주세요")
		return
	}

	now := h.clock()
	// 🔴 여기서 load 를 쓰면 안 된다. load 는 「남의 것」과 「없는 것」을 똑같이 NotFound 로
	// 돌려주는데, 그러면 남의 통화 ID 로 upload-url 을 불러 **그 작업 문서를 통째로 덮어쓸 수
	// 있다.** 존재 여부와 소유권을 여기서만 따로 본다(응답은 여전히 404 라 구분되지 않는다).
	j, err := h.jobs.Get(r.Context(), id)
	switch {
	case status.Code(err) == codes.NotFound:
		j = &job{UID: uid, CallID: id, CreatedAt: now, NextAttemptAt: now}
	case err != nil:
		httpx.WriteError(w, 500, httpx.CodeInternal, "업로드 주소를 만들지 못했어요")
		return
	case j.UID != uid:
		httpx.WriteError(w, 404, "CALL_NOT_FOUND", "통화를 찾을 수 없어요")
		return
	case j.State != stateAwaitingUpload:
		// 이미 큐에 들어갔거나 끝난 통화의 오디오를 갈아 끼우면 전사문과 원본이 어긋난다.
		httpx.WriteError(w, 409, "CALL_ALREADY_QUEUED", "이미 분석이 시작된 통화예요")
		return
	}

	object := audioObject(uid, id, ext)
	url, err := h.audio.SignedPut(r.Context(), object, contentType, now.Add(signedURLTTL))
	if err != nil {
		// ⚠️ 원인은 로그에만 남긴다. 운영에서 이 에러는 대개 iam.serviceAccounts.signBlob 권한이다.
		log.Printf("calls: 서명 URL 발급 실패 call=%s: %v", id, err)
		httpx.WriteError(w, 500, httpx.CodeInternal, "업로드 주소를 만들지 못했어요")
		return
	}

	j.State = stateAwaitingUpload
	j.Stage = stageOf(stateAwaitingUpload)
	j.Progress = progressOf(stateAwaitingUpload)
	j.UpdatedAt = now
	// AWAITING_UPLOAD 는 스윕 대상이 아니다. 값은 남겨 두되 tick 은 이 작업을 집지 않는다.
	j.NextAttemptAt = now
	j.Audio = audioRef{Bucket: h.audio.Bucket(), Object: object, Size: in.Size, ContentType: contentType, FileName: meta.FileName}
	j.ContactName, j.ContactPhone, j.ContactRecipientID = in.Contact.Name, in.Contact.Phone, derefString(in.Contact.RecipientID)
	j.FileName, j.RecordedAt = meta.FileName, meta.RecordedAt
	if meta.Duration > 0 {
		j.Duration = meta.Duration
	}
	if err := h.jobs.Put(r.Context(), j); err != nil {
		httpx.WriteError(w, 500, httpx.CodeInternal, "업로드 주소를 만들지 못했어요")
		return
	}
	httpx.WriteJSON(w, 200, uploadURLResponse{
		CallID: id, Method: "PUT", URL: url,
		// 🔴 이 헤더를 **글자 그대로** 보내야 서명이 맞는다. 다르면 GCS 가 403
		// SignatureDoesNotMatch 를 돌려주는데 응답만 봐서는 원인을 알 수 없다.
		Headers:       map[string]string{"Content-Type": contentType},
		Object:        object,
		ExpiresAt:     now.Add(signedURLTTL).UTC().Format(time.RFC3339),
		MaxBytes:      maxAudioBytes,
		RetentionDays: h.retentionDays,
	})
}

func (h *audioHandler) complete(w http.ResponseWriter, r *http.Request, uid, id string) {
	j, err := h.load(r.Context(), uid, id)
	if err != nil {
		writeJobError(w, err, "통화를 찾을 수 없어요")
		return
	}
	if j.Audio.Object == "" {
		httpx.WriteError(w, 409, "CALL_NO_AUDIO", "업로드 주소를 먼저 받아 주세요")
		return
	}
	// 이미 큐에 들어간 통화를 다시 complete 해도 조용히 지금 상태를 돌려준다(앱 재시도 대비).
	if j.State != stateAwaitingUpload {
		httpx.WriteJSON(w, 200, j.summaryRecord())
		return
	}
	size, err := h.audio.Size(r.Context(), j.Audio.Object)
	switch {
	case errors.Is(err, errAudioMissing):
		httpx.WriteError(w, 400, "CALL_AUDIO_MISSING", "녹음 파일 업로드가 끝나지 않았어요")
		return
	case err != nil:
		httpx.WriteError(w, 500, httpx.CodeInternal, "녹음 파일을 확인하지 못했어요")
		return
	case size <= 0 || size > maxAudioBytes:
		httpx.WriteError(w, 400, "CALL_AUDIO_MISSING", "녹음 파일 크기를 확인해 주세요")
		return
	}

	now := h.clock()
	j.Audio.Size = size
	j.State = stateQueued
	j.Stage = stageOf(stateQueued)
	j.Progress = progressOf(stateQueued)
	j.UpdatedAt = now
	j.NextAttemptAt = now
	j.ErrorCode, j.ErrorKind, j.ErrorAt = "", "", time.Time{}

	// 🔴 **플레이스홀더 통화 레코드를 여기서 만든다.** 이것이 있어야 앱이 이미 쓰고 있는
	// GET /calls, GET /calls/{id} 로 진행 상태가 그대로 보인다. 앱에 새 폴링 API 를
	// 만들게 하지 않는 것이 이 설계의 핵심이다.
	rec := j.summaryRecord()
	pay, perr := prepareServer(&rec, id)
	if perr != nil {
		httpx.WriteError(w, 400, httpx.CodeValidationFailed, "통화 정보를 확인해 주세요")
		return
	}
	if _, err := h.records.SaveOverwrite(r.Context(), uid, pay); err != nil {
		httpx.WriteError(w, 500, httpx.CodeInternal, "통화를 등록하지 못했어요")
		return
	}
	if err := h.jobs.Put(r.Context(), j); err != nil {
		httpx.WriteError(w, 500, httpx.CodeInternal, "통화를 등록하지 못했어요")
		return
	}
	httpx.WriteJSON(w, 200, j.summaryRecord())
}

// reanalyze 는 **오디오를 다시 전사하지 않고** 분석만 다시 돌린다.
//
// 🔴 ASR 이 이 파이프라인에서 가장 비싼 단계이고, 재분석이 필요한 이유는 대개 프롬프트가
// 바뀌었거나 LLM 이 실패했을 때다. 전사문은 그대로 쓸 수 있다.
// 오디오 원본은 366일 뒤 사라지지만 **이 경로는 전사문만 쓰므로 그 뒤에도 동작한다.**
func (h *audioHandler) reanalyze(w http.ResponseWriter, r *http.Request, uid, id string) {
	j, err := h.load(r.Context(), uid, id)
	if err != nil {
		writeJobError(w, err, "통화를 찾을 수 없어요")
		return
	}
	switch j.State {
	case stateCompleted, stateAnalysisFailed, stateTranscribed:
	default:
		httpx.WriteError(w, 409, "CALL_NOT_ANALYZABLE", "아직 분석할 수 있는 상태가 아니에요")
		return
	}

	if j.TranscriptShards == 0 {
		// 작업 문서의 전사문이 없으면(옛 통화, 정리된 조각) 통화 레코드에서 되살린다.
		rec, gerr := h.records.Get(r.Context(), uid, id)
		if gerr != nil || rec.Transcript == nil || strings.TrimSpace(rec.Transcript.Text) == "" {
			httpx.WriteError(w, 409, "CALL_NO_TRANSCRIPT", "다시 분석할 원문이 없어요")
			return
		}
		b, merr := marshalCompact(rec.Transcript)
		if merr != nil {
			httpx.WriteError(w, 500, httpx.CodeInternal, "원문을 준비하지 못했어요")
			return
		}
		shards, perr := h.jobs.PutTranscript(r.Context(), id, b)
		if perr != nil {
			httpx.WriteError(w, 500, httpx.CodeInternal, "원문을 준비하지 못했어요")
			return
		}
		j.TranscriptShards = shards
	}

	now := h.clock()
	j.State = stateTranscribed
	j.Stage = stageOf(stateTranscribed)
	j.Progress = progressOf(stateTranscribed)
	j.HasAnalysis = false
	j.AnalysisAttempt = 0
	j.ErrorCode, j.ErrorKind, j.ErrorAt = "", "", time.Time{}
	j.UpdatedAt = now
	j.NextAttemptAt = now
	if err := h.jobs.Put(r.Context(), j); err != nil {
		httpx.WriteError(w, 500, httpx.CodeInternal, "재분석을 예약하지 못했어요")
		return
	}
	// 앱이 즉시 「내용 정리하는 중」을 보게 한다. 실패해도 다음 tick 이 다시 맞춘다.
	if err := h.records.Touch(r.Context(), uid, id, func(rec *Record) {
		s := j.summaryRecord()
		rec.Status, rec.JobState, rec.Stage, rec.Progress, rec.Error = s.Status, s.JobState, s.Stage, s.Progress, nil
	}); err != nil && status.Code(err) != codes.NotFound {
		log.Printf("calls: 재분석 상태 갱신 실패 call=%s: %v", id, err)
	}
	httpx.WriteJSON(w, 200, j.summaryRecord())
}

// enrichDetail 은 **GET /calls/{id} 응답에만** 작업 문서 정보를 얹는다.
//
// 🔴 목록에는 붙이지 않는다. 30건마다 작업 문서 30개를 읽고 서명 30개를 만드는 것은
// 목록 조회에 불필요한 비용이다 — 그래서 목록용 요약 필드(status/stage/has_audio)는
// 파이프라인이 통화 레코드에 복제해 둔다. 상세 한 건에서 읽기 1회가 느는 것은 감수한다.
func (h *audioHandler) enrichDetail(ctx context.Context, uid string, r *Record) {
	j, err := h.jobs.Get(ctx, r.CallID)
	// 작업 문서가 없는 통화는 기기 업로드 경로로 저장된 것이다 — 정상이므로 그대로 둔다.
	if err != nil || j.UID != uid {
		return
	}
	s := j.summaryRecord()
	// 저장된 요약은 파이프라인이 best-effort 로 갱신한다. 상세에서는 작업 문서가 정본이다.
	r.Status, r.JobState, r.Stage = s.Status, s.JobState, s.Stage
	r.Progress, r.Error, r.HasAudio = s.Progress, s.Error, s.HasAudio
	if j.Audio.Object == "" {
		return
	}
	url, e := h.audio.SignedGet(ctx, j.Audio.Object, h.clock().Add(audioURLTTL))
	if e != nil {
		// 재생 주소가 없어도 분석 결과는 보여 준다. 원인은 로그에만 남긴다.
		log.Printf("calls: 재생 URL 발급 실패 call=%s: %v", r.CallID, e)
		return
	}
	r.AudioURL = &url
}

// audioMeta 는 검증을 통과한 통화 메타다.
type audioMeta struct {
	FileName   string
	Duration   float64
	RecordedAt time.Time
}

// validateAudioMeta 는 기존 통화 레코드 규약과 **같은 규칙**을 쓴다. 여기서만 느슨해지면
// 전사가 끝난 뒤 저장 단계에서 거부돼 ASR 요금만 날린다.
func validateAudioMeta(in *uploadURLRequest, id string) (audioMeta, error) {
	var m audioMeta
	if !recipients.ValidateID(id) || !validRunes(in.Contact.Name, maxNameRunes, true) || !phonePattern.MatchString(in.Contact.Phone) || !validText(in.Call.FileName, 1024, true) {
		return m, invalid
	}
	if in.Contact.RecipientID != nil && !recipients.ValidateID(*in.Contact.RecipientID) {
		return m, invalid
	}
	recorded, err := time.Parse(time.RFC3339Nano, in.Call.RecordedAt)
	// 기기 시계가 서버보다 조금 빠른 것은 정상이다(기기 경로와 같은 허용치).
	if err != nil || recorded.After(time.Now().Add(maxClockSkew)) {
		return m, invalid
	}
	m.FileName, m.RecordedAt = in.Call.FileName, recorded.UTC()
	if d := in.Call.Duration; d != nil {
		if *d < 0 || *d > 86400 || *d != *d {
			return m, invalid
		}
		m.Duration = *d
	}
	return m, nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// writeJobError 는 작업 조회 실패를 응답으로 바꾼다. NotFound 는 남의 통화일 때도 나온다.
func writeJobError(w http.ResponseWriter, err error, msg string) {
	if status.Code(err) == codes.NotFound {
		httpx.WriteError(w, 404, "CALL_NOT_FOUND", msg)
		return
	}
	httpx.WriteError(w, 500, httpx.CodeInternal, "요청을 처리하지 못했어요")
}

// registerAudio 는 오디오 3종 라우트를 건다. 호출부가 nil 을 넘기면(버킷 미설정) 아무것도 걸지 않는다.
func (h *audioHandler) register(mux *http.ServeMux, guard func(http.Handler) http.Handler) {
	wrap := func(fn func(http.ResponseWriter, *http.Request, string, string)) http.Handler {
		return guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			uid := auth.UserID(r.Context())
			if uid == "" {
				httpx.WriteError(w, 401, httpx.CodeUnauthorized, "로그인이 필요해요")
				return
			}
			id := r.PathValue("id")
			if !validID(id) {
				httpx.WriteError(w, 400, httpx.CodeValidationFailed, "통화 ID를 확인해 주세요")
				return
			}
			fn(w, r, uid, id)
		}))
	}
	mux.Handle("POST /calls/{id}/audio/upload-url", wrap(h.uploadURL))
	mux.Handle("POST /calls/{id}/audio/complete", wrap(h.complete))
	mux.Handle("POST /calls/{id}/reanalyze", wrap(h.reanalyze))
}
