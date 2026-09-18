package calls

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// firestore_pipeline_test.go 는 **에뮬레이터가 있을 때만** 도는 통합 검증이다.
// 상태 기계 자체는 pipeline_test.go 가 메모리 저장소로 확인하고, 여기서는 메모리 구현이
// 흉내 낼 수 없는 것만 본다: 실제 transaction, 실제 쿼리 모양, 조각 문서 삭제.

func emulatorClient(t *testing.T) *firestore.Client {
	t.Helper()
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("Firestore emulator required")
	}
	c, err := firestore.NewClient(context.Background(), "nature-call-tests")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// 🔴 lease 가 실제 transaction 으로 동작하는지. 두 tick 이 같은 작업을 집으면 공급자를
// 두 번 부르고 요금이 그대로 두 배가 된다.
func TestFirestoreJobClaimLease(t *testing.T) {
	client := emulatorClient(t)
	jobs := &fsJobs{client}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	id := fmtID() + "-job"

	j := &job{UID: "owner-a", CallID: id, State: stateQueued, NextAttemptAt: now.Add(-time.Second), CreatedAt: now, RecordedAt: now}
	if err := jobs.Put(ctx, j); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(ctx, id)
	if err != nil || got.UID != "owner-a" || got.State != stateQueued {
		t.Fatalf("roundtrip: %v %+v", err, got)
	}

	first, err := jobs.Claim(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !containsJob(first, id) {
		t.Fatal("큐에 있는 작업을 집지 못했다")
	}
	second, err := jobs.Claim(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if containsJob(second, id) {
		t.Fatal("lease 중인 작업을 두 번 집었다")
	}
	// lease 는 tick 주기(1분)보다 길어야 겹치는 tick 이 같은 작업을 건드리지 않는다.
	if after, _ := jobs.Get(ctx, id); after.NextAttemptAt.Sub(now) < time.Minute {
		t.Fatalf("lease 가 너무 짧다: %v", after.NextAttemptAt.Sub(now))
	}

	// 확정 실패는 스윕 쿼리의 status 목록에 없으므로 다시 잡히지 않는다.
	j.State = stateAnalysisFailed
	j.NextAttemptAt = now.Add(-time.Hour)
	if err := jobs.Put(ctx, j); err != nil {
		t.Fatal(err)
	}
	done, err := jobs.Claim(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if containsJob(done, id) {
		t.Fatal("끝난 작업을 다시 집었다")
	}
}

// 🔴 전사문 shard 는 callJobs/{id}/transcript/{i} 의 data 필드다 — 색인 면제가 컬렉션
// 그룹 단위라 이 이름이어야 기존 terraform 면제가 그대로 적용된다.
// 조각 수가 줄 때 낡은 조각을 지우지 않으면 다음에 늘었을 때 그 바이트가 중간에 끼어 깨진다.
func TestFirestoreJobTranscriptShards(t *testing.T) {
	client := emulatorClient(t)
	jobs := &fsJobs{client}
	ctx := context.Background()
	id := fmtID() + "-shards"

	big := []byte(strings.Repeat("가", shardBytes)) // 3바이트 문자 → 여러 shard
	shards, err := jobs.PutTranscript(ctx, id, big)
	if err != nil || shards < 2 {
		t.Fatalf("shard %d: %v", shards, err)
	}
	got, err := jobs.Transcript(ctx, id, shards)
	if err != nil || string(got) != string(big) {
		t.Fatalf("shard 재조립 실패: %v", err)
	}

	// 조각 수를 줄인다. 낡은 조각이 남아 있으면 안 된다.
	if err := jobs.Put(ctx, &job{CallID: id, UID: "owner-a", TranscriptShards: shards, RecordedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	small := []byte("짧은 원문")
	n, err := jobs.PutTranscript(ctx, id, small)
	if err != nil || n != 1 {
		t.Fatalf("%d: %v", n, err)
	}
	for i := 1; i < shards; i++ {
		d, e := client.Collection(jobCollection).Doc(id).Collection("transcript").Doc(pad(i)).Get(ctx)
		if e == nil && d.Exists() {
			t.Fatalf("낡은 shard %d 가 남아 있다", i)
		}
	}

	// 분석 문서는 callJobs/{id}/analysis/v1 의 data 필드다(같은 이유로 이름 고정).
	if err := jobs.PutAnalysis(ctx, id, []byte(`{"schema_version":1}`)); err != nil {
		t.Fatal(err)
	}
	b, err := jobs.Analysis(ctx, id)
	if err != nil || string(b) != `{"schema_version":1}` {
		t.Fatalf("분석 재조회: %v %s", err, b)
	}
	// 없는 분석은 에러가 아니라 빈 값이다 — 아직 분석 전인 작업이 정상 상태다.
	if b, err = jobs.Analysis(ctx, fmtID()+"-none"); err != nil || b != nil {
		t.Fatalf("%v %s", err, b)
	}
}

// 🔴 덮어쓰기가 낡은 조각을 남기면 조회가 깨진다. 그리고 원문·분석이 없는 통화는
// 500 이 아니라 정상 응답이어야 한다(서버 파이프라인의 진행 중 상태가 그것이다).
func TestFirestoreOverwriteAndLenientGet(t *testing.T) {
	client := emulatorClient(t)
	s := &Store{client}
	ctx := context.Background()
	uid := fmtID() + "-overwrite"

	// 1) 원문·분석 없는 플레이스홀더.
	placeholder := serverFixture()
	p, err := prepareServer(&placeholder, "call-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveOverwrite(ctx, uid, p); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, uid, "call-1")
	if err != nil {
		t.Fatalf("원문·분석 없는 통화가 500 이 된다: %v", err)
	}
	if got.Transcript != nil || got.Analysis != nil || got.Status != "PREPARING" {
		t.Fatalf("%+v", got)
	}

	// 2) 큰 원문 + todo 2개로 덮어쓴다.
	full := serverFixture()
	full.Status = "COMPLETED"
	full.Transcript = &Transcript{Text: strings.Repeat("긴 통화 원문. ", 60000), Segments: []Segment{}}
	full.Analysis = &Analysis{SchemaVersion: 1, Summary: "요약", Details: []Detail{}, Decisions: []string{},
		Todos:      []Todo{{Content: "a", Source: "a"}, {Content: "b", Source: "b"}},
		Consulting: emptyConsultingRec()}
	text := full.Transcript.Text
	if p, err = prepareServer(&full, "call-1"); err != nil {
		t.Fatal(err)
	}
	bigShards := p.shards
	if bigShards < 2 {
		t.Fatalf("shard 가 나뉘지 않았다: %d", bigShards)
	}
	if _, err = s.SaveOverwrite(ctx, uid, p); err != nil {
		t.Fatal(err)
	}
	if got, err = s.Get(ctx, uid, "call-1"); err != nil || got.Transcript.Text != text || len(got.Analysis.Todos) != 2 {
		t.Fatalf("%v %+v", err, got.Analysis)
	}

	// 3) 짧은 원문 + todo 1개로 다시 덮어쓴다 — 낡은 조각이 남으면 안 된다.
	small := serverFixture()
	small.Status = "COMPLETED"
	small.Transcript = &Transcript{Text: "짧은 원문", Segments: []Segment{}}
	small.Analysis = &Analysis{SchemaVersion: 1, Summary: "요약", Details: []Detail{}, Decisions: []string{},
		Todos:      []Todo{{Content: "a", Source: "a"}},
		Consulting: emptyConsultingRec()}
	if p, err = prepareServer(&small, "call-1"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveOverwrite(ctx, uid, p); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(ctx, uid, "call-1")
	if err != nil || got.Transcript.Text != "짧은 원문" || len(got.Analysis.Todos) != 1 {
		t.Fatalf("%v %+v", err, got)
	}
	ref := client.Collection("users").Doc(uid).Collection("calls").Doc("call-1")
	for i := 1; i < bigShards; i++ {
		if d, e := ref.Collection("transcript").Doc(pad(i)).Get(ctx); e == nil && d.Exists() {
			t.Fatalf("낡은 shard %d 가 남아 있다", i)
		}
	}
	if d, e := ref.Collection("todos").Doc(pad(1)).Get(ctx); e == nil && d.Exists() {
		t.Fatal("낡은 todo 가 남아 있다")
	}

	// 4) Touch 는 요약 필드만 갈아 끼우고 원문·분석은 그대로 둔다.
	if err = s.Touch(ctx, uid, "call-1", func(r *Record) { r.Stage = "내용 정리하는 중"; r.JobState = stateAnalyzing }); err != nil {
		t.Fatal(err)
	}
	if got, err = s.Get(ctx, uid, "call-1"); err != nil || got.Stage != "내용 정리하는 중" || got.Transcript == nil {
		t.Fatalf("%v %+v", err, got)
	}
	if err = s.Touch(ctx, uid, "no-such-call", func(*Record) {}); status.Code(err) != codes.NotFound {
		t.Fatalf("없는 통화 Touch: %v", err)
	}
}

// 🔴 하위호환: hasAnalysis 필드는 서버 파이프라인과 함께 생겼다. 그전 문서에는 필드가 없고
// 분석은 항상 있었으므로, 없으면 true 로 봐야 구버전 데이터가 계속 읽힌다.
func TestFirestoreHasAnalysisBackcompat(t *testing.T) {
	client := emulatorClient(t)
	s := &Store{client}
	ctx := context.Background()
	uid := fmtID() + "-backcompat"
	ref := client.Collection("users").Doc(uid).Collection("calls").Doc("call-1")

	r := fixture()
	if err := validate(&r, "call-1"); err != nil {
		t.Fatal(err)
	}
	meta, _ := marshalCompact(listRecord(r))
	analysis, _ := marshalCompact(&Analysis{SchemaVersion: 1, Summary: "옛 요약", Details: []Detail{}, Todos: []Todo{}, Decisions: []string{}, Consulting: emptyConsultingRec()})
	// hasAnalysis 필드가 **없는** 옛 문서를 그대로 만든다.
	if _, err := ref.Set(ctx, map[string]any{
		"record": meta, "digest": "x", "recordedAt": time.Now(), "transcriptShards": int64(0), "todoCount": int64(0),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ref.Collection("analysis").Doc("v1").Set(ctx, map[string]any{"data": analysis}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, uid, "call-1")
	if err != nil {
		t.Fatalf("구버전 문서를 읽지 못한다: %v", err)
	}
	if got.Analysis == nil || got.Analysis.Summary != "옛 요약" {
		t.Fatalf("구버전 분석이 사라졌다: %+v", got.Analysis)
	}
}

func containsJob(js []*job, id string) bool {
	for _, j := range js {
		if j.CallID == id {
			return true
		}
	}
	return false
}

func pad(i int) string { return string([]byte{'0', '0', '0', '0', byte('0' + i)}) }

func emptyConsultingRec() Consulting {
	return Consulting{CustomerNeeds: []string{}, Questions: []string{}, Concerns: []string{}, Objections: []string{}, ImportantPoints: []string{}, Followups: []string{}}
}

// serverFixture 는 서버 파이프라인이 만드는 모양의 레코드다(기기 경로와 달리 원문·분석이 없어도 된다).
func serverFixture() Record {
	p := 0.05
	return Record{
		CallID: "call-1", Contact: Contact{Name: "김고객", Phone: "021234567"},
		Call:      Metadata{FileName: "call.m4a", RecordedAt: "2026-01-01T14:00:00+09:00"},
		CreatedAt: "2026-01-01T14:01:00+09:00",
		Status:    "PREPARING", JobState: stateQueued, Stage: "업로드 완료, 순서 기다리는 중",
		Progress: &p, HasAudio: true,
		AI: &AI{Model: "qwen", ModelVersion: "1", Provider: "alibaba"},
	}
}
