package calls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sslim7/nature-was/internal/callai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pipeline.go 는 서버 통화분석의 **상태 기계**다. HTTP 도 Firestore 도 여기서는 보이지 않는다
// — 저장소와 공급자를 전부 인터페이스로 받으므로 에뮬레이터·네트워크 없이 그대로 돌아간다.
//
//	AWAITING_UPLOAD → QUEUED → ASR_RUNNING → ASR_POLLING ⇄ ASR_POLLING → TRANSCRIBED → ANALYZING → COMPLETED
//	                                      ↘ TRANSCRIPTION_FAILED                    ↘ ANALYSIS_FAILED
//
// 🔴 **TRANSCRIBED 는 독립 상태다.** 분석만 실패해도 전사를 다시 하지 않는다 — ASR 이 이
// 파이프라인에서 가장 비싸고 느린 단계라, 여기서 한 번 더 부르면 요금이 그대로 두 배가 된다.

// recordStore 는 파이프라인이 통화 레코드(users/{uid}/calls)에 대해 필요로 하는 것 전부다.
// handler 가 쓰는 repository 와 일부러 나눠 뒀다 — 기기 경로의 Save 계약을 건드리지 않기 위해서다.
type recordStore interface {
	Get(ctx context.Context, uid, id string) (Record, error)
	SaveOverwrite(ctx context.Context, uid string, p *payload) (Record, error)
	Touch(ctx context.Context, uid, id string, mut func(*Record)) error
}

// audioStore 는 원본 오디오 객체 저장소다.
//
// 🔴 **버킷 메타데이터를 읽는 연산은 여기 없다.** 런타임 서비스 계정에는
// `roles/storage.objectAdmin` 만 있고 `storage.buckets.get` 이 **없다**(redhead-terraform
// `apps/nature/` 주석 참고). `bucket.Attrs` 를 부르는 코드를 새로 쓰는 순간 최소 권한을
// 넓혀야 한다 — 기동 시 버킷 존재 확인, 헬스체크에서의 버킷 조회 전부 금지다.
type audioStore interface {
	// SignedPut 은 앱이 직접 PUT 할 V4 서명 URL 을 만든다.
	SignedPut(ctx context.Context, object, contentType string, expires time.Time) (string, error)
	// SignedGet 은 녹음 재생용 V4 서명 GET URL 을 만든다. 서명은 로컬 계산이라 GCS 왕복이 없다.
	SignedGet(ctx context.Context, object string, expires time.Time) (string, error)
	// Size 는 객체 크기를 돌려준다. 없으면 errAudioMissing.
	Size(ctx context.Context, object string) (int64, error)
	// Open 은 객체를 **스트리밍**으로 연다. 🔴 서버는 오디오 바이트를 메모리에 모으지 않는다.
	Open(ctx context.Context, object string) (io.ReadCloser, error)
	// Bucket 은 버킷 이름이다(작업 문서에 기록하는 용도뿐, GCS 를 부르지 않는다).
	Bucket() string
}

type stepResult int

const (
	stepProgressed stepResult = iota // 같은 tick 에서 바로 다음 단계로 갈 수 있다
	stepWait                         // 공급자가 아직 진행 중 — 잠시 뒤 다시 폴링
	stepDone                         // 이 작업은 이번 tick 에서 끝났다(완료·재시도 예약)
	stepFailed                       // 확정 실패
)

type phase int

const (
	phaseASR phase = iota
	phaseLLM
)

func (ph phase) String() string {
	if ph == phaseLLM {
		return "LLM"
	}
	return "ASR"
}

// stepBudget 은 한 단계가 볼 수 있는 시간 두 개다.
//
// # 🔴 left 하나로는 판단할 수 없다
//
// 예산이 모자라 단계를 미룰 때, 그 원인은 둘 중 하나다:
//
//   - **앞 통화가 이 tick 의 시간을 먼저 썼다.** 정상 동작이다. 미뤄진 작업은 lease 를 풀고
//     나가므로 nextAttemptAt 이 now 가 되고, Claim 이 nextAttemptAt 오름차순이라 **다음
//     tick 에서 먼저 집힌다.** 큐가 밀리는 날 이것을 실패로 세면 멀쩡한 통화가 줄줄이 버려진다.
//   - **빈 tick 이어도 이 단계는 들어가지 않는다.** 이건 설정이 깨진 것이다(예: 공급자가
//     느려져 llmCallNeed 가 tickBudget 을 넘었다). 사람이 손대기 전까지 영원히 미뤄지므로
//     상한을 두고 종료 상태로 보내 알린다.
//
// 그 둘을 가르는 것이 total 이다 — total 은 이 tick 이 **시작할 때** 가졌던 예산이다.
type stepBudget struct {
	left  time.Duration // 지금 남은 시간
	total time.Duration // 이 tick 이 시작할 때의 예산
}

// window 는 need 만큼 여유가 있으면 이 호출에 걸 창을 돌려준다.
//
// 🔴 **모자라면 부르지 않는다.** 시작해 놓고 중간에 끊으면 공급자 쪽 계산은 그대로
// 진행되어 요금은 나가는데 우리는 결과를 못 받는다. 2026-09-18 에 25분짜리 통화가
// 이 자리에서 깨졌다 — 남은 시간을 보지 않고 LLM 을 불렀고, 돌아온 것은 타임아웃뿐이었다.
func (b stepBudget) window(need time.Duration) (time.Duration, bool) {
	if b.left < need {
		return 0, false
	}
	return b.left - saveReserve, true
}

// errBudget 은 「이번 tick 의 시간이 끊겼다」는 표시다. **공급자 실패가 아니다.**
var errBudget = errors.New("calls: tick 예산이 끊겼다")

// classifyCall 은 공급자 호출 실패를 **누구의 데드라인이 끊겼는가** 로 나눈다.
//
// 🔴 이 구분이 없으면 두 가지가 한 덩어리가 된다: 「우리가 시간을 못 냈다」와 「공급자가
// 답을 못 했다」. 앞쪽은 실패가 아니라 미룸이고(시도 횟수를 쓰면 안 된다), 뒤쪽은 진짜
// 실패다(시도 횟수를 쓰고 백오프를 걸어야 한다). 2026-09-18 에는 둘 다 한 줄
// `errors.Is(err, context.DeadlineExceeded)` 에 걸려 재시도 한 번 없이 확정 실패가 됐다.
func classifyCall(parent context.Context, err error, window time.Duration) error {
	if parent.Err() != nil {
		// tick 예산이나 Cloud Run 요청이 먼저 끊겼다. 우리 사정이다.
		return fmt.Errorf("%w: %v", errBudget, parent.Err())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// 부모는 살아 있는데 끊겼다 = 우리가 이 호출에 준 창이 끝났다.
		// 그 창은 need 만큼 확보하고 들어간 시간이므로, 못 끝낸 것은 공급자 쪽 사정이다.
		return &callai.Error{
			Kind: callai.KindRetryable, Code: callai.CodeProviderTimeout,
			Message: fmt.Sprintf("공급자가 %.0f초 안에 응답하지 않았다", window.Seconds()), Err: err,
		}
	}
	if errors.Is(err, context.Canceled) {
		// 부모가 살아 있는 취소는 우리가 만든 자식 ctx 말고는 올 데가 없다.
		// 어느 쪽이든 공급자 실패로 세지 않는다 — 통화를 버리지 않는 쪽이 안전한 오독이다.
		return fmt.Errorf("%w: %v", errBudget, err)
	}
	return err
}

// pollWait 는 폴링 사이 간격이다. 공급자 전사는 보통 수십 초가 걸리므로 더 짧게 찔러 봐야
// 쿼터만 쓴다. 🔴 time.Sleep 이 아니라 ctx 를 존중하는 타이머로 기다린다.
const pollWait = 5 * time.Second

// maxErrLogRunes 는 로그에 남길 공급자 에러 메시지의 상한이다.
// 🔴 공급자 메시지에 우리가 보낸 입력이 되비쳐 들어올 수 있다. 진단에 필요한 것은
// 앞머리뿐이고, 통화 내용이 로그에 남는 경로는 하나도 만들지 않는다.
const maxErrLogRunes = 200

// ── 사용자에게 나가는 실패 코드 ───────────────────────────────────────────────
//
// 🔴 ErrorCode 는 **종료 상태에서만** 통화 레코드의 error 로 나가고(summaryRecord 참고),
// 앱은 그것을 코드별 문구로 바꿔 화면에 쓴다(jayeon-app `src/lib/call-errors.ts`).
// 그러므로 이 값은 진단용 문자열이기 전에 **사용자에게 할 말**이다.
//
// 🔴 **자동으로 회복될 실패에는 코드를 내보내지 않는다.** 재시도 예약 중에는 상태가 종료
// 상태가 아니므로 error 가 비어 나가고, 앱은 「분석 중」을 그대로 보여 준다. 그 약속이
// 깨지면 사용자는 서버가 이미 하고 있는 일을 손으로 누르게 된다 — 2026-09-18 에 화면에
// 뜬 「일시적인 오류로 멈췄습니다. 잠시 뒤 다시 시도해 주세요」가 정확히 그 증상이었다.
const (
	// codeRetriesExhausted 는 **자동 재시도를 다 썼다**는 뜻이다.
	//
	// 예전에는 이 자리에 마지막 공급자 코드나 "MAX_ATTEMPTS" 가 그대로 나갔다. 앱은
	// 그중 재시도 가능 분류를 「일시적인 오류입니다. 잠시 뒤 다시 시도해 주세요」로 옮겼는데,
	// 그건 사용자에게 할 말이 아니다 — 서버가 이미 여러 번 시도했고 더 할 것이 없어서
	// 멈춘 상태다. 마지막 공급자 코드·request_id·HTTP status 는 로그와 작업 문서에 남는다.
	codeRetriesExhausted = "RETRIES_EXHAUSTED"
	// codeBudgetTooSmall 은 tick 예산 안에 그 단계가 들어가지 않는다는 뜻이다.
	// 사용자가 할 수 있는 일은 없고, 고칠 사람은 우리다(예산 상수 또는 Cloud Run 타임아웃).
	codeBudgetTooSmall = "BUDGET_TOO_SMALL"
)

// userCode 는 종료 상태에서 앱에 내보낼 코드를 고른다.
//
// 🔴 공급자 코드(`DataInspectionFailed`, `InputDownloadFailed…`)를 그대로 내보내면 앱은
// 뜻을 모르는 문자열을 받아 「알 수 없는 오류입니다」로 떨어뜨린다. 그런데 이 두 분류는
// **사용자가 할 일이 서로 다르다** — 내용 필터에 걸린 통화는 다시 눌러도 같고, 오디오를
// 못 읽은 통화는 다시 등록해야 한다. 그 차이는 사용자에게 말해 줘야 하는 것이므로
// 분류 이름으로 옮긴다(앱 `src/lib/call-errors.ts` 의 REASONS 가 이 이름을 안다).
//
// 나머지(우리가 만든 코드, 공급자 영구 실패 코드)는 그대로 둔다 — 진단에 쓰인다.
func userCode(kind, code string) string {
	switch kind {
	case callai.KindContentFiltered.String(), callai.KindInputUnavailable.String():
		return kind
	}
	return code
}

type pipeline struct {
	jobs    jobRepo
	records recordStore
	audio   audioStore
	asr     callai.Transcriber
	llm     callai.Analyzer

	asrProvider string
	llmProvider string
	language    string

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

func (p *pipeline) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// wait 는 ctx 가 끝나면 즉시 돌아온다. 🔴 time.Sleep 을 쓰면 요청이 취소돼도 그 시간만큼
// 인스턴스를 붙잡고 있고, Cloud Run 은 그 사이 CPU 를 스로틀한다.
func (p *pipeline) wait(ctx context.Context, d time.Duration) error {
	if p.sleep != nil {
		return p.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// step 은 작업을 한 단계 전진시키고 그 결과를 저장한다.
//
// 반환 error 는 **저장조차 못 했을 때**다. 공급자 실패는 error 가 아니라 상태로 바뀌어
// 문서에 남는다 — 그래야 다음 tick 이 이어서 판단할 수 있다.
func (p *pipeline) step(ctx context.Context, j *job, b stepBudget) (stepResult, error) {
	// 🔴 **기기가 받아쓰는 통화에는 서버 ASR 을 부르지 않는다.** 이 검사가 아래 switch 보다
	// 먼저 와야 한다 — 서버 경로에서 「ASR_RUNNING 인데 토큰이 없다」는 Start 도중 인스턴스가
	// 죽었다는 뜻이라 곧바로 stepStart 로 가는데, 기기 경로의 작업은 **늘 그 모양이다.**
	// 순서가 뒤집히면 폰이 이미 받아쓰고 있는 통화를 공급자에게 한 번 더 맡긴다(요금 두 배).
	if j.clientASR() && j.State == stateASRRunning {
		return p.stepClientWait(ctx, j)
	}
	switch j.State {
	case stateQueued:
		return p.stepStart(ctx, j, b)
	case stateASRRunning:
		// 🔴 ASR_RUNNING 인데 토큰이 없다 = Start 호출 도중 인스턴스가 죽었다는 뜻이다.
		// 다시 Start 한다. 토큰이 있으면 상태 저장만 못 한 것이므로 폴링으로 잇는다.
		if j.ASRToken == "" {
			return p.stepStart(ctx, j, b)
		}
		return p.stepPoll(ctx, j, b)
	case stateASRPolling:
		return p.stepPoll(ctx, j, b)
	case stateTranscribed, stateAnalyzing:
		return p.stepAnalyze(ctx, j, b)
	}
	return stepDone, nil
}

// stepClientWait 는 **기기 받아쓰기를 기다리는 작업을 청소한다.**
//
// 하는 일은 둘뿐이다: 기다릴 시간이 남았으면 다음 확인 시각을 마감으로 밀어 두고 나가고,
// 마감이 지났으면 확정 실패로 세운다. 공급자는 한 번도 부르지 않는다.
//
// 🔴 다음 확인 시각을 마감으로 미는 것이 중요하다. 그대로 두면 이 작업이 **매분 tick 의
// 스윕 쿼리에 잡혀** 6시간 동안 아무 일도 하지 않는 조회·쓰기만 쌓는다. 스윕 쿼리는
// nextAttemptAt 오름차순이라 미래로 밀린 작업은 뒤로 가 다른 통화의 순서를 막지도 않는다.
func (p *pipeline) stepClientWait(ctx context.Context, j *job) (stepResult, error) {
	deadline := j.ClientASRAt.Add(clientTranscriptTimeout)
	if !p.clock().Before(deadline) {
		// 🔴 기기가 돌아오지 않았다. 이 자리를 비워 두면 그 통화는 영원히
		// 「기기에서 받아쓰는 중」으로 남는다 — 사용자는 서버가 일하고 있다고 믿는다.
		log.Printf("calls: 기기 받아쓰기가 %s 안에 돌아오지 않았다 call=%s — 확정 실패로 정리한다",
			dur(clientTranscriptTimeout), j.CallID)
		return p.failTerminal(ctx, j, phaseASR, codeClientTranscriptTimeout, callai.KindPermanent.String(), nil, p.clock().Sub(j.ClientASRAt))
	}
	j.NextAttemptAt = deadline
	if err := p.persist(ctx, j); err != nil {
		return stepDone, err
	}
	return stepDone, nil
}

func (p *pipeline) stepStart(ctx context.Context, j *job, b stepBudget) (stepResult, error) {
	if j.ASRAttempt >= maxASRAttempts {
		return p.failTerminal(ctx, j, phaseASR, codeRetriesExhausted, callai.KindPermanent.String(), nil, 0)
	}
	if j.Audio.Object == "" {
		return p.failTerminal(ctx, j, phaseASR, "AUDIO_MISSING", callai.KindInputUnavailable.String(), nil, 0)
	}
	// 🔴 예산 확인이 **상태를 바꾸기 전에** 와야 한다. ASR_RUNNING 으로 밀어 놓고 미루면
	// 다음 tick 이 「토큰을 잃어버린 작업」으로 오해해 시도 횟수를 한 번 더 쓴다.
	win, ok := b.window(asrStartNeed)
	if !ok {
		return p.deferStep(ctx, j, phaseASR, asrStartNeed, b)
	}
	j.State = stateASRRunning
	j.ASRAttempt++
	j.ASRToken = ""
	// 🔴 **Start 를 부르기 전에 저장한다.** 호출 도중 인스턴스가 죽으면 공급자 쪽에는 작업이
	// 만들어졌는데 우리에게는 토큰이 없는 상태가 된다. 그 사실(상태=ASR_RUNNING, 토큰=빈 값)이
	// 문서에 남아 있어야 다음 tick 이 「토큰을 잃어버린 작업」으로 알아보고 다시 집는다.
	if err := p.persist(ctx, j); err != nil {
		return stepDone, err
	}
	cctx, cancel := context.WithTimeout(ctx, win)
	started := time.Now()
	st, err := p.asr.Start(cctx, callai.Audio{
		Open:        func(c context.Context) (io.ReadCloser, error) { return p.audio.Open(c, j.Audio.Object) },
		Size:        j.Audio.Size,
		FileName:    j.Audio.FileName,
		ContentType: j.Audio.ContentType,
		Language:    p.language,
		// 상담 통화는 둘이 하는 대화다. 화자 분리를 켜야 요약이 누가 한 말인지 구분한다.
		SpeakerCount: 2,
	})
	cancel()
	if err != nil {
		return p.failStep(ctx, j, phaseASR, classifyCall(ctx, err, win), time.Since(started))
	}
	return p.applyASR(ctx, j, st)
}

func (p *pipeline) stepPoll(ctx context.Context, j *job, b stepBudget) (stepResult, error) {
	win, ok := b.window(asrPollNeed)
	if !ok {
		return p.deferStep(ctx, j, phaseASR, asrPollNeed, b)
	}
	cctx, cancel := context.WithTimeout(ctx, win)
	started := time.Now()
	st, err := p.asr.Poll(cctx, j.ASRToken)
	cancel()
	if err != nil {
		return p.failStep(ctx, j, phaseASR, classifyCall(ctx, err, win), time.Since(started))
	}
	return p.applyASR(ctx, j, st)
}

// applyASR 는 공급자 스냅샷을 작업 문서에 반영한다. Start 와 Poll 이 같은 모양을 돌려주므로 하나다.
func (p *pipeline) applyASR(ctx context.Context, j *job, st callai.TranscribeState) (stepResult, error) {
	// 공급자가 답했다 = 예산이 실제로 들어갔다는 뜻이다. 미루기 횟수를 되돌린다
	// (그 상한은 「영영 못 들어가는 단계」를 잡기 위한 것이지 누적 통계가 아니다).
	j.DeferCount = 0
	if st.Token != "" {
		j.ASRToken = st.Token
	}
	if st.RequestID != "" {
		j.ASRRequestID = st.RequestID
	}
	if st.Model != "" {
		j.ASRModel = st.Model
	}
	if p.asrProvider != "" {
		j.ASRProvider = p.asrProvider
	}

	if st.Status != callai.StatusDone || st.Transcript == nil {
		j.State = stateASRPolling
		if err := p.persist(ctx, j); err != nil {
			return stepDone, err
		}
		return stepWait, nil
	}

	// 🔴 사용량은 **재시도분까지 전부** 누적한다 — 실패한 시도의 요금도 실제로 청구되므로
	// 성공분만 기록하면 원가 집계가 청구서와 어긋나고 그 차이는 아무도 설명하지 못한다.
	// 폴링 응답마다 더하지 않고 완료 시점에만 더하는 이유는, 공급자가 진행 중 응답에도
	// 같은 사용량을 실어 보내면 한 건이 폴링 횟수만큼 중복 집계되기 때문이다.
	j.Usage.add(st.Usage)

	t := sanitizeTranscript(*st.Transcript)
	if t == nil {
		// 빈 전사문을 분석에 넘겨 봐야 지어낸 요약만 나온다. 재시도해도 같으므로 확정 실패다.
		return p.failTerminal(ctx, j, phaseASR, "EMPTY_TRANSCRIPT", callai.KindPermanent.String(), nil, 0)
	}
	b, err := marshalCompact(t)
	if err != nil || len(b) > maxTranscriptStoreBytes {
		return p.failTerminal(ctx, j, phaseASR, "TRANSCRIPT_TOO_LARGE", callai.KindPermanent.String(), nil, 0)
	}
	shards, err := p.jobs.PutTranscript(ctx, j.CallID, b)
	if err != nil {
		return stepDone, err
	}
	j.TranscriptShards = shards
	if st.Usage.AudioSeconds > 0 && st.Usage.AudioSeconds <= 86400 {
		// Metadata.Duration 은 ASR 이 알려 준 실제 오디오 길이로 채운다.
		j.Duration = st.Usage.AudioSeconds
	}
	j.State = stateTranscribed
	j.ErrorCode, j.ErrorKind = "", ""
	if err := p.persist(ctx, j); err != nil {
		return stepDone, err
	}
	return stepProgressed, nil
}

func (p *pipeline) stepAnalyze(ctx context.Context, j *job, b stepBudget) (stepResult, error) {
	tb, err := p.jobs.Transcript(ctx, j.CallID, j.TranscriptShards)
	if err != nil {
		return stepDone, err
	}
	t, decErr := decodeTranscript(tb)
	if decErr != nil || t == nil || strings.TrimSpace(t.Text) == "" {
		// 전사문이 사라졌다(수명주기 정리, 부분 쓰기 등). 있는 오디오로 처음부터 다시 한다.
		return p.requeueASR(ctx, j, "TRANSCRIPT_MISSING")
	}

	var a *Analysis
	if j.HasAnalysis {
		// 🔴 이미 저장된 분석이 있으면 **LLM 을 다시 부르지 않는다.** 아래에서 분석을 먼저
		// 저장하고 통화 레코드를 나중에 확정하는 순서가 이것을 위한 것이다 — 레코드 쓰기가
		// 일시 실패해 재시도로 돌아와도 비싼 호출은 한 번뿐이다.
		if b, e := p.jobs.Analysis(ctx, j.CallID); e == nil && len(b) > 0 {
			var parsed Analysis
			if json.Unmarshal(b, &parsed) == nil {
				a = &parsed
			}
		}
	}

	if a == nil {
		if j.AnalysisAttempt >= maxAnalysisAttempts {
			return p.failTerminal(ctx, j, phaseLLM, codeRetriesExhausted, callai.KindPermanent.String(), nil, 0)
		}
		// 🔴 **예산 확인이 시도 횟수를 올리기 전에 와야 한다.** 이 순서가 뒤집히면 「시간이
		// 없어 부르지도 못한 시도」가 횟수를 갉아먹고, 다섯 번이면 통화가 확정 실패한다.
		// 전사문 하나를 정리하는 데 실제로 걸리는 시간(llmCallNeed)은 ASR 폴링보다 훨씬
		// 길어서, 단계별로 나누지 않으면 이 자리가 조용히 먼저 깨진다.
		win, ok := b.window(llmCallNeed)
		if !ok {
			return p.deferStep(ctx, j, phaseLLM, llmCallNeed, b)
		}
		j.State = stateAnalyzing
		j.AnalysisAttempt++
		if err := p.persist(ctx, j); err != nil {
			return stepDone, err
		}
		cctx, cancel := context.WithTimeout(ctx, win)
		started := time.Now()
		res, err := p.llm.Analyze(cctx, toCallaiTranscript(t))
		cancel()
		if err != nil {
			return p.failStep(ctx, j, phaseLLM, classifyCall(ctx, err, win), time.Since(started))
		}
		if a = sanitizeAnalysis(res.Content); a == nil {
			return p.failTerminal(ctx, j, phaseLLM, "EMPTY_ANALYSIS", callai.KindPermanent.String(), nil, time.Since(started))
		}
		ab, mErr := marshalCompact(a)
		if mErr != nil || len(ab) > maxAnalysisStoreBytes {
			return p.failTerminal(ctx, j, phaseLLM, "ANALYSIS_TOO_LARGE", callai.KindPermanent.String(), nil, time.Since(started))
		}
		if err := p.jobs.PutAnalysis(ctx, j.CallID, ab); err != nil {
			return stepDone, err
		}
		j.DeferCount = 0
		j.HasAnalysis = true
		j.LLMRequestID = res.RequestID
		if res.Model != "" {
			j.LLMModel = res.Model
		}
		if p.llmProvider != "" {
			j.LLMProvider = p.llmProvider
		}
		j.PromptVersion = res.PromptVersion
		j.Usage.add(res.Usage)
		if err := p.persist(ctx, j); err != nil {
			return stepDone, err
		}
	}

	// 통화 레코드 확정. 상태를 미리 바꾼 **사본**으로 레코드를 만든다 — 저장이 실패하면
	// 작업은 아직 ANALYZING 이어야 다음 tick 이 이어서 시도한다.
	done := *j
	done.State = stateCompleted
	r := done.summaryRecord()
	r.Transcript, r.Analysis = t, a
	pay, perr := prepareServer(&r, j.CallID)
	if perr != nil {
		// 우리가 만든 레코드가 우리 검증을 통과하지 못했다 — 재시도해도 같다.
		return p.failTerminal(ctx, j, phaseLLM, "RECORD_INVALID", callai.KindPermanent.String(), nil, 0)
	}
	if _, err := p.records.SaveOverwrite(ctx, j.UID, pay); err != nil {
		return p.failStep(ctx, j, phaseLLM, err, 0)
	}
	j.State = stateCompleted
	j.ErrorCode, j.ErrorKind, j.ErrorAt = "", "", time.Time{}
	// 종료 상태는 스윕 쿼리의 status 목록(sweepStates)에 없으므로 다시 조회되지 않는다.
	// NextAttemptAt 은 마지막 스케줄 시각으로 남겨 둔다(job.NextAttemptAt 주석 참고).
	if err := p.persist(ctx, j); err != nil {
		return stepDone, err
	}
	return stepDone, nil
}

// requeueASR 는 오디오를 처음부터 다시 올리게 한다.
//
// 🔴 KindInputUnavailable 는 「공급자가 우리 오디오를 받아 가지 못했다」는 뜻이라 같은
// 토큰으로 폴링을 이어가 봐야 영원히 끝나지 않는다. 다만 무한 재업로드를 막기 위해
// asrAttempt 상한은 그대로 적용한다.
func (p *pipeline) requeueASR(ctx context.Context, j *job, code string) (stepResult, error) {
	// 🔴 **GCS 에 원본이 없는 통화는 다시 받아쓸 수 없다.** 여기 오는 통화는 둘이다:
	// ① 기기에서 받아쓰기까지 끝내고 결과만 올린 옛 통화를 레코드에서 되살린 작업
	//    (§audio.go jobFromRecord — 애초에 오디오가 GCS 에 없다)
	// ② 보관 기간(366일)이 지나 원본이 삭제된 통화.
	// 그대로 QUEUED 로 돌려보내도 stepStart 가 같은 사실을 발견하고 멈추기는 한다. 그러나
	// 그 사이 한 tick 동안 상태가 「업로드 완료, 순서 기다리는 중」으로 **되돌아가** 사용자
	// 화면에는 진행 중인 것처럼 보이고, ASR 시도 횟수를 한 번 쓴다. 여기서 끊는다.
	if j.Audio.Object == "" {
		return p.failTerminal(ctx, j, phaseASR, "AUDIO_MISSING", callai.KindInputUnavailable.String(), nil, 0)
	}
	if j.ASRAttempt >= maxASRAttempts {
		return p.failTerminal(ctx, j, phaseASR, code, callai.KindInputUnavailable.String(), nil, 0)
	}
	j.State = stateQueued
	j.ASRToken = ""
	j.TranscriptShards = 0
	j.ErrorCode, j.ErrorKind = code, callai.KindInputUnavailable.String()
	j.NextAttemptAt = p.clock().Add(backoff(j.ASRAttempt))
	if err := p.persist(ctx, j); err != nil {
		return stepDone, err
	}
	return stepDone, nil
}

// deferStep 은 이 단계를 **시작하지 않고** 다음 tick 으로 넘긴다.
//
// 🔴 이것은 실패가 아니다. 상태도 시도 횟수도 그대로 두고 lease 만 푼다. 그래야
//
//	① 사용자 화면은 「받아쓰는 중」·「내용 정리하는 중」을 그대로 유지하고(미루는 것은 우리
//	   사정이지 사용자가 알아야 할 일이 아니다),
//	② 시도 상한(=우리가 지불할 금액의 상한)을 부르지도 않은 호출이 갉아먹지 않는다.
//
// ⚠️ 미루기만 반복하는 작업이 영원히 도는 것은 막아야 한다. 다만 **모든 미룸을 세면 안
// 된다** — 앞 통화가 예산을 써서 밀린 것은 정상이고(그런 작업은 lease 를 풀고 나가므로
// 다음 tick 의 Claim 에서 먼저 집힌다), 큐가 밀리는 날 멀쩡한 통화가 줄줄이 확정 실패한다.
// 그래서 「빈 tick 이어도 이 단계는 들어가지 않는다」(b.total < need)일 때만 센다.
// 그 조건은 설정이 깨졌다는 뜻이므로(공급자가 느려졌거나 예산을 잘못 잡았다) 사람이 봐야 한다.
func (p *pipeline) deferStep(ctx context.Context, j *job, ph phase, need time.Duration, b stepBudget) (stepResult, error) {
	if b.total < need {
		j.DeferCount++
		if j.DeferCount > maxDefers {
			// 🔴 여기까지 오면 tick 예산 안에 이 단계가 들어갈 수 없다는 뜻이다.
			// 조용히 도는 것보다 종료 상태로 세워 두는 쪽이 낫다 — 로그와 화면에 남는다.
			log.Printf("calls: tick 예산 안에 단계가 들어가지 않는다 call=%s state=%s phase=%s 필요=%s tick예산=%s — 예산 상수를 다시 잡아야 한다",
				j.CallID, j.State, ph, dur(need), dur(b.total))
			return p.failTerminal(ctx, j, ph, codeBudgetTooSmall, callai.KindPermanent.String(), nil, 0)
		}
	}
	// lease 를 풀어 **다음 tick 이 바로** 이어받게 한다.
	j.NextAttemptAt = p.clock()
	log.Printf("calls: 예산이 모자라 단계를 미뤘다 call=%s state=%s phase=%s 필요=%s 남음=%s tick예산=%s 미룬횟수=%d",
		j.CallID, j.State, ph, dur(need), dur(b.left), dur(b.total), j.DeferCount)
	if err := p.persist(ctx, j); err != nil {
		return stepDone, err
	}
	return stepDone, nil
}

// failStep 은 공급자 실패를 재시도 예약 또는 확정 실패로 바꾼다.
func (p *pipeline) failStep(ctx context.Context, j *job, ph phase, err error, elapsed time.Duration) (stepResult, error) {
	// 🔴 예산 소진·요청 취소는 공급자 실패가 아니다. 이것을 거르지 않으면 「tick 이 시간을
	// 다 썼다」는 이유로 멀쩡한 작업이 확정 실패한다 — 2026-09-18 사고의 형태다.
	// 시도 횟수도 올리지 않고 상태 그대로 다음 tick 에 넘긴다.
	//
	// ⚠️ LLM 단계는 시도 횟수를 **호출 전에** 올린다(인스턴스가 죽어도 무한 재호출이 되지
	// 않게 하려는 것이다). 그래서 호출이 이미 나간 뒤 예산이 끊긴 경우에는 횟수가 하나
	// 소모된 채로 남는다 — 요금은 실제로 나갔으므로 그 편이 정직하다. 애초에 부르지
	// 않는 길(deferStep)이 먼저 막아 주므로 여기에 오는 것은 Cloud Run 이 우리를 먼저
	// 끊은 경우뿐이다.
	if ctx.Err() != nil || errors.Is(err, errBudget) {
		j.NextAttemptAt = p.clock()
		log.Printf("calls: 예산이 끊겨 단계 중단 call=%s state=%s phase=%s 경과=%s: %v",
			j.CallID, j.State, ph, dur(elapsed), err)
		return stepDone, p.persist(context.WithoutCancel(ctx), j)
	}
	kind := callai.KindOf(err)
	code := callai.CodeOf(err)
	if rid := callai.RequestIDOf(err); rid != "" {
		if ph == phaseASR {
			j.ASRRequestID = rid
		} else {
			j.LLMRequestID = rid
		}
	}
	attempt, limit := j.ASRAttempt, maxASRAttempts
	if ph == phaseLLM {
		attempt, limit = j.AnalysisAttempt, maxAnalysisAttempts
	}
	if ph == phaseASR && kind == callai.KindInputUnavailable {
		return p.requeueASR(ctx, j, code)
	}
	// 재시도해도 결과가 같은 실패에 다섯 번을 쓰지 않는다. 그 사이 사용자는 진행 막대만 본다.
	if !callai.Retryable(err) {
		// 공급자 코드를 그대로 내보낸다 — 「내용 필터에 걸렸다」처럼 사용자가 알아야 할
		// 사실이 그 코드에 들어 있다.
		return p.failTerminal(ctx, j, ph, code, kind.String(), err, elapsed)
	}
	if attempt >= limit {
		// 🔴 회복 가능한 실패를 상한까지 시도하고 멈춘 것이다. 마지막 공급자 코드를 그대로
		// 내보내면 앱이 「잠시 뒤 다시 시도해 주세요」로 옮기는데, 그건 사용자에게 떠넘기는
		// 말이다 — 서버가 이미 다 해 봤다. 코드로 그 사실을 말한다.
		return p.failTerminal(ctx, j, ph, codeRetriesExhausted, kind.String(), err, elapsed)
	}
	j.ErrorCode, j.ErrorKind = code, kind.String()
	j.NextAttemptAt = p.clock().Add(backoff(attempt))
	if ph == phaseLLM {
		// 전사문은 그대로 두고 분석만 다시 한다.
		j.State = stateTranscribed
	}
	// 🔴 사용자에게는 아직 아무것도 알리지 않는다. 자동으로 회복될 실패를 화면에 띄우면
	// 사용자는 서버가 이미 하고 있는 일을 손으로 누르게 된다. summaryRecord 가 종료
	// 상태에서만 error 를 채우는 것이 그 약속이다 — 그 조건을 넓히지 마라.
	//
	// 🔴 통화 내용은 한 글자도 남기지 않는다. 남기는 것은 코드·분류·HTTP status·request_id·
	// 소요 시간뿐이다. 이만큼도 없으면 원인을 알아내려고 코드를 거꾸로 읽게 된다.
	log.Printf("calls: 재시도 예약 call=%s phase=%s attempt=%d/%d 다음=%s 경과=%s %s",
		j.CallID, ph, attempt, limit, dur(backoff(attempt)), dur(elapsed), errDetail(err))
	if e := p.persist(ctx, j); e != nil {
		return stepDone, e
	}
	return stepDone, nil
}

// failTerminal 은 작업을 **사람이 손대야 하는 상태**로 세운다.
//
// 🔴 여기 도달하는 것은 ①자동 재시도를 다 썼거나 ②재시도가 무의미한 경우뿐이어야 한다.
// 종료 상태는 sweepStates 에 없어 다음 tick 이 다시 집지 않으므로, 회복 가능한 실패를
// 여기로 보내면 그 통화는 사람이 「분석 다시 시도」를 누르기 전까지 영원히 멈춘다.
// 2026-09-18 에 25분짜리 통화 한 건이 정확히 그렇게 멈췄다.
func (p *pipeline) failTerminal(ctx context.Context, j *job, ph phase, code, kind string, err error, elapsed time.Duration) (stepResult, error) {
	if ph == phaseASR {
		j.State = stateTranscriptionFailed
	} else {
		j.State = stateAnalysisFailed
	}
	// 🔴 작업 문서에 남는 code 가 곧 앱 화면에 나가는 값이다(summaryRecord).
	j.ErrorCode, j.ErrorKind, j.ErrorAt = userCode(kind, code), kind, p.clock()
	reqID := j.ASRRequestID
	if ph == phaseLLM {
		reqID = j.LLMRequestID
	}
	attempt, limit := j.ASRAttempt, maxASRAttempts
	if ph == phaseLLM {
		attempt, limit = j.AnalysisAttempt, maxAnalysisAttempts
	}
	// 🔴 무엇이 왜 실패했는지 여기 다 남긴다. 통화 내용은 여전히 한 글자도 남기지 않는다.
	log.Printf("calls: 작업 확정 실패 call=%s state=%s code=%s kind=%s attempt=%d/%d request_id=%s 경과=%s %s",
		j.CallID, j.State, code, kind, attempt, limit, reqID, dur(elapsed), errDetail(err))
	if e := p.persist(ctx, j); e != nil {
		return stepFailed, e
	}
	return stepFailed, nil
}

// persist 는 작업 문서를 저장하고 통화 레코드의 요약을 같은 상태로 맞춘다.
func (p *pipeline) persist(ctx context.Context, j *job) error {
	j.Stage = j.stageName()
	j.Progress = progressOf(j.State)
	j.UpdatedAt = p.clock()
	if err := p.jobs.Put(ctx, j); err != nil {
		return err
	}
	p.syncRecord(ctx, j)
	return nil
}

// syncRecord 는 앱이 보는 통화 레코드에 진행 상태를 비춘다.
//
// 실패해도 파이프라인은 계속 간다 — 화면의 단계 표시가 한 tick 늦을 뿐이고 다음 단계에서
// 다시 맞춰진다. 여기서 멈추면 정작 비싼 ASR 결과를 버리게 된다.
func (p *pipeline) syncRecord(ctx context.Context, j *job) {
	err := p.records.Touch(ctx, j.UID, j.CallID, func(r *Record) {
		s := j.summaryRecord()
		r.Status, r.JobState, r.Stage = s.Status, s.JobState, s.Stage
		r.Progress, r.Error, r.HasAudio = s.Progress, s.Error, s.HasAudio
		if s.Call.Duration != nil {
			r.Call.Duration = s.Call.Duration
		}
		if s.AI != nil {
			r.AI = s.AI
		}
	})
	// 레코드가 아직 없는 단계(AWAITING_UPLOAD)는 정상이다.
	if err != nil && status.Code(err) != codes.NotFound {
		log.Printf("calls: 통화 레코드 상태 갱신 실패 call=%s state=%s: %v", j.CallID, j.State, err)
	}
}

func (u *jobUsage) add(o callai.Usage) {
	u.AudioSeconds += o.AudioSeconds
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.ReasoningTokens += o.ReasoningTokens
}

func toCallaiTranscript(t *Transcript) callai.Transcript {
	segs := make([]callai.Segment, 0, len(t.Segments))
	for _, s := range t.Segments {
		segs = append(segs, callai.Segment{Start: s.Start, End: s.End, Text: s.Text, Speaker: s.Speaker})
	}
	return callai.Transcript{Text: t.Text, Segments: segs}
}

// sanitizeTranscript / sanitizeAnalysis 는 공급자 출력을 **우리 검증을 통과하는 모양으로
// 자른다.**
//
// 🔴 공급자 출력은 우리 한도를 모른다. detail 이 201개라는 이유로 통화 하나를 통째로
// 실패시키면 사용자는 아무것도 못 보는데, 잘라 내면 200개까지는 그대로 쓸 수 있다.
// 반대로 **요약이 비어 있으면** 보여 줄 것이 없으므로 그때만 실패로 본다.
func sanitizeTranscript(t callai.Transcript) *Transcript {
	text := strings.TrimSpace(t.Text)
	if text == "" {
		return nil
	}
	out := &Transcript{Text: truncBytes(text, maxTranscriptTextBytes), Segments: []Segment{}}
	last := float64(0)
	for _, s := range t.Segments {
		if len(out.Segments) >= 20000 {
			break
		}
		txt := strings.TrimSpace(s.Text)
		if txt == "" {
			continue
		}
		// 검증은 start 가 단조 증가하고 end >= start 이며 24시간 이하일 것을 요구한다.
		// 공급자가 어긴 값은 거부가 아니라 보정한다.
		start := clampSeconds(s.Start, last)
		end := clampSeconds(s.End, start)
		out.Segments = append(out.Segments, Segment{Start: start, End: end, Text: truncBytes(txt, 64000), Speaker: truncBytes(strings.TrimSpace(s.Speaker), 100)})
		last = start
	}
	return out
}

func clampSeconds(v, min float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < min {
		return min
	}
	if v > 86400 {
		return 86400
	}
	return v
}

func sanitizeAnalysis(c callai.AnalysisContent) *Analysis {
	summary := strings.TrimSpace(c.Summary)
	if summary == "" {
		return nil
	}
	a := &Analysis{SchemaVersion: 1, Summary: truncBytes(summary, 32000), Details: []Detail{}, Todos: []Todo{}}
	for _, d := range c.Details {
		if len(a.Details) >= 200 {
			break
		}
		title, body := strings.TrimSpace(d.Title), strings.TrimSpace(d.Content)
		if title == "" || body == "" {
			continue
		}
		a.Details = append(a.Details, Detail{Title: truncBytes(title, 1000), Content: truncBytes(body, 32000)})
	}
	for _, t := range c.Todos {
		if len(a.Todos) >= 100 {
			break
		}
		content := strings.TrimSpace(t.Content)
		if content == "" {
			continue
		}
		source := strings.TrimSpace(t.Source)
		if source == "" {
			// 근거 문장을 지어내지 않는다. 비어 있으면 할 일 문장 자체를 근거로 둔다.
			source = content
		}
		td := Todo{Content: truncBytes(content, 8000), Source: truncBytes(source, 8000)}
		if t.Owner != nil {
			if o := strings.TrimSpace(*t.Owner); o != "" {
				o = truncBytes(o, 400)
				td.Owner = &o
			}
		}
		if t.DueDate != nil {
			if d := strings.TrimSpace(*t.DueDate); d != "" {
				if _, e := time.Parse("2006-01-02", d); e == nil {
					td.DueDate = &d
				}
			}
		}
		a.Todos = append(a.Todos, td)
	}
	a.Decisions = sanitizeList(c.Decisions)
	a.Consulting = Consulting{
		CustomerNeeds:   sanitizeList(c.Consulting.CustomerNeeds),
		Questions:       sanitizeList(c.Consulting.Questions),
		Concerns:        sanitizeList(c.Consulting.Concerns),
		Objections:      sanitizeList(c.Consulting.Objections),
		ImportantPoints: sanitizeList(c.Consulting.ImportantPoints),
		Followups:       sanitizeList(c.Consulting.Followups),
	}
	return a
}

func sanitizeList(in []string) []string {
	out := []string{}
	for _, s := range in {
		if len(out) >= 200 {
			break
		}
		if v := strings.TrimSpace(s); v != "" {
			out = append(out, truncBytes(v, 16000))
		}
	}
	return out
}

// truncBytes 는 UTF-8 경계를 지키며 바이트 수로 자른다. 한도는 전부 바이트 기준이라
// rune 수로 자르면 한글에서 세 배까지 어긋난다.
func truncBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	b := []byte(s)[:max]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

// dur 은 로그에 넣을 소요 시간이다. 0 이면 「재지 않았다」는 뜻으로 - 를 쓴다 —
// 0.0s 로 찍으면 「즉시 실패했다」로 읽혀서 원인 추적이 엉뚱한 곳으로 간다.
func dur(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// errDetail 은 공급자 실패의 **진단 정보**를 한 줄로 만든다.
//
// 🔴 2026-09-18 에 운영 로그에 남은 것은 `code=RETRYABLE kind=RETRYABLE request_id=` 뿐이었다.
// HTTP status 도, 공급자 메시지도, 걸린 시간도 없어서 원인을 알아내려면 소스를 거꾸로
// 읽어야 했다. 그 일을 되풀이하지 않으려고 status·code·request_id·메시지를 남긴다.
//
// 🔴 **메시지에는 통화 내용이 섞여 들어올 수 있다.** 공급자가 「이런 입력은 처리 못 한다」며
// 입력 일부를 되비추는 경우가 실제로 있다. 그래서 길이 상한을 두고 자른다 — 진단에 필요한
// 것은 앞머리뿐이고, 원문을 통째로 남기는 것은 어떤 경우에도 허용되지 않는다.
func errDetail(err error) string {
	if err == nil {
		return ""
	}
	b := strings.Builder{}
	if st := callai.StatusOf(err); st != 0 {
		fmt.Fprintf(&b, "status=%d ", st)
	}
	fmt.Fprintf(&b, "code=%s kind=%s", callai.CodeOf(err), callai.KindOf(err))
	if rid := callai.RequestIDOf(err); rid != "" {
		fmt.Fprintf(&b, " request_id=%s", rid)
	}
	if m := strings.TrimSpace(callai.MessageOf(err)); m != "" {
		fmt.Fprintf(&b, " msg=%q", truncRunes(m, maxErrLogRunes))
	}
	return b.String()
}

// truncRunes 는 rune 경계로 자른다. 바이트로 자르면 한글이 깨진 채 로그에 들어간다.
func truncRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
