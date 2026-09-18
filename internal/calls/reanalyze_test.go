package calls

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sslim7/nature-was/internal/callai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// reanalyze_test.go 는 **작업 문서가 없는 옛 통화를 서버 LLM 으로 다시 분석하는** 경로를 지킨다.
//
// 기기(whisper.rn + 온디바이스 LLM)에서 다 끝내고 PUT /calls/{id} 로 결과만 올린 통화에는
// callJobs 문서가 아예 없다. 그런 통화도 원문만 있으면 다시 분석할 수 있어야 하는데,
// 이 경로에서 조용히 깨질 수 있는 것이 셋이다:
//
//	① **없는 오디오로 ASR 을 부른다** → 요금이 나간다. GCS 에 원본이 없는 통화들이다.
//	② **쓰지도 않은 받아쓰기 비용이 뜬다** → 통화 길이를 사용량에 옮겨 적으면 그렇게 된다.
//	③ **어느 모델이 만든 결과인지 화면이 거짓말을 한다** → 기기 모델 이름이 그대로 남는 경우다.
//
// 셋 다 화면만 봐서는 알아챌 수 없다(②③ 은 그럴듯한 숫자와 이름으로 보인다). 테스트가 유일한 방어선이다.

// deviceRecord 는 기기 경로(PUT /calls/{id})로 올라온 옛 통화 한 건이다.
// 실제 문제 통화(28분 26초, 폰 받아쓰기 + Qwen3-0.6B-Q8_0 요약)와 같은 모양으로 만든다.
func deviceRecord(id string) Record {
	return Record{
		CallID:  id,
		Contact: Contact{Name: "강연정", Phone: "01012345678"},
		Call:    Metadata{FileName: "call-old.m4a", Duration: ptrFloat(1706), RecordedAt: "2026-09-01T14:00:00+09:00"},
		// 🔴 created_at 은 통화일시보다 뒤다. 둘을 뒤섞으면 아래 「메타가 보존되는가」 검사가
		// 무엇을 확인하는지 알 수 없게 된다.
		CreatedAt:  "2026-09-01T14:30:00+09:00",
		Status:     "COMPLETED",
		Transcript: &Transcript{Text: "견적서 전달해주세요.", Segments: []Segment{{Start: 0, End: 5, Text: "견적서 전달해주세요."}}},
		Analysis: &Analysis{
			SchemaVersion: 1, Summary: "폰에서 만든 요약", Details: []Detail{}, Todos: []Todo{},
			Decisions:  []string{},
			Consulting: Consulting{CustomerNeeds: []string{}, Questions: []string{}, Concerns: []string{}, Objections: []string{}, ImportantPoints: []string{}, Followups: []string{}},
		},
		AI: &AI{Model: "Qwen3-0.6B-Q8_0", ModelVersion: "1", ProcessedOnDevice: true},
	}
}

func ptrFloat(v float64) *float64 { return &v }

// putDevice 는 구버전 앱과 **같은 엔드포인트로** 통화를 올린다. 저장소에 직접 쓰지 않는
// 이유는, 이 테스트가 지키려는 것이 「기기가 실제로 남긴 레코드」이기 때문이다.
func putDevice(t *testing.T, h *harness, uid string, rec Record) {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if w := h.do(t, "PUT", "/calls/"+rec.CallID, uid, string(b)); w.Code != 200 {
		t.Fatalf("기기 경로 저장: %d %s", w.Code, w.Body.String())
	}
}

// requireNoJob 은 **이 테스트의 전제**를 확인한다. 기기 경로 통화에 작업 문서가 생기기
// 시작하면 아래 테스트들은 통과하면서도 아무것도 지키지 못한다 — 검증하려는 상황 자체가
// 재현되지 않기 때문이다.
func requireNoJob(t *testing.T, h *harness, id string) {
	t.Helper()
	if _, err := h.jobs.Get(context.Background(), id); status.Code(err) != codes.NotFound {
		t.Fatalf("기기 경로 통화 %s 에 작업 문서가 있다: %v", id, err)
	}
}

// 1. 작업 문서가 없는 기기 경로 통화를 재분석하면 작업이 만들어지고, 다음 tick 이 그것을
// 집어 **서버 LLM 으로만** 분석을 채운다.
//
// 🔴 ASR 은 한 번도 불리지 않아야 한다. 이 통화의 녹음 파일은 GCS 에 없어서, 부른다는 것은
// 곧 없는 파일을 올리려다 실패하거나 최악의 경우 요금만 나간다는 뜻이다.
func TestReanalyzeAdoptsDeviceRecord(t *testing.T) {
	asr := &callai.FakeTranscriber{Result: fakeResult(), AudioSeconds: 1706}
	h := newHarness(asr, &callai.FakeAnalyzer{})
	h.audioH.pricing = testPricing()
	putDevice(t, h, "owner-a", deviceRecord("call-old"))
	requireNoJob(t, h, "call-old")

	w := h.do(t, "POST", "/calls/call-old/reanalyze", "owner-a", "")
	if w.Code != 200 {
		t.Fatalf("재분석 예약: %d %s", w.Code, w.Body.String())
	}
	// 앱이 즉시 진행 상태를 보게 한다 — 버튼을 눌렀는데 화면이 그대로면 사용자는 또 누른다.
	queued := decodeRecordBody(t, w)
	if queued.Status != "ANALYZING" || queued.JobState != stateTranscribed {
		t.Fatalf("재분석 응답이 진행 중이 아니다: %+v", queued)
	}

	j := h.job(t, "call-old")
	// 🔴 만들어 낸 작업에는 오디오가 없어야 한다. object 가 채워지면 파이프라인이 ASR 을 부른다.
	if j.Audio.Object != "" || j.Audio.Bucket != "" {
		t.Fatalf("없는 오디오가 작업에 들어갔다: %+v", j.Audio)
	}
	// 레코드가 아는 값은 그대로 옮겨져야 한다 — 이 값들이 분석 완료 시 레코드에 되돌아 쓰인다.
	if j.ContactName != "강연정" || j.ContactPhone != "01012345678" || j.FileName != "call-old.m4a" || j.Duration != 1706 {
		t.Fatalf("레코드 메타가 작업에 옮겨지지 않았다: %+v", j)
	}
	if j.RecordedAt.IsZero() || j.RecordedAt.UTC().Format("2006-01-02T15:04:05Z") != "2026-09-01T05:00:00Z" {
		t.Fatalf("통화일시가 비었거나 틀렸다: %v", j.RecordedAt)
	}
	// ⚠️ 기기 모델 이름은 작업 문서로 따라오면 안 된다(§audio.go jobFromRecord).
	if j.LLMModel != "" || j.ASRModel != "" || j.PromptVersion != "" {
		t.Fatalf("기기 모델 이름이 작업에 들어갔다: llm=%s asr=%s prompt=%s", j.LLMModel, j.ASRModel, j.PromptVersion)
	}

	if res := h.runTick(); res.Claimed != 1 || res.Advanced != 1 || res.Failed != 0 {
		t.Fatalf("tick 결과: %+v", res)
	}
	if j = h.job(t, "call-old"); j.State != stateCompleted {
		t.Fatalf("분석이 끝나지 않았다: %s (%s)", j.State, j.ErrorCode)
	}

	// 🔴 ASR 을 한 번도 부르지 않았다.
	if asr.StartCalls != 0 || asr.PollCalls != 0 {
		t.Fatalf("없는 오디오로 ASR 을 불렀다: start=%d poll=%d", asr.StartCalls, asr.PollCalls)
	}
	// 🔴 서버 ASR 을 쓴 적이 없으므로 오디오 초는 0 이다. 통화 길이(1706초)를 여기 넣으면
	// 쓰지도 않은 170.6원이 화면에 뜬다.
	if j.Usage.AudioSeconds != 0 {
		t.Fatalf("쓰지 않은 받아쓰기 사용량이 기록됐다: %v초", j.Usage.AudioSeconds)
	}
	// 분석 모델은 **이번에 실제로 답한** 모델로 갱신돼야 한다. 그러지 않으면 상세 화면의
	// 「분석: … · 모델」이 어느 모델이 만든 결과인지 거짓말을 한다.
	if j.LLMModel != "fake-llm" || j.PromptVersion != "fake-1" || j.LLMProvider != "fake" {
		t.Fatalf("모델 정보가 갱신되지 않았다: %s/%s/%s", j.LLMProvider, j.LLMModel, j.PromptVersion)
	}

	got := decodeRecordBody(t, h.do(t, "GET", "/calls/call-old", "owner-a", ""))
	if got.AI == nil || got.AI.Model != "fake-llm" || got.AI.ModelVersion != "fake-1" || got.AI.Provider != "fake" || got.AI.ProcessedOnDevice {
		t.Fatalf("상세의 모델 표기가 기기 값 그대로다: %+v", got.AI)
	}
	if got.Analysis == nil || !strings.HasPrefix(got.Analysis.Summary, "요약:") {
		t.Fatalf("분석이 새로 채워지지 않았다: %+v", got.Analysis)
	}
	if got.Transcript == nil || got.Transcript.Text != "견적서 전달해주세요." {
		t.Fatalf("원문이 보존되지 않았다: %+v", got.Transcript)
	}
	// 재분석이 통화 메타를 갈아 치우면 목록에서 그 통화가 사라진 것처럼 보인다.
	if got.Contact.Name != "강연정" || got.Call.Duration == nil || *got.Call.Duration != 1706 {
		t.Fatalf("통화 메타가 손상됐다: %+v", got)
	}
	if !strings.HasPrefix(got.Call.RecordedAt, "2026-09-01T05:00:00") {
		t.Fatalf("통화일시가 덮어써졌다: %s", got.Call.RecordedAt)
	}
	// 🔴 원가의 받아쓰기 몫은 0원이다. 분석만 0.3원(입력 100 × 0.001 + 출력 50 × 0.004).
	if got.Cost == nil || got.Cost.Transcription != 0 || got.Cost.Usage.AudioSeconds != 0 {
		t.Fatalf("쓰지 않은 받아쓰기 요금이 붙었다: %+v", got.Cost)
	}
	if got.Cost.Analysis != 0.3 || got.Cost.Total != 0.3 {
		t.Fatalf("분석 원가: %+v", got.Cost)
	}
	// GCS 에 원본이 없으므로 재생 주소는 없다. **실패가 아니다.**
	if got.HasAudio || got.AudioURL != nil {
		t.Fatalf("없는 오디오의 재생 주소가 생겼다: %v %v", got.HasAudio, got.AudioURL)
	}
}

// 2. 원문이 없으면 거절한다. **그리고 작업 문서를 남기지 않는다** — 남기면 그 통화는
// 다음부터 「작업은 있는데 전사문은 없는」 상태로 스윕 후보에 들어간다.
func TestReanalyzeWithoutTranscriptIsRejected(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	// 원문이 사라진 옛 통화(보관 정리 등). 서버 경로 검증으로 저장해 transcript 만 비운다.
	rec := deviceRecord("call-bare")
	rec.Transcript, rec.Analysis, rec.AI = nil, nil, nil
	pay, err := prepareServer(&rec, "call-bare")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.recs.SaveOverwrite(context.Background(), "owner-a", pay); err != nil {
		t.Fatal(err)
	}

	w := h.do(t, "POST", "/calls/call-bare/reanalyze", "owner-a", "")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "CALL_NO_TRANSCRIPT") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if _, err := h.jobs.Get(context.Background(), "call-bare"); status.Code(err) != codes.NotFound {
		t.Fatalf("거절해 놓고 작업 문서를 남겼다: %v", err)
	}
	// 통화 자체가 없는 경우는 404 다 — 「원문이 없다」와 「그런 통화가 없다」는 다른 말이다.
	if w := h.do(t, "POST", "/calls/call-none/reanalyze", "owner-a", ""); w.Code != 404 {
		t.Fatalf("없는 통화가 %d: %s", w.Code, w.Body.String())
	}
}

// 3. 🔴 남의 통화 ID 로 재분석을 눌러 **그 작업 문서를 가로챌 수 없다.**
//
// 통화 ID 는 앱이 정하는 값이라 남의 것과 같은 ID 로 자기 통화를 만들 수 있다. 「작업이
// 없으면 레코드에서 만든다」를 소유권 확인 없이 하면, 그 순간 남의 작업 문서가 내 uid 로
// 덮어써진다 — 남의 통화 분석 결과가 내 목록에 들어온다.
func TestReanalyzeDoesNotHijackOthersJob(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	h.enqueue(t, "owner-a", "call-1")
	h.runTick()
	// owner-b 가 같은 ID 로 자기 통화를 올린다. 레코드는 uid 아래에 있으므로 이것은 정상이다.
	putDevice(t, h, "owner-b", deviceRecord("call-1"))

	if w := h.do(t, "POST", "/calls/call-1/reanalyze", "owner-b", ""); w.Code != 404 {
		t.Fatalf("남의 통화 재분석이 %d: %s", w.Code, w.Body.String())
	}
	if j := h.job(t, "call-1"); j.UID != "owner-a" || j.State != stateCompleted {
		t.Fatalf("작업 문서가 탈취됐다: uid=%s state=%s", j.UID, j.State)
	}
}

// 4. 🔴 전사문 조각이 사라져도 **없는 오디오로 ASR 을 다시 부르지 않는다.**
//
// stepAnalyze 는 전사문을 못 읽으면 「있는 오디오로 처음부터 다시」를 시도한다(requeueASR).
// 그 길이 열려 있으면, 오디오가 애초에 없는 이 통화들이 ASR 재시도 상한(3회)까지
// 없는 파일을 올리려 든다. 확정 실패로 끊는 것이 맞다.
func TestReanalyzeNeverCallsASRWithoutAudio(t *testing.T) {
	asr := &callai.FakeTranscriber{Result: fakeResult()}
	h := newHarness(asr, &callai.FakeAnalyzer{})
	putDevice(t, h, "owner-a", deviceRecord("call-old"))
	requireNoJob(t, h, "call-old")
	if w := h.do(t, "POST", "/calls/call-old/reanalyze", "owner-a", ""); w.Code != 200 {
		t.Fatalf("재분석 예약: %d %s", w.Code, w.Body.String())
	}
	// 전사문 조각이 사라진 상황을 만든다(수명주기 정리, 부분 쓰기).
	h.jobs.mu.Lock()
	delete(h.jobs.transcripts, "call-old")
	h.jobs.mu.Unlock()

	h.runTick()
	j := h.job(t, "call-old")
	if j.State != stateTranscriptionFailed {
		t.Fatalf("오디오 없이 다시 받아쓰려 했다: %s", j.State)
	}
	if asr.StartCalls != 0 || asr.PollCalls != 0 {
		t.Fatalf("없는 오디오로 ASR 을 불렀다: start=%d poll=%d", asr.StartCalls, asr.PollCalls)
	}
	// 종료 상태라 다음 tick 이 다시 집지 않는다 — 조용히 도는 작업을 남기지 않는다.
	if res := h.runTick(); res.Claimed != 0 {
		t.Fatalf("끝난 작업을 다시 집었다: %+v", res)
	}
}
