package calls

import (
	"context"
	"errors"
	"io"
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
// complete 에 `{"asr":"client"}` 를 보내면 **서버가 받아쓰지 않고 기다린다** — 폰이 whisper 로
// 받아쓴 전사문을 POST /calls/{id}/transcript 로 올리고 분석부터 이어간다(§transcript.go).
// 오디오 원본은 그때도 그대로 올라간다(재생 기능이 쓴다). 달라지는 것은 받아쓰기 주체뿐이다.
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

// completeRequest 는 POST /calls/{id}/audio/complete 의 **선택** 본문이다.
//
// 🔴 본문이 아예 없는 요청이 정상이다. 구버전 앱과 웹은 바디 없이 부르고 있고(앱
// `src/lib/call-api.ts` 의 complete), 그 요청은 여기서 한 글자도 다르게 동작하면 안 된다 —
// 사용자가 로컬 받아쓰기를 끄면 지금과 완전히 똑같이 도는 것이 이 기능의 안전망이다.
type completeRequest struct {
	// ASR 은 **받아쓰기를 누가 하는가**다. 비어 있으면 "server" 다.
	//
	//	server — 지금까지의 동작. 서버가 공급자로 전사하고 이어서 분석한다.
	//	client — 폰이 whisper 로 받아쓴다. 서버는 전사문이 올 때까지 기다리다가
	//	         POST /calls/{id}/transcript 를 받고 분석부터 이어간다.
	//
	// 🔴 오디오 원본은 두 경우 모두 지금처럼 GCS 에 올라간다(재생 기능이 그것을 쓴다).
	// 달라지는 것은 받아쓰기를 누가 하느냐뿐이다.
	ASR string `json:"asr"`
}

const (
	asrServer = "server"
	asrClient = "client"
)

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
	// pricing 은 원가 환산 단가다(§cost.go). 비어 있으면 상세 응답에 비용이 붙지 않는다 —
	// 🔴 「모름」이지 「0원」이 아니다.
	pricing Pricing
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
	// 🔴 본문이 없으면 io.EOF 다 — **그것이 기존 요청의 모양이고 정상이다.**
	// 빈 본문을 400 으로 만들면 지금 돌고 있는 앱과 웹이 전부 멈춘다.
	var in completeRequest
	if derr := httpx.DecodeBody(r, &in); derr != nil && !errors.Is(derr, io.EOF) {
		httpx.WriteError(w, 400, httpx.CodeValidationFailed, "요청 값을 확인해 주세요")
		return
	}
	switch in.ASR {
	case "", asrServer, asrClient:
	default:
		// 모르는 값을 서버 받아쓰기로 눙치지 않는다. 앱이 오타를 냈다면 요금이 나가는
		// 쪽으로 조용히 흐르는 것보다 400 으로 알려 주는 편이 낫다.
		httpx.WriteError(w, 400, httpx.CodeValidationFailed, "받아쓰기 방식을 확인해 주세요")
		return
	}

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
	j.NextAttemptAt = now
	if in.ASR == asrClient {
		// 🔴 **큐에 넣지 않는다.** 폰이 지금 받아쓰는 중이므로 서버가 할 일은 기다리는 것뿐이다.
		// 상태를 ASR_RUNNING 으로 두는 이유는 사실이 그렇기 때문이고(누가 하느냐만 다르다),
		// CallStatus 는 닫힌 집합이라 새 값을 만들지 않는다 — 앱은 그대로 TRANSCRIBING 을 본다.
		//
		// 🔴 다음 확인 시각을 마감(지금 + 6시간)으로 둔다. now 로 두면 이 작업이 매분 tick 에
		// 잡혀 6시간 내내 아무 일도 하지 않는 조회·쓰기만 쌓는다. 스윕 쿼리가 nextAttemptAt
		// 오름차순이라 미래로 밀어 둔 작업은 다른 통화의 순서를 막지 않는다(§job.Claim).
		j.State = stateASRRunning
		j.ClientASRAt = now
		j.NextAttemptAt = now.Add(clientTranscriptTimeout)
	}
	j.Stage = j.stageName()
	j.Progress = progressOf(j.State)
	j.UpdatedAt = now
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

// jobFromRecord 는 **작업 문서가 없는 옛 통화**를 위해 통화 레코드에서 작업을 만들어 낸다.
//
// 기기에서 받아쓰기(whisper.rn)와 요약(온디바이스 LLM)까지 끝내고 PUT /calls/{id} 로 결과만
// 올린 통화에는 callJobs 문서가 **아예 없다.** 그런 통화를 서버 LLM 으로 다시 분석하려면
// 파이프라인이 읽을 작업 문서가 있어야 하는데, 없는 값을 그럴듯하게 채우면 화면이 조용히
// 거짓말을 한다. 그래서 **레코드가 이미 아는 값만** 옮기고 나머지는 비운 채로 둔다.
//
// 🔴 Audio 를 비워 두는 것이 이 함수의 핵심이다. 이 통화의 녹음 파일은 GCS 에 없다 —
// 기기가 자기 파일로 받아쓰고 결과만 보냈기 때문이다. 여기에 그럴듯한 object 경로를 채우면
// 파이프라인이 **없는 파일로 ASR 을 부른다**: 실패로 끝나기 전에 오디오 업로드와 전사 제출이
// 이미 나가므로 요금은 요금대로 나가고, 재시도 상한까지 세 번 되풀이한다.
// pipeline.stepStart 와 requeueASR 의 AUDIO_MISSING 가드가 그 뒤를 받친다.
//
// 🔴 Usage 도 비운다. 서버 ASR 을 한 번도 부른 적이 없으므로 AudioSeconds 는 **0 이 맞다** —
// 서버가 받아쓰기에 쓴 돈이 실제로 0원이다. 통화 길이를 여기에 옮겨 적으면 쓰지도 않은
// 받아쓰기 요금이 상세 화면의 원가에 그대로 뜬다(§cost.go). 토큰은 이번 LLM 호출이 채운다.
//
// ⚠️ 모델 이름(ASRModel/LLMModel/PromptVersion)도 비운다. 레코드의 ai.model 에는 기기가 쓴
// `Qwen3-0.6B-Q8_0` 이 들어 있는데 그것을 작업 문서로 옮기면, 서버가 한 일이 아닌 모델 이름이
// **서버 파이프라인의 값인 양** 상세 화면에 붙는다. 비워 두면 summaryRecord 가 ai 를 아예
// 만들지 않아(§job.go) 레코드의 기존 값이 그대로 남고, 서버 LLM 이 답하는 순간 새 값으로 바뀐다.
func jobFromRecord(uid, id string, rec *Record, now time.Time) (*job, error) {
	// 🔴 통화일시는 반드시 레코드 값을 잇는다. 이 값은 분석이 끝날 때 summaryRecord 를 거쳐
	// **통화 레코드에 그대로 덮어써진다** — 비워 두면 0001-01-01 이 저장되고, 목록이
	// recordedAt 순이라 그 통화는 화면에서 사라진 것처럼 맨 끝으로 밀린다.
	recorded, err := time.Parse(time.RFC3339Nano, rec.Call.RecordedAt)
	if err != nil {
		return nil, err
	}
	j := &job{
		UID: uid, CallID: id,
		State: stateTranscribed, Stage: stageOf(stateTranscribed), Progress: progressOf(stateTranscribed),
		ContactName: rec.Contact.Name, ContactPhone: rec.Contact.Phone, ContactRecipientID: derefString(rec.Contact.RecipientID),
		FileName: rec.Call.FileName, RecordedAt: recorded.UTC(),
		CreatedAt: now, UpdatedAt: now, NextAttemptAt: now,
	}
	// createdAt 도 레코드 값을 잇는다. 읽을 수 없으면 지금 시각으로 둔다 — 이 값은
	// 응답에만 쓰이고 저장 순서를 정하지 않아, 통화일시와 달리 되돌릴 수 있는 오차다.
	if t, cerr := time.Parse(time.RFC3339Nano, rec.CreatedAt); cerr == nil {
		j.CreatedAt = t.UTC()
	}
	if d := rec.Call.Duration; d != nil && *d > 0 {
		j.Duration = *d
	}
	return j, nil
}

// hasTranscript 는 다시 분석할 원문이 레코드에 남아 있는지다.
func hasTranscript(rec *Record) bool {
	return rec.Transcript != nil && strings.TrimSpace(rec.Transcript.Text) != ""
}

// reanalyze 는 **오디오를 다시 전사하지 않고** 분석만 다시 돌린다.
//
// 🔴 ASR 이 이 파이프라인에서 가장 비싼 단계이고, 재분석이 필요한 이유는 대개 프롬프트가
// 바뀌었거나 LLM 이 실패했을 때다. 전사문은 그대로 쓸 수 있다.
// 오디오 원본은 366일 뒤 사라지지만 **이 경로는 전사문만 쓰므로 그 뒤에도 동작한다.**
//
// 조건은 하나뿐이다 — **다시 분석할 원문이 있을 것.** 작업 문서가 없는 기기 경로 통화도
// 레코드에 원문이 있으면 여기서 작업을 만들어 이어간다(jobFromRecord).
func (h *audioHandler) reanalyze(w http.ResponseWriter, r *http.Request, uid, id string) {
	// 🔴 여기서 load 를 쓰면 안 된다. load 는 「남의 것」과 「없는 것」을 똑같이 NotFound 로
	// 돌려주는데, 그 둘을 뭉뚱그린 채 「없으면 만든다」로 가면 **남의 통화 ID 로 재분석을 눌러
	// 그 작업 문서를 통째로 덮어쓸 수 있다**(uploadURL 과 같은 함정이다). 존재 여부와 소유권을
	// 여기서만 따로 보고, 응답은 여전히 404 라 바깥에서는 구분되지 않는다.
	j, err := h.jobs.Get(r.Context(), id)
	// rec 은 **읽었을 때만** 채운다. 원문은 25분 통화면 수십만 바이트라, 아래에서 한 번 더
	// 읽으면 같은 조각들을 통째로 두 번 내려받게 된다.
	var rec *Record
	switch {
	case status.Code(err) == codes.NotFound:
		got, gerr := h.records.Get(r.Context(), uid, id)
		if gerr != nil {
			// 작업도 레코드도 없다 = 이 사용자에게 그런 통화가 없다.
			writeJobError(w, gerr, "통화를 찾을 수 없어요")
			return
		}
		if !hasTranscript(&got) {
			httpx.WriteError(w, 409, "CALL_NO_TRANSCRIPT", "다시 분석할 원문이 없어요")
			return
		}
		rec = &got
		j, err = jobFromRecord(uid, id, rec, h.clock())
		if err != nil {
			// 우리가 저장한 레코드를 우리가 읽지 못한 것이다. 통화일시를 지어내 덮어쓰느니 멈춘다.
			log.Printf("calls: 레코드에서 작업을 만들지 못했다 call=%s: %v", id, err)
			httpx.WriteError(w, 500, httpx.CodeInternal, "재분석을 예약하지 못했어요")
			return
		}
	case err != nil:
		httpx.WriteError(w, 500, httpx.CodeInternal, "재분석을 예약하지 못했어요")
		return
	case j.UID != uid:
		httpx.WriteError(w, 404, "CALL_NOT_FOUND", "통화를 찾을 수 없어요")
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
		if rec == nil {
			got, gerr := h.records.Get(r.Context(), uid, id)
			if gerr != nil || !hasTranscript(&got) {
				httpx.WriteError(w, 409, "CALL_NO_TRANSCRIPT", "다시 분석할 원문이 없어요")
				return
			}
			rec = &got
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
	j.Stage = j.stageName()
	j.Progress = progressOf(stateTranscribed)
	j.HasAnalysis = false
	j.AnalysisAttempt = 0
	// 🔴 미루기 횟수도 되돌린다. 예산 부족 상한(maxDefers)에 걸려 멈춘 작업을 사람이 다시
	// 돌릴 때 이것이 남아 있으면, 첫 미룸에서 곧바로 같은 자리로 되돌아간다 —
	// 버튼을 눌러도 아무 일도 일어나지 않는 것처럼 보인다.
	j.DeferCount = 0
	j.ErrorCode, j.ErrorKind, j.ErrorAt = "", "", time.Time{}
	j.UpdatedAt = now
	j.NextAttemptAt = now
	if err := h.jobs.Put(r.Context(), j); err != nil {
		httpx.WriteError(w, 500, httpx.CodeInternal, "재분석을 예약하지 못했어요")
		return
	}
	// 앱이 즉시 「내용 정리하는 중」을 보게 한다. 실패해도 다음 tick 이 다시 맞춘다.
	if err := h.records.Touch(r.Context(), uid, id, func(cur *Record) {
		s := j.summaryRecord()
		cur.Status, cur.JobState, cur.Stage, cur.Progress, cur.Error = s.Status, s.JobState, s.Stage, s.Progress, nil
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
// 비용도 같은 자리에서 붙는다: 금액의 근거인 사용량은 **작업 문서에만** 있기 때문에,
// 목록에 싣는 것은 곧 목록 한 페이지마다 작업 문서를 그만큼 더 읽는다는 뜻이다.
func (h *audioHandler) enrichDetail(ctx context.Context, uid string, r *Record) {
	j, err := h.jobs.Get(ctx, r.CallID)
	// 작업 문서가 없는 통화는 기기 업로드 경로로 저장된 것이다 — 정상이므로 그대로 둔다.
	// 🔴 그런 통화에는 사용량 자체가 없으므로 비용도 붙지 않는다. **0원이 아니라 「모름」이다.**
	if err != nil || j.UID != uid {
		return
	}
	s := j.summaryRecord()
	// 저장된 요약은 파이프라인이 best-effort 로 갱신한다. 상세에서는 작업 문서가 정본이다.
	r.Status, r.JobState, r.Stage = s.Status, s.JobState, s.Stage
	r.Progress, r.Error, r.HasAudio = s.Progress, s.Error, s.HasAudio
	// 🔴 **조건 없이 대입한다.** 단가나 사용량이 없으면 costOf 가 nil 을 돌려주고, 그 nil 이
	// 저장 레코드에 혹시 남아 있을 값까지 함께 지운다 — 「모름」을 확실히 모름으로 내보내는
	// 자리가 여기뿐이다.
	r.Cost = h.pricing.costOf(j.Usage)
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
	// 기기 받아쓰기 경로의 마지막 단계다(§transcript.go). 서버 받아쓰기 경로는 이 라우트를 쓰지 않는다.
	mux.Handle("POST /calls/{id}/transcript", wrap(h.transcript))
}
