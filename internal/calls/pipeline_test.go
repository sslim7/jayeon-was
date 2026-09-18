package calls

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sslim7/nature-was/internal/auth"
	"github.com/sslim7/nature-was/internal/callai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pipeline_test.go 는 **네트워크도 에뮬레이터도 없이** 상태 기계 전체를 돌린다.
// 저장소를 인터페이스로 뽑아 둔 이유가 이것이다 — 상태 전이와 재시도 상한은 Firestore 와
// 아무 상관이 없는 로직인데, 확인하려고 매번 에뮬레이터를 띄우면 테스트가 개발 머신 설정에 묶인다.

// ── 메모리 저장소 ──────────────────────────────────────────────────────────────

type memJobs struct {
	mu          sync.Mutex
	docs        map[string]job
	transcripts map[string][]byte
	analyses    map[string][]byte
}

func newMemJobs() *memJobs {
	return &memJobs{docs: map[string]job{}, transcripts: map[string][]byte{}, analyses: map[string][]byte{}}
}

func (m *memJobs) Get(_ context.Context, id string) (*job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.docs[id]
	if !ok {
		return nil, status.Error(codes.NotFound, "no job")
	}
	return &j, nil
}

func (m *memJobs) Put(_ context.Context, j *job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.docs[j.CallID] = *j
	return nil
}

// Claim 은 Firestore 구현과 같은 규칙이다: status 목록으로 거르고 nextAttemptAt 오름차순,
// 그리고 **집는 순간 lease 를 민다**. 이 전체가 한 잠금 안에서 일어나는 것이 곧 transaction 이다.
func (m *memJobs) Claim(_ context.Context, now time.Time, limit int) ([]*job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sweep := map[string]bool{}
	for _, s := range sweepStates {
		sweep[s] = true
	}
	var due []job
	for _, j := range m.docs {
		if sweep[j.State] && !j.NextAttemptAt.After(now) {
			due = append(due, j)
		}
	}
	sort.Slice(due, func(a, b int) bool { return due[a].NextAttemptAt.Before(due[b].NextAttemptAt) })
	if len(due) > limit {
		due = due[:limit]
	}
	out := make([]*job, 0, len(due))
	for i := range due {
		j := due[i]
		j.NextAttemptAt = now.Add(leaseDuration)
		j.UpdatedAt = now
		m.docs[j.CallID] = j
		copied := j
		out = append(out, &copied)
	}
	return out, nil
}

func (m *memJobs) PutTranscript(_ context.Context, id string, data []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.transcripts[id] = append([]byte(nil), data...)
	return (len(data) + shardBytes - 1) / shardBytes, nil
}

func (m *memJobs) Transcript(_ context.Context, id string, shards int) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if shards <= 0 {
		return nil, nil
	}
	return m.transcripts[id], nil
}

func (m *memJobs) PutAnalysis(_ context.Context, id string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.analyses[id] = append([]byte(nil), data...)
	return nil
}

func (m *memJobs) Analysis(_ context.Context, id string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.analyses[id], nil
}

// memRecords 는 Store 와 **같은 조각 구조**로 저장한다. payload 를 그대로 받아 두었다가
// Get 에서 다시 이어 붙이므로, 조각 분리·재조립이 어긋나면 여기서 바로 드러난다.
type memRecords struct {
	mu         sync.Mutex
	meta       map[string][]byte
	transcript map[string][]byte
	analysis   map[string][]byte
	todos      map[string][][]byte
}

func newMemRecords() *memRecords {
	return &memRecords{meta: map[string][]byte{}, transcript: map[string][]byte{}, analysis: map[string][]byte{}, todos: map[string][][]byte{}}
}

func recKey(uid, id string) string { return uid + "/" + id }

func (m *memRecords) Save(ctx context.Context, uid string, p *payload) (Record, error) {
	m.mu.Lock()
	if _, ok := m.meta[recKey(uid, p.callID)]; ok {
		m.mu.Unlock()
		return Record{}, ErrConflict
	}
	m.mu.Unlock()
	return m.SaveOverwrite(ctx, uid, p)
}

func (m *memRecords) SaveOverwrite(_ context.Context, uid string, p *payload) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := recKey(uid, p.callID)
	m.meta[k] = p.meta
	m.transcript[k] = p.transcript
	m.analysis[k] = p.analysis
	m.todos[k] = p.todos
	return p.summary, nil
}

func (m *memRecords) Get(_ context.Context, uid, id string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := recKey(uid, id)
	meta, ok := m.meta[k]
	if !ok {
		return Record{}, status.Error(codes.NotFound, "no record")
	}
	var r Record
	if err := json.Unmarshal(meta, &r); err != nil {
		return r, err
	}
	if b := m.analysis[k]; b != nil {
		r.Analysis = &Analysis{}
		if err := json.Unmarshal(b, r.Analysis); err != nil {
			return r, err
		}
		for _, tb := range m.todos[k] {
			var t Todo
			if err := json.Unmarshal(tb, &t); err != nil {
				return r, err
			}
			r.Analysis.Todos = append(r.Analysis.Todos, t)
		}
	}
	if b := m.transcript[k]; b != nil {
		r.Transcript = &Transcript{}
		if err := json.Unmarshal(b, r.Transcript); err != nil {
			return r, err
		}
	}
	return r, nil
}

// register 가 요구하는 repository 를 채우기 위한 최소 구현이다. 파이프라인 테스트는 목록을 보지 않는다.
func (m *memRecords) List(_ context.Context, _, _ string, _ int, _ string) (Page, error) {
	return Page{Items: []Record{}}, nil
}

func (m *memRecords) Touch(_ context.Context, uid, id string, mut func(*Record)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := recKey(uid, id)
	meta, ok := m.meta[k]
	if !ok {
		return status.Error(codes.NotFound, "no record")
	}
	var r Record
	if err := json.Unmarshal(meta, &r); err != nil {
		return err
	}
	mut(&r)
	r.Transcript, r.Analysis = nil, nil
	b, err := marshalCompact(r)
	if err != nil {
		return err
	}
	m.meta[k] = b
	return nil
}

type memAudio struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (m *memAudio) Bucket() string { return "test-bucket" }
func (m *memAudio) SignedPut(_ context.Context, object, contentType string, _ time.Time) (string, error) {
	return "https://storage.test/" + object + "?ct=" + contentType, nil
}
func (m *memAudio) SignedGet(_ context.Context, object string, _ time.Time) (string, error) {
	return "https://storage.test/" + object + "?get=1", nil
}
func (m *memAudio) Size(_ context.Context, object string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[object]
	if !ok {
		return 0, errAudioMissing
	}
	return int64(len(b)), nil
}
func (m *memAudio) Open(_ context.Context, object string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[object]
	if !ok {
		return nil, errAudioMissing
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m *memAudio) put(object string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.objects == nil {
		m.objects = map[string][]byte{}
	}
	m.objects[object] = bytes.Repeat([]byte("a"), n)
}

// ── 하네스 ────────────────────────────────────────────────────────────────────

type harness struct {
	jobs  *memJobs
	recs  *memRecords
	audio *memAudio
	asr   *callai.FakeTranscriber
	llm   *callai.FakeAnalyzer
	pipe  *pipeline
	tick  *tickHandler
	mux   *http.ServeMux
	// audioH 는 상세 응답을 꾸미는 핸들러다. 테스트가 단가(pricing)를 나중에 끼워 넣을 수
	// 있도록 들고 있는다 — enrichDetail 은 포인터 리시버 메서드값이라, 등록한 뒤에 필드를
	// 바꿔도 이미 걸린 라우트에 그대로 반영된다.
	audioH *audioHandler

	clockMu sync.Mutex
	now     time.Time
}

func (h *harness) clock() time.Time {
	h.clockMu.Lock()
	defer h.clockMu.Unlock()
	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.clockMu.Lock()
	defer h.clockMu.Unlock()
	h.now = h.now.Add(d)
}

func newHarness(asr *callai.FakeTranscriber, llm *callai.FakeAnalyzer) *harness {
	h := &harness{jobs: newMemJobs(), recs: newMemRecords(), audio: &memAudio{}, asr: asr, llm: llm, now: time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)}
	h.pipe = &pipeline{
		jobs: h.jobs, records: h.recs, audio: h.audio, asr: asr, llm: llm,
		asrProvider: "fake", llmProvider: "fake", language: "ko",
		now: h.clock,
		// 폴링 대기는 실제로 자지 않고 **가짜 시계만 민다**. 예산 계산은 그대로 검증되면서
		// 테스트는 즉시 끝난다.
		sleep: func(_ context.Context, d time.Duration) error { h.advance(d); return nil },
	}
	h.tick = &tickHandler{auth: &tickAuth{shared: "local-secret"}, pipe: h.pipe, batch: 5}
	pass := func(next http.Handler) http.Handler { return next }
	h.mux = http.NewServeMux()
	ah := &audioHandler{jobs: h.jobs, records: h.recs, audio: h.audio, now: h.clock, retentionDays: 366}
	h.audioH = ah
	register(h.mux, h.recs, pass, ah.enrichDetail)
	ah.register(h.mux, pass)
	h.mux.HandleFunc("POST /internal/calls/tick", h.tick.serve)
	return h
}

func (h *harness) do(t *testing.T, method, path, uid, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if uid != "" {
		req = req.WithContext(auth.WithUserID(req.Context(), uid))
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	return w
}

func (h *harness) runTick() tickResponse {
	return h.tick.run(context.Background(), h.clock().Add(tickBudget))
}

func (h *harness) job(t *testing.T, id string) *job {
	t.Helper()
	j, err := h.jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("작업을 찾을 수 없다: %v", err)
	}
	return j
}

const uploadBody = `{"content_type":"audio/m4a","size":1024,
 "contact":{"name":"김고객","phone":"021234567"},
 "call":{"file_name":"call.m4a","duration":123.4,"recorded_at":"2026-09-18T14:00:00+09:00"}}`

// enqueue 는 upload-url → (GCS PUT) → complete 까지를 실제 HTTP 로 밟아 큐잉한다.
func (h *harness) enqueue(t *testing.T, uid, id string) {
	t.Helper()
	w := h.do(t, "POST", "/calls/"+id+"/audio/upload-url", uid, uploadBody)
	if w.Code != 200 {
		t.Fatalf("upload-url: %d %s", w.Code, w.Body.String())
	}
	var res uploadURLResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	// 앱이 서명 URL 로 직접 올린 상황을 흉내 낸다. 서버는 바이트를 통과시키지 않는다.
	h.audio.put(res.Object, 2048)
	if w = h.do(t, "POST", "/calls/"+id+"/audio/complete", uid, ""); w.Code != 200 {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
}

func fakeResult() callai.Transcript {
	return callai.Transcript{Text: "견적서 전달해주세요.", Segments: []callai.Segment{{Start: 0, End: 5, Text: "견적서 전달해주세요.", Speaker: "A"}}}
}

// ── 테스트 ────────────────────────────────────────────────────────────────────

// 1. 큐잉 → 여러 번 폴링 → 전사 저장 → 분석 → COMPLETED, 그리고 GET 이 원문+분석을 돌려준다.
func TestPipelineHappyPath(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Polls: 3, Result: fakeResult(), AudioSeconds: 321}, &callai.FakeAnalyzer{})
	h.enqueue(t, "owner-a", "call-1")

	// 큐잉 직후에도 앱이 이미 쓰는 GET 으로 진행 상태가 보여야 한다.
	got := decodeRecordBody(t, h.do(t, "GET", "/calls/call-1", "owner-a", ""))
	if got.Status != "PREPARING" || got.JobState != stateQueued || got.Stage == "" || !got.HasAudio {
		t.Fatalf("큐잉 상태가 안 보인다: %+v", got)
	}
	if got.Transcript != nil || got.Analysis != nil {
		t.Fatal("원문/분석이 없는 통화가 500 이 아니라 정상 응답이어야 한다")
	}

	res := h.runTick()
	if res.Claimed != 1 || res.Advanced != 1 || res.Failed != 0 {
		t.Fatalf("tick 결과: %+v", res)
	}
	// 🔴 폴링에 예산을 쓰고 나면 그 tick 에는 LLM 한 번(llmCallNeed)이 들어가지 않는다.
	// 그래서 분석은 **다음 tick** 몫이다 — 모자란 예산으로 부르지 않는 것이 이 설계의 핵심이다.
	// 그 사이 작업은 실패가 아니라 TRANSCRIBED 로 남고 lease 가 풀려 바로 이어진다.
	if j := h.job(t, "call-1"); j.State != stateTranscribed {
		t.Fatalf("폴링 뒤 상태 %s (code=%s)", j.State, j.ErrorCode)
	}
	if h.llm.Calls != 0 {
		t.Fatalf("예산이 모자란데 LLM 을 불렀다: %d", h.llm.Calls)
	}
	if res = h.runTick(); res.Claimed != 1 || res.Failed != 0 {
		t.Fatalf("다음 tick 이 분석을 잇지 못했다: %+v", res)
	}
	j := h.job(t, "call-1")
	if j.State != stateCompleted {
		t.Fatalf("상태 %s (code=%s)", j.State, j.ErrorCode)
	}
	if h.asr.PollCalls != 3 {
		t.Fatalf("폴링 횟수 %d", h.asr.PollCalls)
	}
	if h.asr.ReadBytes != 2048 {
		t.Fatalf("공급자가 오디오를 읽지 못했다: %d bytes", h.asr.ReadBytes)
	}

	got = decodeRecordBody(t, h.do(t, "GET", "/calls/call-1", "owner-a", ""))
	if got.Status != "COMPLETED" || got.JobState != stateCompleted {
		t.Fatalf("완료 상태가 아니다: %+v", got)
	}
	if got.Transcript == nil || got.Transcript.Text != fakeResult().Text {
		t.Fatalf("원문이 없다: %+v", got.Transcript)
	}
	if got.Analysis == nil || got.Analysis.Summary == "" {
		t.Fatalf("분석이 없다: %+v", got.Analysis)
	}
	// Metadata.Duration 은 ASR 이 알려 준 길이로 채워져야 한다.
	if got.Call.Duration == nil || *got.Call.Duration != 321 {
		t.Fatalf("duration 이 ASR 값으로 채워지지 않았다: %v", got.Call.Duration)
	}
	if got.AI == nil || got.AI.ProcessedOnDevice || got.AI.Provider != "fake" {
		t.Fatalf("ai 메타: %+v", got.AI)
	}
}

// 2. 🔴 분석만 실패했다가 다음 tick 에서 성공 — **전사를 다시 하지 않는다**(ASR 이 가장 비싸다).
func TestPipelineAnalysisRetryReusesTranscript(t *testing.T) {
	h := newHarness(
		&callai.FakeTranscriber{Result: fakeResult(), AudioSeconds: 100},
		&callai.FakeAnalyzer{Err: callai.FakeError(callai.KindRetryable, "Throttling.RateQuota"), FailTimes: 1},
	)
	h.enqueue(t, "owner-a", "call-1")

	h.runTick()
	j := h.job(t, "call-1")
	if j.State != stateTranscribed || j.ErrorCode != "Throttling.RateQuota" {
		t.Fatalf("재시도 예약 상태가 아니다: %s %s", j.State, j.ErrorCode)
	}
	if j.TranscriptShards == 0 {
		t.Fatal("전사문이 저장되지 않았다")
	}

	h.advance(2 * time.Minute) // backoff(1) = 1분
	h.runTick()
	j = h.job(t, "call-1")
	if j.State != stateCompleted {
		t.Fatalf("두 번째 tick 에서 완료되지 않았다: %s %s", j.State, j.ErrorCode)
	}
	if h.asr.StartCalls != 1 {
		t.Fatalf("전사를 다시 했다: StartCalls=%d", h.asr.StartCalls)
	}
	if h.llm.Calls != 2 {
		t.Fatalf("분석 호출 %d", h.llm.Calls)
	}
}

// 3. 재시도 상한. 도달하면 확정 실패하고, 그 뒤 tick 은 그 작업을 다시 집지 않는다.
func TestPipelineRetryCap(t *testing.T) {
	h := newHarness(
		&callai.FakeTranscriber{Result: fakeResult()},
		&callai.FakeAnalyzer{Err: callai.FakeError(callai.KindRetryable, "InternalError")},
	)
	h.enqueue(t, "owner-a", "call-1")
	for i := 0; i < maxAnalysisAttempts+2; i++ {
		h.runTick()
		h.advance(maxBackoff + time.Minute)
	}
	j := h.job(t, "call-1")
	// 🔴 마지막 공급자 코드(InternalError)가 아니라 **자동 재시도를 다 썼다**가 나가야 한다.
	// 앱은 이 코드를 그대로 문구로 옮긴다 — 회복 가능한 분류를 그대로 내보내면 화면에
	// 「잠시 뒤 다시 시도해 주세요」가 뜨는데, 서버는 이미 다섯 번 해 본 뒤다.
	if j.State != stateAnalysisFailed || j.ErrorCode != codeRetriesExhausted {
		t.Fatalf("확정 실패가 아니다: %s %s", j.State, j.ErrorCode)
	}
	if h.llm.Calls != maxAnalysisAttempts {
		t.Fatalf("상한을 넘겨 호출했다: %d", h.llm.Calls)
	}
	// 확정 실패 상태는 스윕 목록(sweepStates)에 없어 다시 조회되지 않는다.
	if res := h.runTick(); res.Claimed != 0 {
		t.Fatalf("끝난 작업을 다시 집었다: %+v", res)
	}
	got := decodeRecordBody(t, h.do(t, "GET", "/calls/call-1", "owner-a", ""))
	if got.Status != "ANALYSIS_FAILED" || got.Error == nil || *got.Error != codeRetriesExhausted {
		t.Fatalf("실패가 앱에 보이지 않는다: %+v", got)
	}
}

// 4. 재시도해도 결과가 같은 실패는 **한 번 만에** 확정한다. 다섯 번을 쓰는 동안
// 사용자는 진행 막대만 보고 있게 된다.
func TestPipelinePermanentFailsImmediately(t *testing.T) {
	t.Run("transcription", func(t *testing.T) {
		h := newHarness(
			&callai.FakeTranscriber{StartErr: callai.FakeError(callai.KindPermanent, "InvalidApiKey"), Result: fakeResult()},
			&callai.FakeAnalyzer{},
		)
		h.enqueue(t, "owner-a", "call-1")
		h.runTick()
		j := h.job(t, "call-1")
		if j.State != stateTranscriptionFailed || j.ErrorCode != "InvalidApiKey" || j.ASRAttempt != 1 {
			t.Fatalf("%s %s attempt=%d", j.State, j.ErrorCode, j.ASRAttempt)
		}
		if h.asr.StartCalls != 1 {
			t.Fatalf("재시도 낭비: %d", h.asr.StartCalls)
		}
	})
	t.Run("analysis", func(t *testing.T) {
		h := newHarness(
			&callai.FakeTranscriber{Result: fakeResult()},
			&callai.FakeAnalyzer{Err: callai.FakeError(callai.KindPermanent, "InvalidApiKey")},
		)
		h.enqueue(t, "owner-a", "call-1")
		h.runTick()
		j := h.job(t, "call-1")
		if j.State != stateAnalysisFailed || j.ErrorCode != "InvalidApiKey" || h.llm.Calls != 1 {
			t.Fatalf("%s %s calls=%d", j.State, j.ErrorCode, h.llm.Calls)
		}
	})
	// 콘텐츠 필터도 즉시 확정이지만 분류가 달라야 사용자 안내 문구를 나눌 수 있다.
	t.Run("content filtered", func(t *testing.T) {
		h := newHarness(
			&callai.FakeTranscriber{Result: fakeResult()},
			&callai.FakeAnalyzer{Err: callai.FakeError(callai.KindContentFiltered, "DataInspectionFailed")},
		)
		h.enqueue(t, "owner-a", "call-1")
		h.runTick()
		j := h.job(t, "call-1")
		if j.State != stateAnalysisFailed || j.ErrorKind != callai.KindContentFiltered.String() {
			t.Fatalf("%s kind=%s", j.State, j.ErrorKind)
		}
		// 🔴 앱에 나가는 코드는 공급자 문자열(DataInspectionFailed)이 아니라 분류 이름이다.
		// 공급자 코드를 그대로 내보내면 앱이 뜻을 몰라 「알 수 없는 오류입니다」로 떨어뜨리는데,
		// 이 통화에 대해 사용자가 알아야 할 사실은 「내용 때문에 공급자가 처리하지 않았다」다.
		if j.ErrorCode != callai.KindContentFiltered.String() {
			t.Fatalf("사용자에게 공급자 코드가 그대로 나간다: %s", j.ErrorCode)
		}
	})
}

// 🔴 KindInputUnavailable 은 「공급자가 우리 오디오를 못 받아 갔다」는 뜻이라 폴링을
// 이어가 봐야 끝나지 않는다. QUEUED 로 되돌려 처음부터 다시 올린다.
func TestPipelineInputUnavailableRequeues(t *testing.T) {
	h := newHarness(
		&callai.FakeTranscriber{Polls: 2, Result: fakeResult(), PollErr: callai.FakeError(callai.KindInputUnavailable, "InputDownloadFailed"), FailTimes: 1},
		&callai.FakeAnalyzer{},
	)
	h.enqueue(t, "owner-a", "call-1")
	h.runTick()
	j := h.job(t, "call-1")
	if j.State != stateQueued || j.ASRToken != "" {
		t.Fatalf("다시 올리기로 되돌아가지 않았다: %s token=%q", j.State, j.ASRToken)
	}
	h.advance(2 * time.Minute)
	h.runTick()
	// 재업로드 tick 은 폴링에 예산을 써서 분석까지는 못 간다(해피패스 주석 참고).
	h.runTick()
	if j = h.job(t, "call-1"); j.State != stateCompleted {
		t.Fatalf("재업로드 후 완료되지 않았다: %s %s", j.State, j.ErrorCode)
	}
	if h.asr.StartCalls != 2 {
		t.Fatalf("StartCalls=%d", h.asr.StartCalls)
	}
}

// 5. lease. 같은 작업을 두 tick 이 동시에 집지 못한다 — 집으면 공급자를 두 번 부르고
// 요금이 그대로 두 배가 된다.
func TestPipelineLeasePreventsDoubleWork(t *testing.T) {
	t.Run("이미 집힌 작업은 다음 스윕에 안 잡힌다", func(t *testing.T) {
		h := newHarness(&callai.FakeTranscriber{Polls: 5, Result: fakeResult()}, &callai.FakeAnalyzer{})
		h.enqueue(t, "owner-a", "call-1")
		// 앞선 tick 이 집어 lease 를 건 상황.
		claimed, err := h.jobs.Claim(context.Background(), h.clock(), 5)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v %d", err, len(claimed))
		}
		if res := h.runTick(); res.Claimed != 0 {
			t.Fatalf("lease 중인 작업을 또 집었다: %+v", res)
		}
		if h.asr.StartCalls != 0 {
			t.Fatalf("공급자를 불렀다: %d", h.asr.StartCalls)
		}
	})
	t.Run("동시에 두 tick 을 돌려도 공급자 호출은 한 번", func(t *testing.T) {
		h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
		h.enqueue(t, "owner-a", "call-1")
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); h.runTick() }()
		}
		wg.Wait()
		if h.asr.StartCalls != 1 || h.llm.Calls != 1 {
			t.Fatalf("중복 실행: StartCalls=%d Calls=%d", h.asr.StartCalls, h.llm.Calls)
		}
		if j := h.job(t, "call-1"); j.State != stateCompleted {
			t.Fatalf("%s", j.State)
		}
	})
}

// 8. 재분석은 분석만 다시 돈다. 전사는 그대로 재사용한다.
func TestPipelineReanalyze(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	h.enqueue(t, "owner-a", "call-1")
	h.runTick()
	if j := h.job(t, "call-1"); j.State != stateCompleted {
		t.Fatalf("선행 조건 실패: %s", j.State)
	}

	w := h.do(t, "POST", "/calls/call-1/reanalyze", "owner-a", "")
	if w.Code != 200 {
		t.Fatalf("reanalyze: %d %s", w.Code, w.Body.String())
	}
	j := h.job(t, "call-1")
	if j.State != stateTranscribed || j.HasAnalysis || j.AnalysisAttempt != 0 {
		t.Fatalf("재분석 예약 상태가 아니다: %+v", j)
	}
	h.runTick()
	if j = h.job(t, "call-1"); j.State != stateCompleted {
		t.Fatalf("재분석이 끝나지 않았다: %s %s", j.State, j.ErrorCode)
	}
	if h.asr.StartCalls != 1 {
		t.Fatalf("전사를 다시 했다: %d", h.asr.StartCalls)
	}
	if h.llm.Calls != 2 {
		t.Fatalf("분석 호출 %d", h.llm.Calls)
	}
	// 전사문을 그대로 넘겼는지 — 분석기가 받은 원문으로 확인한다.
	if h.llm.LastTranscript.Text != fakeResult().Text {
		t.Fatalf("재분석에 다른 원문이 갔다")
	}
}

// 🔴 분석을 먼저 저장하고 통화 레코드를 나중에 확정하는 순서 덕분에, 레코드 저장이
// 실패해 재시도로 돌아와도 비싼 LLM 호출은 한 번뿐이다.
func TestPipelineReusesStoredAnalysisAfterSaveFailure(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	h.enqueue(t, "owner-a", "call-1")
	failing := &failingRecords{memRecords: h.recs, failSaves: 1}
	h.pipe.records = failing

	h.runTick()
	j := h.job(t, "call-1")
	if !j.HasAnalysis {
		t.Fatal("분석이 통화 레코드보다 먼저 저장되지 않았다")
	}
	if j.State == stateCompleted {
		t.Fatal("레코드 저장이 실패했는데 완료로 표시됐다")
	}
	h.advance(2 * time.Minute)
	h.runTick()
	if j = h.job(t, "call-1"); j.State != stateCompleted {
		t.Fatalf("%s %s", j.State, j.ErrorCode)
	}
	if h.llm.Calls != 1 {
		t.Fatalf("저장된 분석을 재사용하지 않고 LLM 을 다시 불렀다: %d", h.llm.Calls)
	}
}

type failingRecords struct {
	*memRecords
	mu        sync.Mutex
	failSaves int
}

func (f *failingRecords) SaveOverwrite(ctx context.Context, uid string, p *payload) (Record, error) {
	f.mu.Lock()
	if f.failSaves > 0 {
		f.failSaves--
		f.mu.Unlock()
		return Record{}, status.Error(codes.Unavailable, "firestore down")
	}
	f.mu.Unlock()
	return f.memRecords.SaveOverwrite(ctx, uid, p)
}

// 공급자 출력이 우리 한도를 넘어도 통화 하나를 통째로 버리지 않는다. 잘라서 살린다.
func TestSanitizeClampsProviderOutput(t *testing.T) {
	long := strings.Repeat("가", 40000)
	details := make([]callai.Detail, 300)
	for i := range details {
		details[i] = callai.Detail{Title: "제목", Content: "내용"}
	}
	a := sanitizeAnalysis(callai.AnalysisContent{Summary: long, Details: details, Todos: []callai.Todo{{Content: "할 일"}, {Content: "  "}}})
	if a == nil || len(a.Details) != 200 || len(a.Summary) > 32000 || len(a.Todos) != 1 {
		t.Fatalf("clamp 실패: %+v", a)
	}
	if a.Todos[0].Source == "" {
		t.Fatal("근거가 빈 할 일은 검증을 통과하지 못한다")
	}
	if a.Decisions == nil || a.Consulting.Questions == nil {
		t.Fatal("누락 배열은 nil 이 아니라 빈 배열이어야 한다")
	}
	// 요약이 없으면 보여 줄 것이 없다 — 이때만 실패다.
	if sanitizeAnalysis(callai.AnalysisContent{Summary: "  "}) != nil {
		t.Fatal("빈 요약을 받아들였다")
	}

	// 구간 시각이 거꾸로 오거나 말이 안 되는 값이면 거부가 아니라 보정한다.
	tr := sanitizeTranscript(callai.Transcript{Text: "안녕하세요", Segments: []callai.Segment{{Start: 10, End: 5, Text: "b"}, {Start: 1, End: 2, Text: "a"}}})
	if tr == nil || len(tr.Segments) != 2 || tr.Segments[1].Start < tr.Segments[0].Start {
		t.Fatalf("구간 보정 실패: %+v", tr)
	}
	if sanitizeTranscript(callai.Transcript{Text: "  "}) != nil {
		t.Fatal("빈 전사문을 받아들였다")
	}
}

func TestBackoffSchedule(t *testing.T) {
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, w := range want {
		if got := backoff(i + 1); got != w {
			t.Fatalf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func decodeRecordBody(t *testing.T, w *httptest.ResponseRecorder) Record {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("응답 %d: %s", w.Code, w.Body.String())
	}
	var r Record
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// ── 예산 회귀 테스트 ───────────────────────────────────────────────────────────
//
// 아래 세 테스트는 2026-09-18 사고를 재현하고 그 재발을 막는다. 그날 25분짜리 상담 통화의
// 첫 서버 분석이, ASR 폴링에 예산을 쓴 tick 이 남은 13초로 LLM 을 부르면서 깨졌다.
// 돌아온 것은 타임아웃 하나였고, 그것이 「재시도 불가」로 분류되어 통화가 ANALYSIS_FAILED
// 로 확정됐다. 종료 상태는 스윕 대상이 아니라 그 뒤 매분 도는 tick 은 그 통화를 두 번 다시
// 집지 않았다 — 사람이 「분석 다시 시도」를 누르기 전까지 영원히 멈춰 있었다.

// 🔴 예산이 모자라면 **공급자를 부르지 않는다.** 그리고 그것은 실패가 아니다.
func TestPipelineDefersStepWhenBudgetShort(t *testing.T) {
	// 폴링 3회 = 가짜 시계로 15초. 남는 예산이 llmCallNeed(45초)에 못 미친다 —
	// 사고 당일과 같은 모양이다(ASR 이 예산을 쓰고 LLM 차례가 왔다).
	h := newHarness(&callai.FakeTranscriber{Polls: 3, Result: fakeResult()}, &callai.FakeAnalyzer{})
	h.enqueue(t, "owner-a", "call-1")

	res := h.runTick()
	j := h.job(t, "call-1")

	// ① 모자란 예산으로 공급자를 부르지 않았다. 부르면 요금은 나가고 결과는 못 받는다.
	if h.llm.Calls != 0 {
		t.Fatalf("예산이 모자란데 LLM 을 불렀다: %d회", h.llm.Calls)
	}
	// ② 종료 상태로 가지 않았다. 갔다면 다음 tick 이 영원히 집지 않는다.
	if terminalStates[j.State] {
		t.Fatalf("예산 부족을 확정 실패로 처리했다: %s (code=%s)", j.State, j.ErrorCode)
	}
	if j.State != stateTranscribed {
		t.Fatalf("상태가 전사 완료로 남아 있지 않다: %s", j.State)
	}
	// ③ 시도 횟수를 쓰지 않았다. 부르지도 않은 호출이 상한을 갉아먹으면 안 된다
	//    (그 상한은 곧 우리가 지불할 금액의 상한이다).
	if j.AnalysisAttempt != 0 {
		t.Fatalf("부르지도 않고 시도 횟수를 썼다: %d", j.AnalysisAttempt)
	}
	if j.ErrorCode != "" || j.ErrorKind != "" {
		t.Fatalf("미룸이 실패로 기록됐다: code=%s kind=%s", j.ErrorCode, j.ErrorKind)
	}
	if res.Failed != 0 {
		t.Fatalf("미룸이 실패로 집계됐다: %+v", res)
	}
	// 🔴 사용자 화면은 「분석 중」을 유지해야 한다. 자동으로 회복될 일에 실패 문구나
	// 재시도 버튼을 띄우면, 사용자는 서버가 이미 하고 있는 일을 손으로 누르게 된다.
	got := decodeRecordBody(t, h.do(t, "GET", "/calls/call-1", "owner-a", ""))
	if got.Status != "ANALYZING" || got.Error != nil {
		t.Fatalf("미루는 동안 앱에 실패가 보인다: status=%s error=%v stage=%q", got.Status, got.Error, got.Stage)
	}

	// ④ lease 가 풀려 다음 tick 이 **바로** 집는다. 풀지 않으면 lease 만료(2분)까지 논다.
	if j.NextAttemptAt.After(h.clock()) {
		t.Fatalf("lease 가 풀리지 않았다: %v > %v", j.NextAttemptAt, h.clock())
	}
	if next := h.runTick(); next.Claimed != 1 {
		t.Fatalf("다음 tick 이 미뤄진 작업을 집지 않았다: %+v", next)
	}
	if h.llm.Calls != 1 {
		t.Fatalf("다음 tick 이 분석을 잇지 않았다: %d회", h.llm.Calls)
	}
	if j = h.job(t, "call-1"); j.State != stateCompleted {
		t.Fatalf("이어받은 tick 이 끝내지 못했다: %s (code=%s)", j.State, j.ErrorCode)
	}
	// 🔴 전사를 다시 하지 않았다. 미루기가 ASR 을 되돌리면 요금이 그대로 두 배다.
	if h.asr.StartCalls != 1 {
		t.Fatalf("미룬 뒤 전사를 다시 시작했다: %d회", h.asr.StartCalls)
	}
}

// 🔴 **공급자 타임아웃은 진짜 실패다.** 예산 부족(위 테스트)과 달리 시도 횟수를 쓰고
// 백오프를 건다. 둘을 같은 것으로 다루면, 답하지 않는 공급자를 상한 없이 계속 부르거나
// (한쪽으로 뭉개면) 멀쩡한 통화를 재시도 없이 버린다(다른 쪽으로 뭉개면 — 2026-09-18).
func TestPipelineProviderTimeoutConsumesAttemptAndBacksOff(t *testing.T) {
	// 사고 당일 공급자 계층이 올려보낸 것과 같은 맨 에러다. 이것이 *callai.Error 로
	// 감싸이지 않으면 callai.Retryable 이 false 를 돌려주고 통화가 확정 실패한다.
	h := newHarness(
		&callai.FakeTranscriber{Result: fakeResult()},
		&callai.FakeAnalyzer{Err: context.DeadlineExceeded, FailTimes: 1},
	)
	h.enqueue(t, "owner-a", "call-1")
	h.runTick()

	j := h.job(t, "call-1")
	if terminalStates[j.State] {
		t.Fatalf("타임아웃 한 번으로 통화를 버렸다: %s (code=%s)", j.State, j.ErrorCode)
	}
	// 전사문은 그대로 두고 분석만 다시 한다 — ASR 이 가장 비싼 단계다.
	if j.State != stateTranscribed {
		t.Fatalf("분석 재시도 상태가 아니다: %s", j.State)
	}
	if j.AnalysisAttempt != 1 {
		t.Fatalf("공급자 실패인데 시도 횟수를 쓰지 않았다: %d", j.AnalysisAttempt)
	}
	if j.ErrorCode != callai.CodeProviderTimeout {
		t.Fatalf("타임아웃이 공급자 타임아웃으로 분류되지 않았다: code=%s kind=%s", j.ErrorCode, j.ErrorKind)
	}
	// 예산 부족은 곧바로(now) 다시 집지만, 진짜 실패는 백오프를 둔다.
	if want := h.clock().Add(backoff(1)); !j.NextAttemptAt.Equal(want) {
		t.Fatalf("백오프가 걸리지 않았다: %v (기대 %v)", j.NextAttemptAt, want)
	}
	// 🔴 자동으로 회복될 실패는 사용자에게 알리지 않는다.
	got := decodeRecordBody(t, h.do(t, "GET", "/calls/call-1", "owner-a", ""))
	if got.Status != "ANALYZING" || got.Error != nil {
		t.Fatalf("재시도 중인데 앱에 실패가 보인다: status=%s error=%v", got.Status, got.Error)
	}

	h.advance(backoff(1))
	h.runTick()
	if j = h.job(t, "call-1"); j.State != stateCompleted {
		t.Fatalf("백오프 뒤 재시도가 끝내지 못했다: %s (code=%s)", j.State, j.ErrorCode)
	}
	if h.asr.StartCalls != 1 {
		t.Fatalf("분석 재시도가 전사를 다시 했다: %d회", h.asr.StartCalls)
	}
}

// ⚠️ 미루기에는 상한이 있어야 한다. 상한이 없으면 「예산에 영영 들어가지 않는 단계」가
// 매 tick 조회·쓰기만 하며 영원히 돈다 — 화면에는 「내용 정리하는 중」이 계속 떠 있어서
// 확정 실패보다 알아채기 어렵다.
func TestPipelineDeferHasCap(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	h.enqueue(t, "owner-a", "call-1")
	// 전사는 들어가지만 분석(llmCallNeed)은 빈 tick 이어도 들어가지 않는 예산이다.
	// 「공급자가 느려져 우리 예산을 넘어선」 상황이 이 모양이다.
	short := asrStartNeed + time.Second
	for i := 0; i < maxDefers; i++ {
		h.tick.run(context.Background(), h.clock().Add(short))
		j := h.job(t, "call-1")
		if terminalStates[j.State] {
			t.Fatalf("%d번째 미룸에서 이미 종료 상태다: %s", i+1, j.State)
		}
		if j.DeferCount != i+1 {
			t.Fatalf("%d번째 미룸인데 미룬횟수=%d", i+1, j.DeferCount)
		}
		// 상한에 닿기 전까지 사용자 화면은 진행 중이다.
		got := decodeRecordBody(t, h.do(t, "GET", "/calls/call-1", "owner-a", ""))
		if got.Status != "ANALYZING" || got.Error != nil {
			t.Fatalf("%d번째 미룸에서 앱에 실패가 보인다: status=%s error=%v", i+1, got.Status, got.Error)
		}
	}
	// 상한을 넘으면 사람이 볼 수 있는 종료 상태로 세운다.
	h.tick.run(context.Background(), h.clock().Add(short))
	j := h.job(t, "call-1")
	if j.State != stateAnalysisFailed || j.ErrorCode != "BUDGET_TOO_SMALL" {
		t.Fatalf("미루기 상한이 동작하지 않았다: %s (code=%s, 미룬횟수=%d)", j.State, j.ErrorCode, j.DeferCount)
	}
	// 🔴 그 사이 공급자는 한 번도 부르지 않았다.
	if h.llm.Calls != 0 {
		t.Fatalf("예산이 없는데 LLM 을 불렀다: %d회", h.llm.Calls)
	}
}

// 앞 통화가 예산을 써서 밀린 것은 **세지 않는다.** 이것까지 세면 큐가 밀리는 날
// 멀쩡한 통화가 줄줄이 확정 실패한다.
func TestPipelineDeferNotCountedWhenTickWasBusy(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Polls: 3, Result: fakeResult()}, &callai.FakeAnalyzer{})
	h.enqueue(t, "owner-a", "call-1")
	h.runTick() // 폴링으로 예산을 쓰고 분석을 미룬다
	if j := h.job(t, "call-1"); j.DeferCount != 0 {
		t.Fatalf("정상적인 밀림을 실패로 셌다: 미룬횟수=%d", j.DeferCount)
	}
}

// 🔴 2026-09-18 에 운영에 멈춘 통화 한 건(ANALYSIS_FAILED)이 「분석 다시 시도」로 되살아나는지.
// 이 경로가 막혀 있으면 이미 벌어진 사고를 사람이 손으로도 못 푼다.
func TestReanalyzeRecoversFromAnalysisFailed(t *testing.T) {
	h := newHarness(
		&callai.FakeTranscriber{Result: fakeResult()},
		// 상한까지 실패시켜 사고 당일과 같은 종료 상태를 만든다.
		&callai.FakeAnalyzer{Err: callai.FakeError(callai.KindRetryable, "InternalError"), FailTimes: maxAnalysisAttempts},
	)
	h.enqueue(t, "owner-a", "call-1")
	for i := 0; i < maxAnalysisAttempts+1; i++ {
		h.runTick()
		h.advance(maxBackoff + time.Minute)
	}
	j := h.job(t, "call-1")
	if j.State != stateAnalysisFailed {
		t.Fatalf("선행 조건 실패: %s (code=%s)", j.State, j.ErrorCode)
	}
	shards, calls := j.TranscriptShards, h.llm.Calls
	if shards == 0 {
		t.Fatal("전사문이 남아 있지 않다 — 재분석할 원문이 없다")
	}

	if w := h.do(t, "POST", "/calls/call-1/reanalyze", "owner-a", ""); w.Code != 200 {
		t.Fatalf("재분석 요청이 거절됐다: %d %s", w.Code, w.Body.String())
	}
	j = h.job(t, "call-1")
	if j.State != stateTranscribed || j.AnalysisAttempt != 0 || j.DeferCount != 0 || j.ErrorCode != "" {
		t.Fatalf("재분석 예약 상태가 아니다: state=%s attempt=%d defer=%d code=%s", j.State, j.AnalysisAttempt, j.DeferCount, j.ErrorCode)
	}
	// 🔴 전사문을 그대로 쓴다. 여기서 ASR 을 다시 돌리면 요금이 통째로 두 배다.
	if j.TranscriptShards != shards {
		t.Fatalf("전사문을 버렸다: %d → %d", shards, j.TranscriptShards)
	}

	h.runTick()
	if j = h.job(t, "call-1"); j.State != stateCompleted {
		t.Fatalf("재분석이 끝나지 않았다: %s (code=%s)", j.State, j.ErrorCode)
	}
	if h.asr.StartCalls != 1 {
		t.Fatalf("재분석이 전사를 다시 했다: %d회", h.asr.StartCalls)
	}
	if h.llm.Calls != calls+1 {
		t.Fatalf("분석 호출 수가 맞지 않는다: %d → %d", calls, h.llm.Calls)
	}
	got := decodeRecordBody(t, h.do(t, "GET", "/calls/call-1", "owner-a", ""))
	if got.Status != "COMPLETED" || got.Error != nil || got.Analysis == nil {
		t.Fatalf("되살아난 통화가 앱에 완료로 보이지 않는다: status=%s error=%v", got.Status, got.Error)
	}
}
