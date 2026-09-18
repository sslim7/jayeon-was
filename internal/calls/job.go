package calls

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// job.go 는 서버 통화분석 파이프라인의 **작업 문서**를 소유한다.
//
// # 왜 최상위 callJobs/{callId} 인가
//
// 기존 통화 레코드는 users/{uid}/calls/{callId} 에 있고, 그 컬렉션의 byte 필드에는
// terraform(`apps/nature/call-analysis.tf`)이 `prevent_destroy` 로 잠가 둔 단일 필드
// 색인 면제가 걸려 있다. 작업 상태를 그 문서에 얹으면 면제 설정을 건드리게 되고,
// 잘못 건드리면 **운영에서만** 400 KB byte 필드 쓰기가 거부된다(에뮬레이터는 색인 한도를
// 강제하지 않아 테스트가 전부 통과한다). 그래서 작업은 완전히 다른 최상위 컬렉션에 둔다.
// uid 는 경로가 아니라 필드다 — tick 이 전체 사용자를 가로질러 한 번에 조회해야 하기 때문이다.
//
// # 🔴 큰 byte 블롭은 작업 문서 본문에 넣지 않는다
//
// Firestore 단일 필드 색인 항목 한도(1500 bytes)에 걸려 쓰기가 거부된다. 전사문과 분석은
// 하위 컬렉션 문서의 `data` 필드에 넣는다. 그리고 그 하위 컬렉션 이름은 반드시
// `transcript` / `analysis` 여야 한다 — **색인 면제는 컬렉션 그룹 단위**이고 컬렉션 그룹
// ID 는 경로의 마지막 세그먼트 이름이라, 기존 면제(`transcript`/`analysis`/`todos` 의 `data`)가
// 이름만 같으면 이 새 경로에도 그대로 적용된다. 이름을 바꾸면 면제가 안 먹고 운영에서만 깨진다.
const (
	// jobCollection 은 작업 문서 컬렉션이다.
	jobCollection = "callJobs"
	// leaseDuration 은 한 tick 이 작업을 물고 있는 시간이다.
	//
	// 🔴 tick 주기(1분)보다 **길어야** 한다. Cloud Run 이 concurrency 80 / max_instances 3 이라
	// 앞 tick 이 1분을 넘기면 다음 tick 과 겹치고, lease 가 주기보다 짧으면 두 tick 이 같은
	// 작업을 동시에 붙잡아 공급자를 두 번 부른다(= 요금이 두 배). Cloud Run 요청 상한이
	// 60초라 한 tick 은 최대 1분이므로 2분이면 겹치지 않는다.
	leaseDuration = 2 * time.Minute
	// 🔴 **시도 상한을 단계별로 나눈 이유는 돈이다.**
	//
	// 공급자는 task_status=FAILED 에 모르는 코드가 오면 KindRetryable 로 돌려준다
	// (확정 실패로 통화를 버리느니 상한에 걸려 멈추는 쪽이 안전하다는 판단이다).
	// 그 말은 **이 상한이 곧 우리가 지불하는 금액의 상한**이라는 뜻이다.
	//
	// ASR 은 과금 단위가 **오디오 초**다. 재시도할 때마다 오디오를 공급자 스토리지에 다시
	// 올리고 전사를 처음부터 다시 돌린다 — 1시간 통화를 5번 재시도하면 5시간치 요금이 나간다.
	// 분석은 이미 저장된 전사문을 재사용하므로 훨씬 싸다. 그래서 한 상수로 뭉뚱그리지 않는다.
	maxASRAttempts      = 3
	maxAnalysisAttempts = 5
	// maxBackoff 는 재시도 간격 상한이다.
	maxBackoff = 30 * time.Minute
)

// 작업 상태. 🔴 이 값을 그대로 API 로 내보내지 마라 — appStatus 로 사상해야 한다.
const (
	stateAwaitingUpload      = "AWAITING_UPLOAD"
	stateQueued              = "QUEUED"
	stateASRRunning          = "ASR_RUNNING"
	stateASRPolling          = "ASR_POLLING"
	stateTranscribed         = "TRANSCRIBED"
	stateAnalyzing           = "ANALYZING"
	stateCompleted           = "COMPLETED"
	stateTranscriptionFailed = "TRANSCRIPTION_FAILED"
	stateAnalysisFailed      = "ANALYSIS_FAILED"
)

// terminalStates 는 tick 이 더 이상 건드리지 않는 상태다.
var terminalStates = map[string]bool{
	stateAwaitingUpload: true, stateCompleted: true,
	stateTranscriptionFailed: true, stateAnalysisFailed: true,
}

// stageOf 는 사용자에게 그대로 보여 줄 한국어 단계명이다.
func stageOf(state string) string {
	switch state {
	case stateAwaitingUpload:
		return "녹음 파일 준비 중"
	case stateQueued:
		return "업로드 완료, 순서 기다리는 중"
	case stateASRRunning, stateASRPolling:
		return "받아쓰는 중"
	case stateTranscribed, stateAnalyzing:
		return "내용 정리하는 중"
	case stateCompleted:
		return "분석 완료"
	case stateTranscriptionFailed:
		return "받아쓰기에 실패했어요"
	case stateAnalysisFailed:
		return "내용 정리에 실패했어요"
	}
	return ""
}

// progressOf 는 앱 진행 막대에 쓸 0~1 값이다. 단계가 실제로 얼마나 걸리는지와 무관하게
// 「멈춰 있지 않다」는 것만 보여 주면 되므로 단계별 고정값이다.
func progressOf(state string) float64 {
	switch state {
	case stateQueued:
		return 0.05
	case stateASRRunning:
		return 0.15
	case stateASRPolling:
		return 0.4
	case stateTranscribed:
		return 0.7
	case stateAnalyzing:
		return 0.8
	case stateCompleted:
		return 1
	case stateTranscriptionFailed:
		// 실패 상태의 진행률은 **거기까지 갔다**는 뜻으로 둔다. 0 으로 되돌리면 앱 진행
		// 막대가 처음으로 튀어 「아무것도 안 했다」처럼 보인다.
		return progressOf(stateASRPolling)
	case stateAnalysisFailed:
		return progressOf(stateAnalyzing)
	}
	return 0
}

// audioRef 는 GCS 에 올라간 원본 오디오 한 건이다. 바이트는 서버를 통과하지 않는다.
type audioRef struct {
	Bucket      string `firestore:"bucket" json:"bucket"`
	Object      string `firestore:"object" json:"object"`
	Size        int64  `firestore:"size" json:"size"`
	ContentType string `firestore:"contentType" json:"content_type"`
	FileName    string `firestore:"fileName" json:"file_name"`
}

// jobUsage 는 원가 집계용 누적 사용량이다.
type jobUsage struct {
	AudioSeconds     float64 `firestore:"audioSeconds"`
	PromptTokens     int     `firestore:"promptTokens"`
	CompletionTokens int     `firestore:"completionTokens"`
	ReasoningTokens  int     `firestore:"reasoningTokens"`
}

// job 은 callJobs/{callId} 문서다. **전부 작은 값만 담는다**(위 주석 참고).
type job struct {
	UID    string `firestore:"uid"`
	CallID string `firestore:"callId"`
	// State 는 **내부 작업 상태**다(QUEUED/ASR_RUNNING/...).
	//
	// 🔴 Firestore 필드 이름이 `status` 인 것은 복합 인덱스가 그 이름으로 만들어져 있기
	// 때문이다. **API 응답의 status 와 같은 단어지만 값 집합이 완전히 다르다** —
	// 앱에 내보낼 때는 반드시 appStatus() 로 사상해야 한다. Go 쪽 이름을 State 로 둔 것은
	// 코드에서 그 둘을 눈으로 구분하기 위해서다.
	State    string   `firestore:"status"`
	Stage    string   `firestore:"stage"`
	Progress float64  `firestore:"progress"`
	Audio    audioRef `firestore:"audio"`

	ContactName        string `firestore:"contactName"`
	ContactPhone       string `firestore:"contactPhone"`
	ContactRecipientID string `firestore:"contactRecipientId"`

	FileName   string    `firestore:"fileName"`
	Duration   float64   `firestore:"duration"`
	RecordedAt time.Time `firestore:"recordedAt"`

	CreatedAt time.Time `firestore:"createdAt"`
	UpdatedAt time.Time `firestore:"updatedAt"`

	// NextAttemptAt 은 **스케줄링과 lease 를 겸하는 단 하나의 필드**다.
	//
	// 🔴 인프라(redhead-terraform)가 `callJobs(status ASC, nextAttemptAt ASC)` 복합 인덱스를
	// 이 **이름 그대로** 만들어 뒀다. Go 필드 이름을 바꾸는 것은 자유지만 firestore 태그를
	// 바꾸면 인덱스가 안 먹고, 증상은 **운영에서만 나는 FAILED_PRECONDITION** 이다
	// (에뮬레이터는 인덱스를 요구하지 않아 테스트가 전부 통과한다).
	//
	//   - 큐잉/재시도 예약: NextAttemptAt = now + backoff
	//   - lease 획득: transaction 안에서 아직 <= now 인지 확인하고 now+leaseDuration 으로 민다
	//
	// 종료 상태에서는 이 값을 건드리지 않는다. 스윕 쿼리가 status 로 먼저 거르므로
	// 끝난 작업은 애초에 후보에 들어오지 않는다 — 예전에 쓰려던 「필드를 지워 쿼리에서
	// 빠지게 하는」 트릭은 필요 없어졌다. 오히려 필드를 지우면 OrderBy(nextAttemptAt) 쿼리가
	// 그 문서를 통째로 무시하게 되어 디버깅할 때 보이지 않는다.
	NextAttemptAt time.Time `firestore:"nextAttemptAt"`

	ASRAttempt      int    `firestore:"asrAttempt"`
	AnalysisAttempt int    `firestore:"analysisAttempt"`
	ASRToken        string `firestore:"asrToken"`
	ASRRequestID    string `firestore:"asrRequestId"`
	LLMRequestID    string `firestore:"llmRequestId"`

	ASRProvider   string `firestore:"asrProvider"`
	ASRModel      string `firestore:"asrModel"`
	LLMProvider   string `firestore:"llmProvider"`
	LLMModel      string `firestore:"llmModel"`
	PromptVersion string `firestore:"promptVersion"`

	Usage jobUsage `firestore:"usage"`

	ErrorCode string    `firestore:"errorCode"`
	ErrorKind string    `firestore:"errorKind"`
	ErrorAt   time.Time `firestore:"errorAt"`

	TranscriptShards int  `firestore:"transcriptShards"`
	HasAnalysis      bool `firestore:"hasAnalysis"`
}

// fields 는 작업 문서 본문이다. 전체 Set 으로 쓴다.
//
// 🔴 **여기에 큰 byte 블롭을 절대 넣지 마라.** callJobs 본문 문서에는 색인 면제가 없어
// Firestore 단일 필드 색인 항목 한도(1500 bytes)에 걸려 쓰기가 거부된다. 전사문·분석은
// 하위 컬렉션 transcript/analysis 의 data 필드에 넣는다(파일 상단 주석 참고).
//
// nextAttemptAt 은 종료 상태에서도 그대로 쓴다. 스윕 쿼리가 status 로 먼저 거르기 때문에
// 끝난 작업은 후보에 들어오지 않고, 필드를 남겨 두면 나중에 문서를 눈으로 볼 때 마지막
// 스케줄 시각이 보인다.
func (j *job) fields() map[string]any {
	m := map[string]any{
		// 🔴 키 이름은 `status` 다(State 필드 주석 참고). 복합 인덱스가 그 이름으로 만들어져
		// 있어 여기서 `state` 로 쓰면 스윕 쿼리가 **아무것도 못 찾는다** — 그런데 에러는 나지
		// 않고 큐가 조용히 멈춘다.
		"uid": j.UID, "callId": j.CallID, "status": j.State, "stage": j.Stage, "progress": j.Progress,
		"audio":       map[string]any{"bucket": j.Audio.Bucket, "object": j.Audio.Object, "size": j.Audio.Size, "contentType": j.Audio.ContentType, "fileName": j.Audio.FileName},
		"contactName": j.ContactName, "contactPhone": j.ContactPhone, "contactRecipientId": j.ContactRecipientID,
		"fileName": j.FileName, "duration": j.Duration, "recordedAt": j.RecordedAt,
		"createdAt": j.CreatedAt, "updatedAt": j.UpdatedAt,
		"asrAttempt": j.ASRAttempt, "analysisAttempt": j.AnalysisAttempt,
		"asrToken": j.ASRToken, "asrRequestId": j.ASRRequestID, "llmRequestId": j.LLMRequestID,
		"asrProvider": j.ASRProvider, "asrModel": j.ASRModel, "llmProvider": j.LLMProvider,
		"llmModel": j.LLMModel, "promptVersion": j.PromptVersion,
		"usage":     map[string]any{"audioSeconds": j.Usage.AudioSeconds, "promptTokens": j.Usage.PromptTokens, "completionTokens": j.Usage.CompletionTokens, "reasoningTokens": j.Usage.ReasoningTokens},
		"errorCode": j.ErrorCode, "errorKind": j.ErrorKind, "nextAttemptAt": j.NextAttemptAt,
		"transcriptShards": j.TranscriptShards, "hasAnalysis": j.HasAnalysis,
	}
	if !j.ErrorAt.IsZero() {
		m["errorAt"] = j.ErrorAt
	}
	return m
}

// contact 는 작업 문서의 연락처를 통화 레코드 모양으로 되돌린다.
func (j *job) contact() Contact {
	c := Contact{Name: j.ContactName, Phone: j.ContactPhone}
	if j.ContactRecipientID != "" {
		id := j.ContactRecipientID
		c.RecipientID = &id
	}
	return c
}

// metadata 는 작업 문서의 통화 메타를 레코드 모양으로 되돌린다.
func (j *job) metadata() Metadata {
	m := Metadata{FileName: j.FileName, RecordedAt: j.RecordedAt.UTC().Format(time.RFC3339Nano)}
	if j.Duration > 0 {
		d := j.Duration
		m.Duration = &d
	}
	return m
}

// summaryRecord 는 작업 상태를 통화 레코드의 요약 필드로 옮긴다.
// transcript/analysis 는 호출부가 채운다.
func (j *job) summaryRecord() Record {
	r := Record{
		CallID: j.CallID, Contact: j.contact(), Call: j.metadata(),
		CreatedAt: j.CreatedAt.UTC().Format(time.RFC3339Nano),
		Status:    appStatus(j.State), JobState: j.State, Stage: j.Stage,
		HasAudio: j.Audio.Object != "",
	}
	p := progressOf(j.State)
	r.Progress = &p
	if j.ErrorCode != "" && (j.State == stateTranscriptionFailed || j.State == stateAnalysisFailed) {
		code := j.ErrorCode
		r.Error = &code
	}
	if j.ASRModel != "" || j.LLMModel != "" {
		r.AI = &AI{Model: firstNonEmpty(j.LLMModel, j.ASRModel), ModelVersion: firstNonEmpty(j.PromptVersion, "1"), Provider: firstNonEmpty(j.LLMProvider, j.ASRProvider)}
	}
	return r
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// backoff 는 attempt(1부터) 번째 실패 뒤 다음 시도까지의 간격이다. 1m, 2m, 4m, 8m, 16m, 상한 30m.
func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Minute
	for i := 1; i < attempt && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// jobRepo 는 파이프라인이 작업 문서에 대해 필요로 하는 것 전부다.
//
// 인터페이스로 뽑은 이유는 **상태 기계를 에뮬레이터 없이 돌리기 위해서**다. 상태 전이와
// 재시도 상한은 Firestore 와 아무 상관이 없는 로직인데, 그것을 확인하려고 매번 에뮬레이터를
// 띄우면 테스트가 개발 머신 설정에 묶인다.
type jobRepo interface {
	// Get 은 작업을 읽는다. 없으면 codes.NotFound 에러다.
	Get(ctx context.Context, callID string) (*job, error)
	// Put 은 작업을 통째로 쓴다(생성 포함).
	Put(ctx context.Context, j *job) error
	// Claim 은 처리할 차례가 된 작업을 최대 limit 개 집어 lease 를 건다.
	// 집힌 작업의 NextAttemptAt 은 now+leaseDuration 으로 밀려 있다.
	Claim(ctx context.Context, now time.Time, limit int) ([]*job, error)
	// PutTranscript 는 전사문 JSON 을 shard 로 나눠 쓰고 남는 낡은 shard 를 지운다.
	PutTranscript(ctx context.Context, callID string, data []byte) (shards int, err error)
	// Transcript 는 shard 를 이어 붙여 돌려준다. shard 가 없으면 nil.
	Transcript(ctx context.Context, callID string, shards int) ([]byte, error)
	// PutAnalysis / Analysis 는 분석 JSON 한 벌을 다룬다.
	PutAnalysis(ctx context.Context, callID string, data []byte) error
	Analysis(ctx context.Context, callID string) ([]byte, error)
}

// fsJobs 는 jobRepo 의 Firestore 구현이다.
type fsJobs struct{ FS *firestore.Client }

func (s *fsJobs) doc(callID string) *firestore.DocumentRef {
	return s.FS.Collection(jobCollection).Doc(callID)
}

func (s *fsJobs) Get(ctx context.Context, callID string) (*job, error) {
	d, err := s.doc(callID).Get(ctx)
	if err != nil {
		return nil, err
	}
	return decodeJob(d)
}

func decodeJob(d *firestore.DocumentSnapshot) (*job, error) {
	var j job
	if err := d.DataTo(&j); err != nil {
		return nil, err
	}
	if j.CallID == "" {
		j.CallID = d.Ref.ID
	}
	return &j, nil
}

func (s *fsJobs) Put(ctx context.Context, j *job) error {
	_, err := s.doc(j.CallID).Set(ctx, j.fields())
	return err
}

// sweepStates 는 스윕 쿼리가 후보로 삼는 내부 상태다.
//
// 🔴 인프라가 만든 복합 인덱스 `callJobs(status ASC, nextAttemptAt ASC)` 모양에 정확히
// 맞춘 쿼리를 쓴다. **`Where("nextAttemptAt","<=",now)` 를 붙이면 안 된다** — 부등호가
// 붙는 순간 인덱스 모양에서 벗어나 운영에서 FAILED_PRECONDITION 이 난다.
// 대신 오름차순 정렬을 이용해 **코드에서** 미래 항목을 끊는다(첫 번째가 미래면 뒤는 전부 미래다).
var sweepStates = []string{stateQueued, stateASRRunning, stateASRPolling, stateTranscribed, stateAnalyzing}

// Claim 은 후보를 쿼리로 고르고 **작업마다 transaction 으로** lease 를 건다.
//
// 🔴 쿼리 결과를 그대로 믿으면 안 된다. 쿼리와 쓰기 사이에 다른 tick 이 같은 작업을
// 집어 갈 수 있으므로, transaction 안에서 NextAttemptAt 이 여전히 <= now 인지 다시 확인한다.
// 확인에 실패한 후보는 조용히 건너뛴다 — 그 tick 이 가져간 것이 맞다.
func (s *fsJobs) Claim(ctx context.Context, now time.Time, limit int) ([]*job, error) {
	q := s.FS.Collection(jobCollection).Where("status", "in", sweepStates).OrderBy("nextAttemptAt", firestore.Asc).Limit(limit)
	iter := q.Documents(ctx)
	defer iter.Stop()
	var ids []string
	for {
		d, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		// 정렬이 오름차순이라 첫 미래 항목에서 끊으면 나머지도 전부 미래다.
		if due, ok := d.Data()["nextAttemptAt"].(time.Time); !ok || due.After(now) {
			break
		}
		ids = append(ids, d.Ref.ID)
	}
	out := make([]*job, 0, len(ids))
	for _, id := range ids {
		ref := s.doc(id)
		var claimed *job
		err := s.FS.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
			claimed = nil
			d, e := tx.Get(ref)
			if e != nil {
				if status.Code(e) == codes.NotFound {
					return nil
				}
				return e
			}
			j, e := decodeJob(d)
			if e != nil {
				// 디코딩할 수 없는 작업 문서 하나가 큐 전체를 막지 않게 건너뛴다.
				// 🔴 무엇이 들어 있었는지는 로그로도 남기지 않는다.
				return nil
			}
			if j.NextAttemptAt.After(now) || terminalStates[j.State] {
				return nil
			}
			j.NextAttemptAt = now.Add(leaseDuration)
			j.UpdatedAt = now
			if e = tx.Set(ref, j.fields()); e != nil {
				return e
			}
			claimed = j
			return nil
		})
		if err != nil {
			return out, err
		}
		if claimed != nil {
			out = append(out, claimed)
		}
	}
	return out, nil
}

func (s *fsJobs) PutTranscript(ctx context.Context, callID string, data []byte) (int, error) {
	ref := s.doc(callID)
	shards := (len(data) + shardBytes - 1) / shardBytes
	if shards > maxShards {
		return 0, errTooLarge
	}
	// 낡은 shard 를 먼저 세어 둔다. 개수가 줄었을 때 남는 조각이 있으면 다음 읽기가 깨진다.
	old := 0
	if d, err := ref.Get(ctx); err == nil {
		if j, e := decodeJob(d); e == nil {
			old = j.TranscriptShards
		}
	}
	bulk := s.FS.BulkWriter(ctx)
	for i := 0; i < shards; i++ {
		end := (i + 1) * shardBytes
		if end > len(data) {
			end = len(data)
		}
		if _, err := bulk.Set(ref.Collection("transcript").Doc(fmt.Sprintf("%05d", i)), map[string]any{"data": data[i*shardBytes : end]}); err != nil {
			return 0, err
		}
	}
	for i := shards; i < old && i < maxShards; i++ {
		if _, err := bulk.Delete(ref.Collection("transcript").Doc(fmt.Sprintf("%05d", i))); err != nil {
			return 0, err
		}
	}
	bulk.End()
	return shards, nil
}

func (s *fsJobs) Transcript(ctx context.Context, callID string, shards int) ([]byte, error) {
	if shards <= 0 {
		return nil, nil
	}
	if shards > maxShards {
		shards = maxShards
	}
	ref := s.doc(callID)
	refs := make([]*firestore.DocumentRef, 0, shards)
	for i := 0; i < shards; i++ {
		refs = append(refs, ref.Collection("transcript").Doc(fmt.Sprintf("%05d", i)))
	}
	docs, err := s.FS.GetAll(ctx, refs)
	if err != nil {
		return nil, err
	}
	var out []byte
	for _, d := range docs {
		if !d.Exists() {
			return nil, nil
		}
		b, ok := d.Data()["data"].([]byte)
		if !ok {
			return nil, errStored
		}
		out = append(out, b...)
	}
	return out, nil
}

func (s *fsJobs) PutAnalysis(ctx context.Context, callID string, data []byte) error {
	_, err := s.doc(callID).Collection("analysis").Doc("v1").Set(ctx, map[string]any{"data": data})
	return err
}

func (s *fsJobs) Analysis(ctx context.Context, callID string) ([]byte, error) {
	d, err := s.doc(callID).Collection("analysis").Doc("v1").Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, err
	}
	b, ok := d.Data()["data"].([]byte)
	if !ok {
		return nil, errStored
	}
	return b, nil
}

// decodeTranscript 는 shard 바이트를 통화 레코드 원문으로 되돌린다.
func decodeTranscript(b []byte) (*Transcript, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var t Transcript
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	if t.Segments == nil {
		t.Segments = []Segment{}
	}
	return &t, nil
}
