package calls

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sslim7/nature-was/internal/httpx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// transcript.go 는 **기기가 받아쓴 전사문을 받아 분석 단계로 넘기는** 경로다.
//
//	앱 → POST /calls/{id}/audio/upload-url            (서명 URL 발급 — 서버 경로와 같다)
//	앱 → PUT  <서명 URL>                               (앱 → GCS 직접. 재생 기능이 이 원본을 쓴다)
//	앱 → POST /calls/{id}/audio/complete {asr:client}  (서버 ASR 을 하지 않고 기다린다)
//	앱 → POST /calls/{id}/transcript                   (전사문 전달 → 분석 단계로 진입)  ← 여기
//	앱 → GET  /calls/{id}                              (진행 상태 폴링 — 서버 경로와 같다)
//
// 🔴 **분석 파이프라인을 새로 만들지 않는다.** 하는 일은 전사문을 기존 자리(작업 문서의
// transcript 하위 컬렉션)에 넣고 상태를 TRANSCRIBED 로 놓는 것뿐이고, 그 뒤는 tick 이
// 서버 경로와 **같은 코드로** 이어받는다(§pipeline.stepAnalyze). 재분석(reanalyze)도 정확히
// 같은 지점으로 들어온다 — 분석 경로가 둘이 되면 한쪽만 고쳐진 버그가 조용히 남는다.
//
// 🔴 **받아쓰기 비용은 0원이다.** 여기서 j.Usage 를 건드리지 않는 것이 그 장치다.
// 통화 길이를 usage.audioSeconds 에 옮겨 적으면 쓰지도 않은 받아쓰기 요금이 상세 화면의
// 원가에 뜬다(§cost.go, §audio.go jobFromRecord 가 같은 이유로 Usage 를 비운다).

// transcript 는 POST /calls/{id}/transcript 다.
//
// 거절 규칙은 넷이다:
//
//	404 — 본인 소유 작업이 없다(남의 통화 ID 도 404 다. 존재 사실을 알려 주지 않는다).
//	409 — asr:"client" 로 만들어진 작업이 아니다. 🔴 서버가 받아쓰는 중인 통화에 전사문을
//	      밀어넣게 두면 공급자 결과와 앱 결과가 경쟁해 어느 쪽이 남는지 알 수 없다.
//	409 — 이미 COMPLETED 다. 다시 분석하려면 기존 reanalyze 를 쓴다.
//	400 — 전사문이 비었다. 빈 원문을 분석에 넘겨 봐야 지어낸 요약만 나온다.
//
// 그리고 **같은 요청을 두 번 보내면 두 번째는 200** 이다. 앱이 응답을 못 받고 재시도하는
// 경우가 실제로 있어서(audio/complete 가 이미 그렇게 되어 있다) 여기서 409 를 돌려주면
// 사용자는 이미 잘 처리된 통화를 실패로 본다.
func (h *audioHandler) transcript(w http.ResponseWriter, r *http.Request, uid, id string) {
	j, err := h.load(r.Context(), uid, id)
	if err != nil {
		writeJobError(w, err, "통화를 찾을 수 없어요")
		return
	}
	if !j.clientASR() {
		httpx.WriteError(w, 409, "CALL_NOT_CLIENT_ASR", "서버가 받아쓰는 통화예요")
		return
	}
	if j.State == stateCompleted {
		// 🔴 끝난 통화의 원문을 갈아 끼우면 이미 사용자가 본 분석과 원문이 어긋난다.
		httpx.WriteError(w, 409, "CALL_ALREADY_COMPLETED", "이미 분석이 끝난 통화예요")
		return
	}
	switch j.State {
	case stateASRRunning:
		// 정상 경로 — 기기가 받아쓰기를 끝내고 돌아왔다.
	case stateTranscriptionFailed:
		// 🔴 6시간 마감을 넘겨 정리된 작업이다(CLIENT_TRANSCRIPT_TIMEOUT). 늦게라도 전사문이
		// 왔다면 받는다 — 여기서 거절하면 그 통화는 원문도 분석도 없이 실패로만 남는데,
		// 정작 원문은 방금 요청 본문에 들어 있다.
	default:
		// TRANSCRIBED / ANALYZING / ANALYSIS_FAILED — 이미 전사문을 받은 작업이다.
		// 앱의 재시도이므로 지금 상태를 그대로 돌려준다(멱등).
		httpx.WriteJSON(w, 200, j.summaryRecord())
		return
	}

	in, err := decodeTranscriptBody(w, r)
	if err != nil {
		switch {
		case errors.Is(err, errTooLarge):
			httpx.WriteError(w, 413, "CALL_TOO_LARGE", "전사문이 너무 커요")
		case errors.Is(err, errEmptyTranscript):
			httpx.WriteError(w, 400, "CALL_EMPTY_TRANSCRIPT", "받아쓴 내용이 비어 있어요")
		default:
			httpx.WriteError(w, 400, httpx.CodeValidationFailed, "전사문 형식을 확인해 주세요")
		}
		return
	}
	b, err := marshalCompact(in)
	if err != nil {
		httpx.WriteError(w, 400, httpx.CodeValidationFailed, "전사문 형식을 확인해 주세요")
		return
	}
	if len(b) > maxTranscriptStoreBytes {
		httpx.WriteError(w, 413, "CALL_TOO_LARGE", "전사문이 너무 커요")
		return
	}
	// 저장은 서버 경로와 **같은 자리, 같은 함수**다. 샤딩과 낡은 조각 정리가 이미 여기 있다.
	shards, err := h.jobs.PutTranscript(r.Context(), id, b)
	if err != nil {
		httpx.WriteError(w, 500, httpx.CodeInternal, "받아쓴 내용을 저장하지 못했어요")
		return
	}

	now := h.clock()
	j.TranscriptShards = shards
	j.State = stateTranscribed
	j.Stage = j.stageName()
	j.Progress = progressOf(stateTranscribed)
	// 분석은 이제부터다. 지난 시도의 흔적이 남아 있으면 첫 tick 이 곧바로 상한에 걸린다
	// (reanalyze 가 같은 값들을 되돌리는 것과 같은 이유다).
	j.HasAnalysis = false
	j.AnalysisAttempt = 0
	j.DeferCount = 0
	j.ErrorCode, j.ErrorKind, j.ErrorAt = "", "", time.Time{}
	j.UpdatedAt = now
	// 🔴 마감으로 밀어 뒀던 예약을 지금으로 되돌린다. 그대로 두면 분석이 6시간 뒤에 시작된다.
	j.NextAttemptAt = now
	// ⚠️ ASRModel/ASRProvider 는 채우지 않는다. 기기가 쓴 모델 이름을 여기에 적으면
	// **서버 파이프라인이 쓴 값인 양** 상세 화면에 붙는다. 비워 두면 summaryRecord 가
	// 그 칸을 만들지 않고, 서버 LLM 이 답하는 순간 분석 쪽 값으로 채워진다(§job.summaryRecord).
	if err := h.jobs.Put(r.Context(), j); err != nil {
		httpx.WriteError(w, 500, httpx.CodeInternal, "받아쓴 내용을 저장하지 못했어요")
		return
	}
	// 앱이 즉시 「내용 정리하는 중」을 보게 한다. 실패해도 다음 tick 이 다시 맞춘다
	// (reanalyze 와 같은 처리다).
	if err := h.records.Touch(r.Context(), uid, id, func(cur *Record) {
		s := j.summaryRecord()
		cur.Status, cur.JobState, cur.Stage, cur.Progress, cur.Error = s.Status, s.JobState, s.Stage, s.Progress, nil
	}); err != nil && status.Code(err) != codes.NotFound {
		log.Printf("calls: 기기 전사문 상태 갱신 실패 call=%s: %v", id, err)
	}
	httpx.WriteJSON(w, 200, j.summaryRecord())
}

// errEmptyTranscript 는 원문이 비어 있다는 표시다. 형식 오류(400 VALIDATION_FAILED)와 나눈다 —
// 앱이 「다시 받아쓰게 할 일」과 「보내는 모양을 고칠 일」을 구분할 수 있어야 한다.
var errEmptyTranscript = errors.New("calls: 전사문이 비었다")

// decodeTranscriptBody 는 요청 본문을 전사문으로 읽고 **기기 경로(PUT /calls/{id})와 같은
// 규칙으로** 검증한다.
//
// 🔴 httpx.DecodeBody 를 쓰지 않는다. 그쪽 상한은 64 KiB 인데 28분 통화의 전사문은 그보다
// 크다 — 그 상한에 걸리면 본문이 잘린 채 JSON 오류로 떨어져 **원인이 「형식 오류」로 보인다.**
// PUT /calls/{id} 와 같은 6 MiB 상한을 쓰고, 넘으면 413 으로 구분해 돌려준다.
func decodeTranscriptBody(w http.ResponseWriter, r *http.Request) (*Transcript, error) {
	var in Transcript
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	// ⚠️ DisallowUnknownFields 를 걸지 않는다. 기기가 세그먼트에 자기 필드(신뢰도 등)를
	// 하나 붙였다는 이유로 이미 끝난 받아쓰기를 버리게 할 이유가 없다 — 우리가 아는 필드만
	// 읽고 나머지는 무시한다. 대신 아는 필드의 검증은 기기 경로와 똑같이 엄격하다.
	if err := dec.Decode(&in); err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			return nil, errTooLarge
		case errors.Is(err, io.EOF):
			// 본문 자체가 없는 요청이다. 형식 오류가 아니라 **원문이 없는 것**이라고
			// 말해 주는 편이 앱이 할 일(다시 받아쓰기)을 고르는 데 쓸모 있다.
			return nil, errEmptyTranscript
		}
		return nil, invalid
	}
	if in.Segments == nil {
		// 세그먼트 없이 본문만 보내는 것도 받는다(whisper 설정에 따라 나오지 않을 수 있다).
		// 저장 모양은 기기 경로와 같게 빈 배열로 맞춘다 — decodeTranscript 와 같은 처리다.
		in.Segments = []Segment{}
	}
	if strings.TrimSpace(in.Text) == "" {
		return nil, errEmptyTranscript
	}
	if !validTranscript(&in) {
		return nil, invalid
	}
	return &in, nil
}
