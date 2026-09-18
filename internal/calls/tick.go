package calls

import (
	"context"
	"crypto/subtle"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sslim7/nature-was/internal/httpx"
	"google.golang.org/api/idtoken"
)

// tick.go 는 Cloud Scheduler 가 1분마다 두드리는 파이프라인 구동부다.
//
// # 🔴 이 핸들러가 유일한 방어선이다
//
// Cloud Run 서비스가 `allow_unauthenticated = true` 다. **Cloud Run IAM 은 이 경로를 전혀
// 막아 주지 않는다** — 아무나 curl 로 때릴 수 있고, 통과시키면 그 요청마다 외부 ASR/LLM
// 요금이 나간다. 검증에 실패하면 무슨 일이 있어도 200 을 돌려주지 않는다.
// 검증 항목은 네 개다: ① 구글 서명 ② aud 일치 ③ email 일치 ④ email_verified == true.
//
// # 🔴 응답을 보낸 뒤 도는 고루틴 금지
//
// Cloud Run 이 `cpu_idle = true` / `min_instances = 0` 이다. 응답 직후 CPU 가 스로틀되어
// 뒤에서 돌던 작업이 **조용히** 끊긴다(로그에도 아무것도 안 남는다). 모든 일은 요청 안에서
// 끝나고, 못 끝낸 것은 상태로 저장해 다음 tick 에 넘긴다.
//
// # 예산
//
// Cloud Run 요청 타임아웃이 60초이고 Scheduler attempt_deadline 도 60초다. 우리는 50초를
// 쓰고 나머지를 상태 저장·응답에 남긴다. 한 작업을 예산이 허락하는 한 여러 단계 전진시키되,
// 남은 예산이 모자라면 **그 단계를 시작하지 않고** 상태를 저장하고 반환한다 —
// 다음 tick 이 이어서 한다.
//
// # 🔴 단계마다 필요한 시간이 다르다
//
// 2026-09-18, 25분짜리 상담 통화의 첫 서버 분석이 여기서 깨졌다. 그때는 「한 단계 더 갈 수
// 있는가」를 stepReserve 하나(12초)로 판단했는데, ASR 폴링은 12초로 충분해도 전사문을
// 정리하는 LLM 호출은 **30초 안팎이 걸린다**(합성 25분 전사문 실측 28.5~33.6초,
// internal/callai/live_test.go 의 TestLiveAnalyzeLatency). 남은 13초로 LLM 을 부른 결과는
// 타임아웃 하나였고, 그 타임아웃이 확정 실패로 분류되어 통화가 통째로 버려졌다.
// 그래서 여유는 **단계별**로 잡는다. 하나의 상수로 되돌리면 같은 사고가 되풀이된다.
//
// # 🔴 시간 상수들은 서로 물려 있다
//
//	leaseDuration(2m)  >  tickBudget(50s)              ← 아래 「lease 와 중복 실행」
//	tickBudget(50s)    <  Cloud Run(60s) - tailReserve(3s)
//	tickBudget(50s)    >= llmCallNeed(45s)             ← 빈 tick 에 LLM 한 번은 들어가야 한다
//	llmCallNeed(45s)   =  LLM 실측 최악(33.6s) + 여유 + saveReserve(5s)
//	CALL_AI_TIMEOUT_SECONDS(50s) > 우리가 거는 최대 창(tickBudget - saveReserve = 45s)
//
// 마지막 줄이 중요하다. 공급자 HTTP 클라이언트의 타임아웃이 우리 창보다 **짧으면**
// 언제나 그쪽이 먼저 끊긴다 — 2026-09-18 에 실제로 그랬다(30초). 우리 창이 먼저 끊겨야
// 「공급자가 우리가 준 시간 안에 못 끝냈다」를 우리 쪽에서 일관되게 분류할 수 있다.
//
// # lease 와 중복 실행
//
// Scheduler 주기는 1분, lease 는 2분이다. tick 이 1분을 넘겨 다음 tick 과 겹쳐도(Cloud Run
// concurrency 80 / max_instances 3 이라 실제로 겹친다) 앞 tick 이 집은 작업은 lease 가
// 살아 있어 뒤 tick 의 스윕 쿼리에 잡히지 않는다. lease 가 주기보다 짧으면 두 tick 이
// 같은 작업을 동시에 붙잡아 공급자를 두 번 부른다 = 요금이 그대로 두 배다.

const (
	// tickBudget 은 한 tick 이 쓰는 시간이다. Cloud Run 상한(60초)보다 짧아야 한다.
	tickBudget = 50 * time.Second
	// tailReserve 는 응답을 쓰기 위해 남겨 두는 시간이다.
	tailReserve = 3 * time.Second
	// saveReserve 는 공급자 호출이 끝난 뒤 **결과를 저장할** 시간이다.
	// 전사문 shard 쓰기, 분석 저장, 통화 레코드 확정이 여기 들어간다. 이것을 떼지 않고
	// 남은 시간을 통째로 공급자에게 주면, 비싼 호출이 성공하고도 저장하지 못해 버려진다.
	saveReserve = 5 * time.Second

	// ── 단계별 최소 예산 ──
	// 이 값보다 남은 시간이 적으면 그 단계를 **시작하지 않는다**(pipeline.deferStep).
	// 시작해 놓고 중간에 끊으면 공급자 쪽 계산은 그대로 돌아 요금은 나가고 결과는 못 받는다.

	// asrStartNeed 는 업로드 정책 조회 + **오디오 전체 업로드** + 전사 제출이다.
	// 오디오 바이트가 통째로 나가는 유일한 단계라 폴링보다 훨씬 크게 잡는다.
	asrStartNeed = 30 * time.Second
	// asrPollNeed 는 작업 조회 한 번과, 끝났을 때 전사 결과 문서를 내려받는 시간이다.
	asrPollNeed = 15 * time.Second
	// llmCallNeed 는 전사문 하나를 정리하는 시간이다.
	//
	// 🔴 **숫자를 지어내지 마라.** 이 값은 실측에서 나왔다 —
	// internal/callai/live_test.go 의 TestLiveAnalyzeLatency 로 25분 분량(프롬프트 60KB,
	// 15.6k 토큰) 합성 전사문을 qwen3.7-plus 에 넣어 3회 측정: 28.5s / 29.8s / 33.6s.
	// 최악 33.6초에 공급자 지연 편차를 얹어 창을 40초로 보고, 저장 여유를 더해 45초다.
	// 모델·프롬프트·thinking 설정을 바꾸면 **다시 재고 이 값을 고쳐야 한다.**
	llmCallNeed = 40*time.Second + saveReserve

	// minStepBudget 은 작업에 손이라도 대 볼 수 있는 최소 예산이다. 단계별 실제 필요량은
	// pipeline 이 다시 보므로, 여기서는 가장 싼 단계(ASR 폴링)조차 못 하는 경우만 거른다.
	minStepBudget = asrPollNeed
)

// tokenValidator 는 OIDC 검증기다. 테스트가 갈아 끼울 수 있게 함수 타입으로 뽑았다.
type tokenValidator func(ctx context.Context, token, audience string) (*idtoken.Payload, error)

// tickAuth 는 tick 호출자 검증이다.
type tickAuth struct {
	validate tokenValidator
	audience string
	// caller 는 허용할 서비스 계정 이메일이다(CALL_TICK_CALLER).
	caller string
	// shared 는 로컬·테스트용 공유 비밀이다(CALL_TICK_TOKEN). 운영에서는 비워 둔다.
	shared string
}

// ok 는 요청이 Cloud Scheduler 에서 온 것인지 확인한다.
//
// 🔴 실패 이유를 응답에 적지 않는다. 공격자에게 「토큰 모양은 맞는데 aud 가 틀렸다」를
// 알려 줄 이유가 없다. 🔴 토큰 값 자체는 로그에도 남기지 않는다.
func (a *tickAuth) ok(ctx context.Context, r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	token := strings.TrimPrefix(h, "Bearer ")
	if a.shared != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.shared)) == 1 {
		return true
	}
	if a.validate == nil || a.caller == "" || a.audience == "" {
		return false
	}
	// idtoken.Validate 가 ①구글 서명과 ②aud 일치를 본다. ③④는 클레임에서 직접 확인한다.
	p, err := a.validate(ctx, token, a.audience)
	if err != nil || p == nil {
		return false
	}
	email, _ := p.Claims["email"].(string)
	verified, _ := p.Claims["email_verified"].(bool)
	// ④ email_verified 를 빼먹으면 검증되지 않은 이메일 클레임을 그대로 믿게 된다.
	return verified && subtle.ConstantTimeCompare([]byte(email), []byte(a.caller)) == 1
}

type tickResponse struct {
	Claimed           int   `json:"claimed"`
	Advanced          int   `json:"advanced"`
	Failed            int   `json:"failed"`
	RemainingBudgetMS int64 `json:"remaining_budget_ms"`
}

type tickHandler struct {
	auth  *tickAuth
	pipe  *pipeline
	batch int
}

func (t *tickHandler) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !t.auth.ok(r.Context(), r) {
		// 본문을 최소화한다. 🔴 200 은 어떤 경우에도 나가지 않는다.
		httpx.WriteError(w, 401, httpx.CodeUnauthorized, "허용되지 않은 요청이에요")
		return
	}
	now := t.pipe.clock()
	deadline := now.Add(tickBudget)
	// 요청 컨텍스트의 마감이 더 이르면 그쪽을 쓴다. Cloud Run 이 먼저 끊는 상황에서
	// 우리 예산만 믿으면 상태를 저장하지 못한 채 잘린다.
	if d, ok := r.Context().Deadline(); ok && d.Before(deadline) {
		deadline = d.Add(-tailReserve)
	}
	res := t.run(r.Context(), deadline)
	httpx.WriteJSON(w, 200, res)
}

// run 은 예산이 허락하는 만큼 작업을 전진시킨다.
//
// 🔴 응답에 **통화 내용은 한 글자도 넣지 않는다.** 개수와 남은 예산뿐이다.
func (t *tickHandler) run(ctx context.Context, deadline time.Time) tickResponse {
	var res tickResponse
	left := func() time.Duration { return deadline.Sub(t.pipe.clock()) }
	// total 은 이 tick 이 **시작할 때** 가졌던 예산이다. pipeline 이 「앞 통화가 시간을 써서
	// 밀린 것」과 「빈 tick 이어도 안 들어가는 단계」를 가르는 데 쓴다(stepBudget 주석 참고).
	total := left()

	// 🔴 예산을 ctx 에도 건다. 시계 검사(left)는 **단계 사이에서만** 돌기 때문에, 한 번 들어간
	// 공급자 호출이 예산을 넘겨도 그 자체로는 멈추지 않는다. 그대로 두면 Cloud Run 요청
	// 상한(60초)에 먼저 걸려 상태를 저장하지 못한 채 잘린다 — 그 통화는 lease 가 만료될
	// 때까지 멈춰 있는다. 기한을 시각이 아니라 **남은 기간**으로 거는 것은 테스트가 가짜
	// 시계를 쓰기 때문이다(가짜 시각으로 WithDeadline 을 걸면 이미 지난 기한이 된다).
	if total > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, total)
		defer cancel()
	}

	jobs, err := t.pipe.jobs.Claim(ctx, t.pipe.clock(), t.batch)
	if err != nil {
		log.Printf("calls: tick 작업 조회 실패: %v", err)
	}
	res.Claimed = len(jobs)

	for _, j := range jobs {
		if left() < minStepBudget {
			// 예산이 모자라 아직 손도 못 댄 작업은 lease 를 바로 풀어 준다.
			t.release(ctx, j)
			continue
		}
		// lease 시각을 기억해 둔다. 아래 루프가 이 값을 바꾸지 않았다면 「일정이 아직
		// 정해지지 않은 채 예산만 떨어진」 것이므로 lease 를 풀어 다음 tick 에 넘긴다.
		lease := j.NextAttemptAt
		progressed := false
	steps:
		for left() >= minStepBudget {
			r, err := t.pipe.step(ctx, j, stepBudget{left: left(), total: total})
			if err != nil {
				// 저장조차 못 했다. lease 가 만료되면 다음 tick 이 다시 집는다.
				log.Printf("calls: tick 작업 저장 실패 call=%s state=%s: %v", j.CallID, j.State, err)
				break steps
			}
			switch r {
			case stepProgressed:
				progressed = true
			case stepWait:
				progressed = true
				// 폴링 간격만큼 기다릴 여유가 없으면 여기서 멈춘다.
				if left() < pollWait+minStepBudget {
					break steps
				}
				if err := t.pipe.wait(ctx, pollWait); err != nil {
					break steps
				}
			case stepFailed:
				res.Failed++
				break steps
			default: // stepDone
				progressed = true
				break steps
			}
		}
		if progressed {
			res.Advanced++
		}
		if j.NextAttemptAt.Equal(lease) && !terminalStates[j.State] {
			t.release(ctx, j)
		}
	}
	res.RemainingBudgetMS = left().Milliseconds()
	if res.RemainingBudgetMS < 0 {
		res.RemainingBudgetMS = 0
	}
	return res
}

// release 는 lease 를 풀어 **다음 tick 이 바로** 이어받게 한다.
//
// 이것이 없으면 예산이 모자라 멈춘 작업이 lease 만료(2분)까지 놀게 된다. 여기까지 왔다면
// 이 tick 은 그 작업을 더 건드리지 않으므로 지금 푸는 것이 안전하다.
func (t *tickHandler) release(ctx context.Context, j *job) {
	j.NextAttemptAt = t.pipe.clock()
	j.UpdatedAt = j.NextAttemptAt
	if err := t.pipe.jobs.Put(context.WithoutCancel(ctx), j); err != nil {
		log.Printf("calls: tick lease 해제 실패 call=%s: %v", j.CallID, err)
	}
}

// defaultTokenValidator 는 google.golang.org/api/idtoken 으로 구글 서명과 aud 를 확인한다.
func defaultTokenValidator(ctx context.Context, token, audience string) (*idtoken.Payload, error) {
	return idtoken.Validate(ctx, token, audience)
}
