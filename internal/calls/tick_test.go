package calls

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sslim7/nature-was/internal/callai"
	"google.golang.org/api/idtoken"
)

// 7. 🔴 **이 핸들러가 유일한 방어선이다.** Cloud Run 이 allow_unauthenticated 라 IAM 은
// tick 을 전혀 막아 주지 않는다 — 통과시키면 그 요청마다 외부 ASR/LLM 요금이 나간다.
func TestTickAuth(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	h.enqueue(t, "owner-a", "call-1")

	for _, c := range []struct {
		name, header string
		want         int
	}{
		{"토큰 없음", "", 401},
		{"Bearer 아님", "local-secret", 401},
		{"틀린 토큰", "Bearer wrong-secret", 401},
		{"길이만 다른 토큰", "Bearer local-secre", 401},
		{"올바른 공유 토큰", "Bearer local-secret", 200},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/internal/calls/tick", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			w := httptest.NewRecorder()
			h.mux.ServeHTTP(w, req)
			if w.Code != c.want {
				t.Fatalf("%d (want %d): %s", w.Code, c.want, w.Body.String())
			}
		})
	}
	// 통과한 요청만 실제로 일을 했어야 한다.
	if h.asr.StartCalls != 1 {
		t.Fatalf("인증 실패 요청이 공급자를 불렀다: %d", h.asr.StartCalls)
	}
}

// OIDC 경로. 🔴 검증 항목은 네 개다: ①구글 서명 ②aud 일치(둘은 idtoken.Validate 가 본다)
// ③email 일치 ④email_verified == true. ④를 빠뜨리면 검증되지 않은 이메일 클레임을 믿게 된다.
func TestTickOIDCValidation(t *testing.T) {
	const caller = "jayeon-call-tick@example.iam.gserviceaccount.com"
	const audience = "https://was.example.run.app"

	payload := func(email string, verified bool) *idtoken.Payload {
		return &idtoken.Payload{Claims: map[string]any{"email": email, "email_verified": verified}}
	}
	for _, c := range []struct {
		name  string
		valid tokenValidator
		want  bool
	}{
		{"정상", func(_ context.Context, _, aud string) (*idtoken.Payload, error) {
			if aud != audience {
				return nil, errors.New("aud 불일치")
			}
			return payload(caller, true), nil
		}, true},
		{"서명/aud 검증 실패", func(context.Context, string, string) (*idtoken.Payload, error) {
			return nil, errors.New("invalid token")
		}, false},
		{"다른 서비스 계정", func(context.Context, string, string) (*idtoken.Payload, error) {
			return payload("someone-else@example.com", true), nil
		}, false},
		{"email_verified=false", func(context.Context, string, string) (*idtoken.Payload, error) {
			return payload(caller, false), nil
		}, false},
		{"email 클레임 없음", func(context.Context, string, string) (*idtoken.Payload, error) {
			return &idtoken.Payload{Claims: map[string]any{}}, nil
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := &tickAuth{validate: c.valid, audience: audience, caller: caller}
			req := httptest.NewRequest("POST", "/internal/calls/tick", nil)
			req.Header.Set("Authorization", "Bearer any-token")
			if got := a.ok(context.Background(), req); got != c.want {
				t.Fatalf("ok=%v want %v", got, c.want)
			}
		})
	}

	// 공유 토큰이 설정돼 있지 않으면 OIDC 검증기가 없을 때 무조건 거부해야 한다.
	empty := &tickAuth{}
	req := httptest.NewRequest("POST", "/internal/calls/tick", nil)
	req.Header.Set("Authorization", "Bearer anything")
	if empty.ok(context.Background(), req) {
		t.Fatal("설정이 비었는데 통과시켰다")
	}
}

// tick 응답에는 개수와 남은 예산만 들어간다. 🔴 통화 내용은 한 글자도 넣지 않는다.
func TestTickResponseShape(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Polls: 2, Result: fakeResult()}, &callai.FakeAnalyzer{})
	h.enqueue(t, "owner-a", "call-1")
	req := httptest.NewRequest("POST", "/internal/calls/tick", nil)
	req.Header.Set("Authorization", "Bearer local-secret")
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"claimed": true, "advanced": true, "failed": true, "remaining_budget_ms": true}
	for k := range body {
		if !want[k] {
			t.Fatalf("응답에 예상 밖 필드가 있다: %s", k)
		}
	}
	if len(body) != len(want) {
		t.Fatalf("응답 필드가 모자라다: %v", body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("tick 응답이 캐시될 수 있다")
	}
}

// 🔴 예산이 모자라면 상태를 저장하고 반환한다. 응답 뒤에 도는 고루틴은 없다 —
// Cloud Run 이 cpu_idle=true 라 응답 직후 CPU 가 스로틀되어 조용히 끊긴다.
func TestTickStopsAtBudgetAndResumesNextTick(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Polls: 100, Result: fakeResult()}, &callai.FakeAnalyzer{})
	h.enqueue(t, "owner-a", "call-1")

	res := h.tick.run(context.Background(), h.clock().Add(tickBudget))
	j := h.job(t, "call-1")
	if j.State != stateASRPolling || j.ASRToken == "" {
		t.Fatalf("폴링 상태가 저장되지 않았다: %s token=%q", j.State, j.ASRToken)
	}
	// 다음 폴링 한 번(대기 + 호출 여유)이 안 들어갈 만큼만 남기고 멈춰야 한다.
	if res.RemainingBudgetMS > int64((pollWait+stepReserve)/time.Millisecond) {
		t.Fatalf("예산을 남기고 멈췄다: %d ms", res.RemainingBudgetMS)
	}
	polled := h.asr.PollCalls
	if polled == 0 {
		t.Fatal("한 번도 폴링하지 않았다")
	}
	// 🔴 예산이 모자라 멈춘 작업은 lease 를 풀어 **다음 tick 이 바로** 이어받게 한다.
	// 풀지 않으면 lease 만료(2분)까지 그 통화가 놀게 된다.
	if j.NextAttemptAt.After(h.clock()) {
		t.Fatalf("lease 가 풀리지 않았다: %v > %v", j.NextAttemptAt, h.clock())
	}
	if next := h.runTick(); next.Claimed != 1 {
		t.Fatalf("다음 tick 이 이어받지 못했다: %+v", next)
	}
	if h.asr.PollCalls <= polled {
		t.Fatal("다음 tick 이 폴링을 잇지 않았다")
	}
	if h.asr.StartCalls != 1 {
		t.Fatalf("이어받으면서 전사를 다시 시작했다: %d", h.asr.StartCalls)
	}
}

// 배치 크기 안에서 여러 통화를 동시에 전진시킨다.
func TestTickAdvancesBatch(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	for _, id := range []string{"call-1", "call-2", "call-3"} {
		h.enqueue(t, "owner-a", id)
	}
	res := h.runTick()
	if res.Claimed != 3 || res.Advanced != 3 || res.Failed != 0 {
		t.Fatalf("%+v", res)
	}
	for _, id := range []string{"call-1", "call-2", "call-3"} {
		if j := h.job(t, id); j.State != stateCompleted {
			t.Fatalf("%s: %s", id, j.State)
		}
	}
}

// AWAITING_UPLOAD 는 스윕 대상이 아니다 — 아직 올라오지 않은 오디오를 전사하려 들면 안 된다.
func TestTickIgnoresAwaitingUpload(t *testing.T) {
	h := newHarness(&callai.FakeTranscriber{Result: fakeResult()}, &callai.FakeAnalyzer{})
	decodeUpload(t, h, "owner-a", "call-1")
	if res := h.runTick(); res.Claimed != 0 {
		t.Fatalf("업로드 전 작업을 집었다: %+v", res)
	}
	if h.asr.StartCalls != 0 {
		t.Fatalf("공급자를 불렀다: %d", h.asr.StartCalls)
	}
}
