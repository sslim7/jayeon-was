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
	if j.State != stateAnalysisFailed || j.ErrorCode != "InternalError" {
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
	if got.Status != "ANALYSIS_FAILED" || got.Error == nil || *got.Error != "InternalError" {
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
