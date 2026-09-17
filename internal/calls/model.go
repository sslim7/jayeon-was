// Package calls stores completed on-device analyses; it never runs speech or language models.
package calls

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sslim7/nature-was/internal/recipients"
)

// 크기 한도. **요청 크기가 아니라 실제로 Firestore 에 쓰이는 바이트**를 기준으로 잡는다.
// 요청만 막으면 인코딩 과정에서 부풀어 오른 저장 바이트가 Commit 한도를 넘겨 500 이 나고,
// 클라이언트는 그것을 일시 오류로 보고 무한 재시도한다.
const (
	// 요청 본문 상한. handler 의 MaxBytesReader 와 같은 값이어야 한다.
	maxRequestBytes = 6 << 20
	// transcript.text 원문 상한(UTF-8 바이트).
	maxTranscriptTextBytes = 4 << 20
	// shard 하나의 크기. Firestore 문서 1 MiB 한도 아래로 넉넉히 잡는다.
	shardBytes = 400000
	// 실제로 저장되는 transcript JSON 상한.
	maxTranscriptStoreBytes = 5 << 20
	// 저장되는 analysis JSON(분리 저장한 todos 포함) 합계 상한.
	maxAnalysisStoreBytes = 400000
	// transcript shard 수 상한.
	maxShards = (maxTranscriptStoreBytes + shardBytes - 1) / shardBytes
	// 한 transaction 이 만드는 문서 수 상한.
	maxTransactionWrites = 500
	// 한 transaction 이 보내는 총 바이트 상한. Firestore Commit 요청 한도 10 MiB 에 여유를 둔다.
	maxCommitBytes = 8 << 20
	// 기기와 서버의 시계 차이 허용치. 기기 시계가 몇 초 빠르다고 그 통화를 영원히
	// 못 올리게 만들지 않는다.
	maxClockSkew = 5 * time.Minute
	// 연락처 이름 길이 상한(rune). internal/sms/history.go 와 같은 기준이다.
	maxNameRunes = 100
)

type Contact struct {
	Name        string  `json:"name"`
	Phone       string  `json:"phone"`
	RecipientID *string `json:"recipient_id,omitempty"`
}
type Metadata struct {
	FileName   string   `json:"file_name"`
	Duration   *float64 `json:"duration"`
	RecordedAt string   `json:"recorded_at"`
}
type Segment struct {
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Text    string  `json:"text"`
	Speaker string  `json:"speaker,omitempty"`
}
type Transcript struct {
	Text     string    `json:"text"`
	Segments []Segment `json:"segments"`
}
type Detail struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}
type Todo struct {
	Content string  `json:"content"`
	Owner   *string `json:"owner"`
	DueDate *string `json:"due_date"`
	Source  string  `json:"source"`
}
type Consulting struct {
	CustomerNeeds   []string `json:"customer_needs"`
	Questions       []string `json:"questions"`
	Concerns        []string `json:"concerns"`
	Objections      []string `json:"objections"`
	ImportantPoints []string `json:"important_points"`
	Followups       []string `json:"followups"`
}
type Analysis struct {
	SchemaVersion int        `json:"schema_version"`
	Summary       string     `json:"summary"`
	Details       []Detail   `json:"details"`
	Todos         []Todo     `json:"todos"`
	Decisions     []string   `json:"decisions"`
	Consulting    Consulting `json:"consulting"`
}
type AI struct {
	Model             string `json:"model"`
	ModelVersion      string `json:"model_version"`
	ProcessedOnDevice bool   `json:"processed_on_device"`
}
type Record struct {
	CallID     string      `json:"call_id"`
	Contact    Contact     `json:"contact"`
	Call       Metadata    `json:"call"`
	CreatedAt  string      `json:"created_at"`
	Status     string      `json:"status"`
	Progress   *float64    `json:"progress"`
	Transcript *Transcript `json:"transcript,omitempty"`
	Analysis   *Analysis   `json:"analysis,omitempty"`
	AI         *AI         `json:"ai,omitempty"`
	Summary    string      `json:"summary,omitempty"`
	Error      *string     `json:"error,omitempty"`
}

var invalid = errors.New("invalid call")

// errTooLarge 는 형식은 맞지만 저장 한도를 넘는 요청이다. 형식 오류(400)와 나눠야
// 클라이언트가 "고쳐서 다시 보낼 수 없는 요청" 을 재시도 대상에서 뺄 수 있다.
var errTooLarge = errors.New("call payload too large")
var phonePattern = regexp.MustCompile(`^\+?[0-9]{7,15}$`)

func validText(s string, max int, required bool) bool {
	return utf8.ValidString(s) && len(s) <= max && (!required || strings.TrimSpace(s) != "")
}

// validRunes 는 rune 수로 길이를 본다. 사용자에게 보이는 글자 수 기준 한도는
// 기존 관례(internal/sms/history.go, internal/recipients)대로 이쪽을 쓴다.
func validRunes(s string, max int, required bool) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) <= max && (!required || strings.TrimSpace(s) != "")
}

// marshalCompact 는 저장·검증 경로가 공유하는 유일한 JSON 인코더다.
//
// 🔴 encoding/json 기본값은 `<`, `>`, `&` 를 `\u003c` 형태로 바꾼다 — 한 글자가 6바이트가 된다.
// 그런 문자가 많은 4 MiB 원문은 24 MiB JSON 이 되어 Firestore Commit 요청 한도(10 MiB)를 넘기고,
// 그 결과는 400 이 아니라 **500** 이다. 클라이언트는 일시 오류로 보고 영원히 재시도한다.
// SetEscapeHTML(false) 로 증폭을 없애고, 남는 증폭(제어문자 `\u0001` 등)은 호출부가
// 실제 바이트 길이로 막는다.
func marshalCompact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	return b[:len(b)-1], nil // Encode 가 붙이는 개행 하나를 뗀다
}
func validate(r *Record, id string) error {
	if !recipients.ValidateID(id) || r.CallID != id || !validRunes(r.Contact.Name, maxNameRunes, true) || !phonePattern.MatchString(r.Contact.Phone) || !validText(r.Call.FileName, 1024, true) {
		return invalid
	}
	if r.Contact.RecipientID != nil && !recipients.ValidateID(*r.Contact.RecipientID) {
		return invalid
	}
	recorded, err := time.Parse(time.RFC3339Nano, r.Call.RecordedAt)
	// 기기 시계가 서버보다 조금 빠른 것은 정상이다. 여유 없이 거부하면 1초 앞선 기기의
	// 통화는 영원히 업로드되지 않는다.
	if err != nil || recorded.After(time.Now().Add(maxClockSkew)) {
		return invalid
	}
	r.Call.RecordedAt = recorded.UTC().Format(time.RFC3339Nano)
	created, err := time.Parse(time.RFC3339Nano, r.CreatedAt)
	if err != nil {
		return invalid
	}
	r.CreatedAt = created.UTC().Format(time.RFC3339Nano)
	if r.Call.Duration != nil && (math.IsNaN(*r.Call.Duration) || math.IsInf(*r.Call.Duration, 0) || *r.Call.Duration < 0 || *r.Call.Duration > 86400) {
		return invalid
	}
	if r.Transcript == nil || r.Analysis == nil || r.AI == nil || !r.AI.ProcessedOnDevice || !validText(r.AI.Model, 200, true) || !validText(r.AI.ModelVersion, 200, true) {
		return invalid
	}
	if !validText(r.Transcript.Text, maxTranscriptTextBytes, true) || r.Transcript.Segments == nil || len(r.Transcript.Segments) > 20000 {
		return invalid
	}
	last := float64(0)
	for _, s := range r.Transcript.Segments {
		if s.Start < last || s.End < s.Start || s.End > 86400 || !validText(s.Text, 64000, true) || !validText(s.Speaker, 100, false) {
			return invalid
		}
		last = s.Start
	}
	a := r.Analysis
	if a.SchemaVersion != 1 || !validText(a.Summary, 32000, true) || a.Details == nil || len(a.Details) > 200 || a.Todos == nil || len(a.Todos) > 100 || a.Decisions == nil {
		return invalid
	}
	for _, d := range a.Details {
		if !validText(d.Title, 1000, true) || !validText(d.Content, 32000, true) {
			return invalid
		}
	}
	for _, t := range a.Todos {
		if !validText(t.Content, 8000, true) || !validText(t.Source, 8000, true) || (t.Owner != nil && !validText(*t.Owner, 400, true)) {
			return invalid
		}
		if t.DueDate != nil {
			if _, e := time.Parse("2006-01-02", *t.DueDate); e != nil {
				return invalid
			}
		}
	}
	for _, list := range [][]string{a.Decisions, a.Consulting.CustomerNeeds, a.Consulting.Questions, a.Consulting.Concerns, a.Consulting.Objections, a.Consulting.ImportantPoints, a.Consulting.Followups} {
		if list == nil || len(list) > 200 {
			return invalid
		}
		for _, s := range list {
			if !validText(s, 16000, true) {
				return invalid
			}
		}
	}
	r.Status = "COMPLETED"
	r.Progress = nil
	r.Error = nil
	r.Summary = ""
	return nil
}
func listRecord(r Record) Record {
	r.Transcript = nil
	if r.Analysis != nil {
		r.Summary = strings.Join(strings.Fields(r.Analysis.Summary), " ")
		runes := []rune(r.Summary)
		if len(runes) > 160 {
			r.Summary = string(runes[:160]) + "…"
		}
	}
	r.Analysis = nil
	return r
}

// payload 는 검증을 통과한 요청을 Firestore 에 그대로 쓸 수 있는 바이트로 바꾼 것이다.
//
// 🔴 **같은 데이터를 두 번 직렬화하지 않는다.** 예전에는 digest 용 전체 marshal, transcript
// marshal, 목록 metadata marshal, 응답 marshal 이 따로 돌아 6 MiB 본문이 동시에 여러 벌
// 메모리에 남았다. Cloud Run 이 memory 512Mi · concurrency 80 이라 동시 업로드 몇 건이면
// 인스턴스가 OOM 으로 죽고, 같은 인스턴스가 처리하던 SMS·로그인 요청까지 함께 끊긴다.
type payload struct {
	summary    Record // 목록/PUT 응답용 요약. 원문·분석을 담지 않는다.
	meta       []byte
	transcript []byte
	analysis   []byte
	todos      [][]byte
	digest     string
	recordedAt time.Time
	callID     string
	shards     int
}

// prepare 는 검증 → 직렬화 → 저장 한도 검사 → digest 를 한 번에 끝낸다.
// 한도를 넘는 요청은 Firestore 에 닿기 전에 errTooLarge 로 잘라 낸다(500 이 아니라 413).
func prepare(r *Record, id string) (*payload, error) {
	if err := validate(r, id); err != nil {
		return nil, err
	}
	recorded, err := time.Parse(time.RFC3339Nano, r.Call.RecordedAt)
	if err != nil {
		return nil, invalid
	}
	p := &payload{summary: listRecord(*r), callID: r.CallID, recordedAt: recorded}
	if p.meta, err = marshalCompact(p.summary); err != nil {
		return nil, invalid
	}
	if p.transcript, err = marshalCompact(r.Transcript); err != nil {
		return nil, invalid
	}
	// 디코딩된 원문은 여기서 버린다. JSON 버퍼와 동시에 들고 있을 이유가 없다.
	r.Transcript = nil
	if len(p.transcript) > maxTranscriptStoreBytes {
		return nil, errTooLarge
	}
	p.shards = (len(p.transcript) + shardBytes - 1) / shardBytes
	if p.shards > maxShards {
		return nil, errTooLarge
	}
	analysisCopy := *r.Analysis
	analysisCopy.Todos = []Todo{}
	if p.analysis, err = marshalCompact(analysisCopy); err != nil {
		return nil, invalid
	}
	analysisTotal := len(p.analysis)
	for _, t := range r.Analysis.Todos {
		b, e := marshalCompact(t)
		if e != nil {
			return nil, invalid
		}
		analysisTotal += len(b)
		p.todos = append(p.todos, b)
	}
	r.Analysis = nil
	if analysisTotal > maxAnalysisStoreBytes {
		return nil, errTooLarge
	}
	// 문서 수와 총 바이트 모두 Commit 한도 아래여야 한다. 둘 중 하나만 봐도 500 이 난다.
	if 2+p.shards+len(p.todos) > maxTransactionWrites {
		return nil, errTooLarge
	}
	if len(p.meta)+len(p.transcript)+analysisTotal > maxCommitBytes {
		return nil, errTooLarge
	}
	// digest 는 이미 만들어 둔 바이트로 계산한다. 길이를 앞에 붙여 조각 경계를 고정한다.
	h := sha256.New()
	for _, b := range append([][]byte{p.meta, p.transcript, p.analysis}, p.todos...) {
		fmt.Fprintf(h, "%d:", len(b))
		h.Write(b)
	}
	p.digest = hex.EncodeToString(h.Sum(nil))
	return p, nil
}
