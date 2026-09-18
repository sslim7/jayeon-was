// Package callai 는 통화 전사(ASR)와 구조화 분석(LLM)을 공급자와 무관한 모양으로 감싼다.
//
// **왜 별도 패키지인가** — internal/calls 는 저장 스키마·상태 기계·크기 한도·권한을 소유하고,
// 여기는 「받아쓰기와 요약을 어디에 맡기는가」만 소유한다. 공급자를 갈아끼울 때 건드릴 파일이
// 이 디렉터리 하나로 끝나야 하고, internal/calls 소스에는 공급자 이름이 한 번도 나오지 않아야 한다.
//
// 공급자를 추가하는 방법: 이 디렉터리에 파일 하나(예: vertex.go)를 더하고 config.go 의
// switch 에 이름 두 줄을 넣는다. 레지스트리나 플러그인 구조는 두지 않는다 — 공급자가
// 두세 개인 동안은 switch 가 더 읽기 쉽다.
package callai

import (
	"context"
	"io"
)

// Audio 는 전사할 오디오 한 건이다.
//
// 🔴 바이트를 담지 않고 **여는 함수**를 담는다. 공급자에 따라 자체 스토리지로 업로드해야 하는데,
// 1시간짜리 통화를 메모리에 통째로 올리면 Cloud Run 인스턴스 하나가 그 요청 때문에 죽는다.
// 재시도할 때마다 새 Reader 가 필요하므로 Reader 자체가 아니라 여는 함수를 받는다.
type Audio struct {
	Open        func(context.Context) (io.ReadCloser, error)
	Size        int64  // 바이트. 0 이면 모름.
	FileName    string // 확장자로 형식을 판별하는 공급자가 있다.
	ContentType string
	// Language 는 BCP-47 에 가까운 힌트다("ko"). 빈 값이면 공급자 기본값.
	Language string
	// SpeakerCount 가 2 이상이면 화자 분리를 요청한다. 0 이면 분리하지 않는다.
	SpeakerCount int
}

// Status 는 전사 작업의 진행 상태다. 실패는 상태가 아니라 error 로 돌려준다 —
// 호출부가 재시도 여부를 Error.Kind 하나로만 판단하게 하기 위해서다.
type Status string

const (
	StatusRunning Status = "RUNNING"
	StatusDone    Status = "DONE"
)

// TranscribeState 는 전사 작업의 스냅샷이다.
//
// 🔴 Token 은 **stateless** 여야 한다. Cloud Run 인스턴스는 tick 사이에 사라지므로,
// 다음 폴링은 전혀 다른 프로세스에서 이 문자열 하나만 들고 이어진다.
type TranscribeState struct {
	Status     Status
	Token      string
	Transcript *Transcript // Status == StatusDone 일 때만 채운다.
	Usage      Usage
	Model      string
	RequestID  string // 공급자 지원 문의에 필요하다. 통화 내용이 아니므로 로그에 남겨도 된다.
}

// Segment 는 화자·시각이 붙은 발화 한 토막이다. 시각 단위는 초다(공급자의 ms 는 여기서 변환한다).
type Segment struct {
	Start   float64
	End     float64
	Text    string
	Speaker string
}

// Transcript 는 공급자 중립 전사 결과다.
type Transcript struct {
	Text     string
	Segments []Segment
	Language string
}

// Detail 은 분석의 소제목 문단이다.
type Detail struct {
	Title   string
	Content string
}

// Todo 는 통화에서 뽑은 할 일이다. Owner/DueDate 는 통화에 없으면 nil 이다 — 지어내지 않는다.
type Todo struct {
	Content string
	Owner   *string
	DueDate *string // YYYY-MM-DD
	Source  string  // 근거가 된 녹취록 문장 그대로
}

// Consulting 은 상담 관점 분류다. 각 항목은 비어 있을 수 있으나 nil 은 아니다.
type Consulting struct {
	CustomerNeeds   []string
	Questions       []string
	Concerns        []string
	Objections      []string
	ImportantPoints []string
	Followups       []string
}

// AnalysisContent 는 앱 `src/types/calls.ts` 의 CallAnalysis 와 같은 모양이다.
// 앱 화면이 이미 이 모양을 그리고 있으므로 필드를 바꾸면 화면이 빈다.
type AnalysisContent struct {
	Summary    string
	Details    []Detail
	Todos      []Todo
	Decisions  []string
	Consulting Consulting
}

// Analysis 는 분석 결과에 과금·추적용 메타를 붙인 것이다.
// 내용과 메타를 한 구조체에 섞지 않으려고 Content 를 따로 뒀다.
type Analysis struct {
	Content       AnalysisContent
	Usage         Usage
	Model         string
	PromptVersion string
	RequestID     string
}

// Usage 는 원가 집계용 사용량이다. 공급자에 사용량 조회 API 가 없어 우리가 직접 모은다.
type Usage struct {
	AudioSeconds     float64 // ASR 과금 단위
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int // thinking 토큰. 0 이 아니면 thinking 이 켜져 있다는 뜻이다.
}

// Add 는 재시도·여러 단계의 사용량을 누적한다.
func (u *Usage) Add(o Usage) {
	u.AudioSeconds += o.AudioSeconds
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.ReasoningTokens += o.ReasoningTokens
}

// Transcriber 는 비동기 공급자와 동기 공급자를 같은 모양으로 흡수한다.
// 동기 공급자는 Start 에서 StatusDone 과 Transcript 를 바로 채워 돌려주면 된다.
type Transcriber interface {
	Start(ctx context.Context, in Audio) (TranscribeState, error)
	Poll(ctx context.Context, token string) (TranscribeState, error)
}

// Analyzer 는 전사문을 구조화 분석으로 바꾼다.
type Analyzer interface {
	Analyze(ctx context.Context, t Transcript) (Analysis, error)
}
