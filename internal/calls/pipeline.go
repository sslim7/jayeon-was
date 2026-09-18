package calls

import (
	"context"
	"encoding/json"
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

// pollWait 는 폴링 사이 간격이다. 공급자 전사는 보통 수십 초가 걸리므로 더 짧게 찔러 봐야
// 쿼터만 쓴다. 🔴 time.Sleep 이 아니라 ctx 를 존중하는 타이머로 기다린다.
const pollWait = 5 * time.Second

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
func (p *pipeline) step(ctx context.Context, j *job) (stepResult, error) {
	switch j.State {
	case stateQueued:
		return p.stepStart(ctx, j)
	case stateASRRunning:
		// 🔴 ASR_RUNNING 인데 토큰이 없다 = Start 호출 도중 인스턴스가 죽었다는 뜻이다.
		// 다시 Start 한다. 토큰이 있으면 상태 저장만 못 한 것이므로 폴링으로 잇는다.
		if j.ASRToken == "" {
			return p.stepStart(ctx, j)
		}
		return p.stepPoll(ctx, j)
	case stateASRPolling:
		return p.stepPoll(ctx, j)
	case stateTranscribed, stateAnalyzing:
		return p.stepAnalyze(ctx, j)
	}
	return stepDone, nil
}

func (p *pipeline) stepStart(ctx context.Context, j *job) (stepResult, error) {
	if j.ASRAttempt >= maxASRAttempts {
		return p.failTerminal(ctx, j, phaseASR, "MAX_ATTEMPTS", callai.KindPermanent.String())
	}
	if j.Audio.Object == "" {
		return p.failTerminal(ctx, j, phaseASR, "AUDIO_MISSING", callai.KindInputUnavailable.String())
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
	st, err := p.asr.Start(ctx, callai.Audio{
		Open:        func(c context.Context) (io.ReadCloser, error) { return p.audio.Open(c, j.Audio.Object) },
		Size:        j.Audio.Size,
		FileName:    j.Audio.FileName,
		ContentType: j.Audio.ContentType,
		Language:    p.language,
		// 상담 통화는 둘이 하는 대화다. 화자 분리를 켜야 요약이 누가 한 말인지 구분한다.
		SpeakerCount: 2,
	})
	if err != nil {
		return p.failStep(ctx, j, phaseASR, err)
	}
	return p.applyASR(ctx, j, st)
}

func (p *pipeline) stepPoll(ctx context.Context, j *job) (stepResult, error) {
	st, err := p.asr.Poll(ctx, j.ASRToken)
	if err != nil {
		return p.failStep(ctx, j, phaseASR, err)
	}
	return p.applyASR(ctx, j, st)
}

// applyASR 는 공급자 스냅샷을 작업 문서에 반영한다. Start 와 Poll 이 같은 모양을 돌려주므로 하나다.
func (p *pipeline) applyASR(ctx context.Context, j *job, st callai.TranscribeState) (stepResult, error) {
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
		return p.failTerminal(ctx, j, phaseASR, "EMPTY_TRANSCRIPT", callai.KindPermanent.String())
	}
	b, err := marshalCompact(t)
	if err != nil || len(b) > maxTranscriptStoreBytes {
		return p.failTerminal(ctx, j, phaseASR, "TRANSCRIPT_TOO_LARGE", callai.KindPermanent.String())
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

func (p *pipeline) stepAnalyze(ctx context.Context, j *job) (stepResult, error) {
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
			return p.failTerminal(ctx, j, phaseLLM, "MAX_ATTEMPTS", callai.KindPermanent.String())
		}
		j.State = stateAnalyzing
		j.AnalysisAttempt++
		if err := p.persist(ctx, j); err != nil {
			return stepDone, err
		}
		res, err := p.llm.Analyze(ctx, toCallaiTranscript(t))
		if err != nil {
			return p.failStep(ctx, j, phaseLLM, err)
		}
		if a = sanitizeAnalysis(res.Content); a == nil {
			return p.failTerminal(ctx, j, phaseLLM, "EMPTY_ANALYSIS", callai.KindPermanent.String())
		}
		b, mErr := marshalCompact(a)
		if mErr != nil || len(b) > maxAnalysisStoreBytes {
			return p.failTerminal(ctx, j, phaseLLM, "ANALYSIS_TOO_LARGE", callai.KindPermanent.String())
		}
		if err := p.jobs.PutAnalysis(ctx, j.CallID, b); err != nil {
			return stepDone, err
		}
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
		return p.failTerminal(ctx, j, phaseLLM, "RECORD_INVALID", callai.KindPermanent.String())
	}
	if _, err := p.records.SaveOverwrite(ctx, j.UID, pay); err != nil {
		return p.failStep(ctx, j, phaseLLM, err)
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
	if j.ASRAttempt >= maxASRAttempts {
		return p.failTerminal(ctx, j, phaseASR, code, callai.KindInputUnavailable.String())
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

// failStep 은 공급자 실패를 재시도 예약 또는 확정 실패로 바꾼다.
func (p *pipeline) failStep(ctx context.Context, j *job, ph phase, err error) (stepResult, error) {
	// 🔴 예산 소진·요청 취소는 공급자 실패가 아니다. callai.Retryable 이 취소를 false 로
	// 보기 때문에, 이것을 거르지 않으면 tick 이 시간을 다 썼다는 이유로 멀쩡한 작업이
	// 확정 실패한다. 시도 횟수도 올리지 않고 그대로 다음 tick 에 넘긴다.
	if ctx.Err() != nil {
		j.NextAttemptAt = p.clock()
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
	if !callai.Retryable(err) || attempt >= limit {
		return p.failTerminal(ctx, j, ph, code, kind.String())
	}
	j.ErrorCode, j.ErrorKind = code, kind.String()
	j.NextAttemptAt = p.clock().Add(backoff(attempt))
	if ph == phaseLLM {
		// 전사문은 그대로 두고 분석만 다시 한다.
		j.State = stateTranscribed
	}
	// 🔴 통화 내용은 한 글자도 남기지 않는다. 코드·분류·request_id 만 남긴다.
	log.Printf("calls: 재시도 예약 call=%s phase=%d attempt=%d code=%s kind=%s", j.CallID, ph, attempt, code, kind)
	if e := p.persist(ctx, j); e != nil {
		return stepDone, e
	}
	return stepDone, nil
}

func (p *pipeline) failTerminal(ctx context.Context, j *job, ph phase, code, kind string) (stepResult, error) {
	if ph == phaseASR {
		j.State = stateTranscriptionFailed
	} else {
		j.State = stateAnalysisFailed
	}
	j.ErrorCode, j.ErrorKind, j.ErrorAt = code, kind, p.clock()
	// 종료 상태는 sweepStates 에 없어 다음 tick 의 스윕 쿼리에 잡히지 않는다.
	reqID := j.ASRRequestID
	if ph == phaseLLM {
		reqID = j.LLMRequestID
	}
	log.Printf("calls: 작업 확정 실패 call=%s state=%s code=%s kind=%s request_id=%s", j.CallID, j.State, code, kind, reqID)
	if e := p.persist(ctx, j); e != nil {
		return stepFailed, e
	}
	return stepFailed, nil
}

// persist 는 작업 문서를 저장하고 통화 레코드의 요약을 같은 상태로 맞춘다.
func (p *pipeline) persist(ctx context.Context, j *job) error {
	j.Stage = stageOf(j.State)
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
