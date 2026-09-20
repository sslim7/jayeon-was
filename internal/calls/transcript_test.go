package calls

import (
	"strings"
	"testing"
	"time"

	"github.com/sslim7/nature-was/internal/callai"
)

// transcript_test.go 는 **기기(폰 whisper)가 받아쓴 전사문을 받아 분석만 돌리는 경로**를 지킨다.
//
// 이 경로에서 조용히 깨질 수 있는 것이 넷이다:
//
//	① 서버가 기기 받아쓰기 통화를 **한 번 더 받아쓴다** → 요금이 그대로 두 배다.
//	② 서버가 받아쓰는 중인 통화에 남이 전사문을 **밀어넣는다** → 두 결과가 경쟁한다.
//	③ 폰이 영영 안 돌아온 통화가 **영원히 「기기에서 받아쓰는 중」으로 남는다.**
//	④ 쓰지도 않은 받아쓰기 요금이 원가에 뜬다.
//
// 그리고 무엇보다, **asr 를 보내지 않는 기존 요청의 동작이 한 글자도 달라지면 안 된다** —
// 사용자가 로컬 받아쓰기를 끄면 지금과 완전히 똑같이 도는 것이 이 기능의 안전망이다.

// clientTranscriptBody 는 폰이 올리는 전사문 한 벌이다(기기 whisper 출력 모양).
const clientTranscriptBody = `{"text":"견적서 전달해주세요.","segments":[{"start":0,"end":5,"text":"견적서 전달해주세요.","speaker":"A"}]}`

// enqueueClient 는 upload-url → (GCS PUT) → complete{asr:"client"} 까지를 실제 HTTP 로 밟는다.
// 🔴 오디오 원본은 기기 받아쓰기에서도 그대로 올라간다 — 재생 기능이 그것을 쓴다.
func enqueueClient(t *testing.T, h *harness, uid, id string) {
	t.Helper()
	res := decodeUpload(t, h, uid, id)
	h.audio.put(res.Object, 2048)
	if w := h.do(t, "POST", "/calls/"+id+"/audio/complete", uid, `{"asr":"client"}`); w.Code != 200 {
		t.Fatalf("complete(client): %d %s", w.Code, w.Body.String())
	}
}

// 1. 정상 흐름: 기기 받아쓰기로 큐잉 → tick 은 손대지 않는다 → 전사문 도착 → 분석 완료.
//
// 🔴 ASR 공급자는 한 번도 불리지 않아야 한다. 부른다는 것은 폰이 이미 한 일을 돈 주고
// 다시 하는 것이고, 화면만 봐서는 알아챌 수 없다(결과가 똑같이 나온다).
func TestClientTranscriptHappyPath(t *testing.T) {
	asr := &callai.FakeTranscriber{Result: fakeResult(), AudioSeconds: 1706}
	h := newHarness(asr, &callai.FakeAnalyzer{})
	h.audioH.pricing = testPricing()
	start := h.clock()
	enqueueClient(t, h, "owner-a", "call-1")

	j := h.job(t, "call-1")
	if j.State != stateASRRunning || !j.clientASR() {
		t.Fatalf("기기 받아쓰기 표시가 없다: state=%s clientAsrAt=%v", j.State, j.ClientASRAt)
	}
	if j.Stage != stageClientTranscribing {
		t.Fatalf("단계명이 %q 다 — 폰이 받아쓰는 중이라는 사실이 화면에 드러나야 한다", j.Stage)
	}
	// 🔴 마감까지 미뤄 두지 않으면 이 작업이 매분 tick 에 잡혀 6시간 내내 빈 조회·쓰기만 쌓는다.
	if want := start.Add(clientTranscriptTimeout); !j.NextAttemptAt.Equal(want) {
		t.Fatalf("다음 확인 시각 %v, want %v", j.NextAttemptAt, want)
	}

	// 앱은 이미 쓰고 있는 GET 으로 진행 상태를 그대로 본다 — 새 폴링 API 를 만들지 않는다.
	got := decodeRecordBody(t, h.do(t, "GET", "/calls/call-1", "owner-a", ""))
	if got.Status != "TRANSCRIBING" || got.JobState != stateASRRunning || got.Stage != stageClientTranscribing {
		t.Fatalf("진행 상태가 안 보인다: %+v", got)
	}
	if !got.HasAudio {
		t.Fatal("기기 받아쓰기에서도 원본 오디오는 올라간다(재생 기능)")
	}

	// tick 은 이 작업을 집지 않는다.
	if res := h.runTick(); res.Claimed != 0 {
		t.Fatalf("tick 이 기기 받아쓰기 작업을 집었다: %+v", res)
	}
	if asr.StartCalls != 0 || asr.PollCalls != 0 {
		t.Fatalf("폰이 받아쓰는 통화에 서버 ASR 을 불렀다: start=%d poll=%d", asr.StartCalls, asr.PollCalls)
	}

	// 전사문 도착 → 분석 단계로 진입한다.
	queued := decodeRecordBody(t, h.do(t, "POST", "/calls/call-1/transcript", "owner-a", clientTranscriptBody))
	if queued.Status != "ANALYZING" || queued.JobState != stateTranscribed {
		t.Fatalf("전사문 응답이 분석 대기가 아니다: %+v", queued)
	}
	if j = h.job(t, "call-1"); j.TranscriptShards != 1 || j.NextAttemptAt.After(h.clock()) {
		t.Fatalf("분석이 바로 이어지지 않는다: shards=%d next=%v", j.TranscriptShards, j.NextAttemptAt)
	}

	if res := h.runTick(); res.Claimed != 1 || res.Failed != 0 {
		t.Fatalf("tick 이 분석을 잇지 못했다: %+v", res)
	}
	if j = h.job(t, "call-1"); j.State != stateCompleted {
		t.Fatalf("분석이 끝나지 않았다: %s (%s)", j.State, j.ErrorCode)
	}
	if asr.StartCalls != 0 || asr.PollCalls != 0 {
		t.Fatalf("서버 ASR 이 불렸다: start=%d poll=%d", asr.StartCalls, asr.PollCalls)
	}
	if h.llm.Calls != 1 {
		t.Fatalf("분석 호출 %d 회", h.llm.Calls)
	}

	got = decodeRecordBody(t, h.do(t, "GET", "/calls/call-1", "owner-a", ""))
	if got.Transcript == nil || got.Transcript.Text != "견적서 전달해주세요." {
		t.Fatalf("기기가 보낸 원문이 남지 않았다: %+v", got.Transcript)
	}
	if got.Analysis == nil || got.Status != "COMPLETED" {
		t.Fatalf("분석 결과가 없다: %+v", got)
	}
	// 🔴 받아쓰기 비용은 0원이다. 통화 길이를 사용량에 옮겨 적으면 여기가 170.6원이 된다
	// (§cost.go testPricing: 초당 0.1원) — 그 숫자는 진짜와 구분되지 않는다.
	if got.Cost == nil {
		t.Fatal("비용이 붙지 않았다")
	}
	if got.Cost.Transcription != 0 || got.Cost.Usage.AudioSeconds != 0 {
		t.Fatalf("쓰지도 않은 받아쓰기 요금이 뜬다: %+v", got.Cost)
	}
	// 분석(LLM) 비용은 그대로 든다: 100×0.001 + 50×0.004 = 0.3원.
	if got.Cost.Analysis != 0.3 || got.Cost.Total != 0.3 {
		t.Fatalf("분석 비용 %+v", got.Cost)
	}
}

// 2. 🔴 서버가 받아쓰는 중인 통화에는 전사문을 밀어넣을 수 없다(409).
//
// 두면 공급자 전사와 앱 전사가 경쟁해 어느 쪽이 남는지 알 수 없고, 서버는 이미 나간
// ASR 요금을 그대로 문다.
func TestClientTranscriptRejectsServerASRJob(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	h.enqueue(t, "owner-a", "call-1") // asr 없이 = 서버 받아쓰기

	w := h.do(t, "POST", "/calls/call-1/transcript", "owner-a", clientTranscriptBody)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "CALL_NOT_CLIENT_ASR") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	j := h.job(t, "call-1")
	if j.State != stateQueued || j.TranscriptShards != 0 {
		t.Fatalf("서버 작업이 밀어넣기에 흔들렸다: state=%s shards=%d", j.State, j.TranscriptShards)
	}
}

// 3. 같은 요청을 두 번 보내도 안전하다. 앱이 응답을 못 받고 재시도하는 경우가 실제로 있다.
//
// 🔴 두 번째를 409 로 돌려주면 사용자는 **이미 잘 처리된 통화를 실패로 본다.**
// 그리고 분석 시도 횟수가 되감기면 비싼 LLM 호출이 한 번 더 나간다.
func TestClientTranscriptRetryIsSafe(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	enqueueClient(t, h, "owner-a", "call-1")

	first := decodeRecordBody(t, h.do(t, "POST", "/calls/call-1/transcript", "owner-a", clientTranscriptBody))
	second := decodeRecordBody(t, h.do(t, "POST", "/calls/call-1/transcript", "owner-a", clientTranscriptBody))
	if first.JobState != stateTranscribed || second.JobState != stateTranscribed {
		t.Fatalf("재시도 응답이 다르다: %s vs %s", first.JobState, second.JobState)
	}

	h.runTick()
	if j := h.job(t, "call-1"); j.State != stateCompleted {
		t.Fatalf("분석이 끝나지 않았다: %s (%s)", j.State, j.ErrorCode)
	}
	if h.llm.Calls != 1 {
		t.Fatalf("중복 요청이 분석을 두 번 부르게 했다: %d", h.llm.Calls)
	}

	// 🔴 끝난 통화의 원문을 갈아 끼우면 사용자가 이미 본 분석과 원문이 어긋난다.
	// 다시 분석하고 싶으면 기존 reanalyze 를 쓴다.
	w := h.do(t, "POST", "/calls/call-1/transcript", "owner-a", clientTranscriptBody)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "CALL_ALREADY_COMPLETED") {
		t.Fatalf("완료된 통화에 %d: %s", w.Code, w.Body.String())
	}
	if h.llm.Calls != 1 {
		t.Fatalf("분석 호출 %d 회", h.llm.Calls)
	}
}

// 4. 빈 전사문은 400 이다. 빈 원문을 분석에 넘겨 봐야 **지어낸 요약**만 나오고, 요금은 나간다.
func TestClientTranscriptRejectsEmptyBody(t *testing.T) {
	for _, c := range []struct {
		name, body, code string
		status           int
	}{
		{"빈 문자열", `{"text":"","segments":[]}`, "CALL_EMPTY_TRANSCRIPT", 400},
		{"공백뿐", `{"text":"   \n","segments":[]}`, "CALL_EMPTY_TRANSCRIPT", 400},
		{"본문 없음", ``, "CALL_EMPTY_TRANSCRIPT", 400},
		{"빈 세그먼트 텍스트", `{"text":"안녕하세요","segments":[{"start":0,"end":1,"text":""}]}`, "VALIDATION_FAILED", 400},
		{"뒤섞인 세그먼트 순서", `{"text":"안녕하세요","segments":[{"start":5,"end":6,"text":"뒤"},{"start":0,"end":1,"text":"앞"}]}`, "VALIDATION_FAILED", 400},
		{"JSON 이 아님", `{`, "VALIDATION_FAILED", 400},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
			enqueueClient(t, h, "owner-a", "call-1")
			w := h.do(t, "POST", "/calls/call-1/transcript", "owner-a", c.body)
			if w.Code != c.status || !strings.Contains(w.Body.String(), c.code) {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			// 🔴 거절한 요청이 작업을 건드리면 안 된다. 상태가 밀리면 폰이 제대로 된
			// 전사문을 다시 보내도 받아 주지 않는다.
			j := h.job(t, "call-1")
			if j.State != stateASRRunning || j.TranscriptShards != 0 {
				t.Fatalf("거절된 요청이 작업을 바꿨다: state=%s shards=%d", j.State, j.TranscriptShards)
			}
		})
	}
}

// 5. 🔴 폰이 영영 안 돌아오는 경우가 반드시 생긴다(앱 삭제, 기기 분실, 사용자 포기).
// 6시간이 지나면 tick 이 정리한다 — 안 하면 그 통화는 영원히 「기기에서 받아쓰는 중」이다.
func TestClientTranscriptTimeoutIsSweptUp(t *testing.T) {
	asr := &callai.FakeTranscriber{Result: fakeResult()}
	h := newHarness(asr, &callai.FakeAnalyzer{})
	enqueueClient(t, h, "owner-a", "call-1")

	// 마감 전에는 건드리지 않는다. 폰이 몇 시간 잠겨 있는 것은 정상이다.
	h.advance(clientTranscriptTimeout - time.Minute)
	if res := h.runTick(); res.Claimed != 0 || res.Failed != 0 {
		t.Fatalf("마감 전에 정리했다: %+v", res)
	}
	if j := h.job(t, "call-1"); j.State != stateASRRunning {
		t.Fatalf("마감 전 상태 %s", j.State)
	}

	h.advance(2 * time.Minute)
	if res := h.runTick(); res.Claimed != 1 || res.Failed != 1 {
		t.Fatalf("마감이 지났는데 정리하지 않았다: %+v", res)
	}
	j := h.job(t, "call-1")
	if j.State != stateTranscriptionFailed || j.ErrorCode != codeClientTranscriptTimeout {
		t.Fatalf("정리 결과: state=%s code=%s", j.State, j.ErrorCode)
	}
	// 🔴 정리한다고 서버가 대신 받아쓰지 않는다. 그 순간 요금이 나간다.
	if asr.StartCalls != 0 {
		t.Fatalf("정리하면서 서버 ASR 을 불렀다: %d", asr.StartCalls)
	}
	got := decodeRecordBody(t, h.do(t, "GET", "/calls/call-1", "owner-a", ""))
	if got.Status != "TRANSCRIPTION_FAILED" || got.Error == nil || *got.Error != codeClientTranscriptTimeout {
		t.Fatalf("화면에 실패가 보이지 않는다: %+v", got)
	}

	// 늦게라도 전사문이 오면 받는다 — 거절하면 원문이 요청 본문에 있는데도 그 통화는
	// 원문도 분석도 없이 실패로만 남는다.
	late := decodeRecordBody(t, h.do(t, "POST", "/calls/call-1/transcript", "owner-a", clientTranscriptBody))
	if late.JobState != stateTranscribed {
		t.Fatalf("늦게 온 전사문을 받지 않았다: %+v", late)
	}
	h.runTick()
	if j = h.job(t, "call-1"); j.State != stateCompleted {
		t.Fatalf("늦게 온 전사문으로 분석이 끝나지 않았다: %s (%s)", j.State, j.ErrorCode)
	}
}

// 6. 🔴 **asr 를 보내지 않는 기존 요청은 예전과 똑같이 동작한다.** 이 테스트가 깨지면
// 구버전 앱과 웹이 그대로 멈춘다 — 로컬 받아쓰기를 끄는 것이 이 기능의 안전망인데
// 그 안전망 자체가 없어진다.
func TestCompleteWithoutASRFieldIsUnchanged(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"본문 없음(구버전 앱)", ``},
		{"빈 객체", `{}`},
		{"명시적 server", `{"asr":"server"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			asr := &callai.FakeTranscriber{Result: fakeResult(), AudioSeconds: 321}
			h := newHarness(asr, &callai.FakeAnalyzer{})
			res := decodeUpload(t, h, "owner-a", "call-1")
			h.audio.put(res.Object, 2048)
			now := h.clock()
			if w := h.do(t, "POST", "/calls/call-1/audio/complete", "owner-a", c.body); w.Code != 200 {
				t.Fatalf("complete: %d %s", w.Code, w.Body.String())
			}
			j := h.job(t, "call-1")
			if j.State != stateQueued || j.clientASR() {
				t.Fatalf("서버 받아쓰기 경로가 달라졌다: state=%s clientAsrAt=%v", j.State, j.ClientASRAt)
			}
			if j.Stage != stageOf(stateQueued) || !j.NextAttemptAt.Equal(now) {
				t.Fatalf("큐잉 모양이 달라졌다: stage=%q next=%v", j.Stage, j.NextAttemptAt)
			}
			// 그리고 tick 이 예전처럼 서버 ASR 로 집어간다.
			if r := h.runTick(); r.Claimed != 1 {
				t.Fatalf("tick 이 집지 않았다: %+v", r)
			}
			if asr.StartCalls != 1 {
				t.Fatalf("서버 ASR 호출 %d 회", asr.StartCalls)
			}
			if j = h.job(t, "call-1"); j.State != stateTranscribed && j.State != stateCompleted {
				t.Fatalf("서버 경로 진행 상태 %s (%s)", j.State, j.ErrorCode)
			}
			// 🔴 서버가 받아쓴 통화의 사용량은 그대로 집계된다(0원이 아니다).
			if j.Usage.AudioSeconds != 321 {
				t.Fatalf("서버 받아쓰기 사용량 %v", j.Usage.AudioSeconds)
			}
		})
	}
}

// 7. 모르는 asr 값은 400 이다. 서버 받아쓰기로 눙치면 요금이 나가는 쪽으로 조용히 흐른다.
func TestCompleteRejectsUnknownASRValue(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	res := decodeUpload(t, h, "owner-a", "call-1")
	h.audio.put(res.Object, 2048)
	for _, body := range []string{`{"asr":"phone"}`, `{"asr":"CLIENT"}`, `{"asr":`} {
		if w := h.do(t, "POST", "/calls/call-1/audio/complete", "owner-a", body); w.Code != 400 {
			t.Fatalf("%s → %d %s", body, w.Code, w.Body.String())
		}
	}
	if j := h.job(t, "call-1"); j.State != stateAwaitingUpload {
		t.Fatalf("거절된 요청이 작업을 큐잉했다: %s", j.State)
	}
}

// 8. 기기 받아쓰기를 기다리는 통화는 재분석 대상이 아니다 — 아직 분석할 원문이 없다.
func TestReanalyzeRejectsWaitingClientJob(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	enqueueClient(t, h, "owner-a", "call-1")
	w := h.do(t, "POST", "/calls/call-1/reanalyze", "owner-a", "")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "CALL_NOT_ANALYZABLE") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
