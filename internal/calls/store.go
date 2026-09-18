package calls

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/nature-was/internal/recipients"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var ErrConflict = errors.New("call already exists with different content")
var ErrCursor = errors.New("invalid cursor")

// errStored 는 저장된 문서가 기대한 모양이 아닐 때다. 클라이언트가 고칠 수 있는 게
// 없으므로 500 으로 나간다 — 예전처럼 panic 으로 인스턴스를 죽이지 않는다.
var errStored = errors.New("invalid stored call")

type Page struct {
	Items      []Record `json:"items"`
	NextCursor string   `json:"nextCursor,omitempty"`
}
type repository interface {
	Save(context.Context, string, *payload) (Record, error)
	Get(context.Context, string, string) (Record, error)
	List(ctx context.Context, uid, q string, limit int, cursor string) (Page, error)
}
type Store struct{ FS *firestore.Client }

func (s *Store) collection(uid string) *firestore.CollectionRef {
	return s.FS.Collection("users").Doc(uid).Collection("calls")
}

// A single transaction publishes metadata and all separate entities. No partial result is visible.
// Transcript byte shards stay below Firestore's per-document size limit; every size cap is already
// enforced by prepare, so this function only writes bytes it was handed.
//
// 🔴 응답으로 돌려주는 것은 **요약 레코드뿐**이다. 방금 받은 6 MiB 본문을 그대로 되돌려
// 주면 응답 직렬화에 같은 크기가 한 벌 더 필요해 OOM 위험이 커진다. 앱은 방금 보낸 원본을
// 이미 갖고 있으므로 돌려받을 이유가 없다.
func (s *Store) Save(ctx context.Context, uid string, p *payload) (Record, error) {
	return s.save(ctx, uid, p, false)
}

// SaveOverwrite 는 같은 통화를 **덮어쓴다**. 서버 파이프라인 전용이다.
//
// 🔴 `Save(..., overwrite bool)` 로 시그니처를 바꾸지 않고 메서드를 하나 더 둔 이유:
// handler 가 쓰는 repository 인터페이스의 Save 는 기기 경로 계약(같은 digest 면 no-op,
// 다르면 409)을 그대로 뜻해야 한다. 그 인터페이스를 바꾸면 기존 테스트의 가짜 저장소까지
// 함께 바뀌어 「기기 경로 동작이 그대로인지」를 확인할 근거가 사라진다.
// 덮어쓰기는 파이프라인만 쓰므로 파이프라인이 보는 인터페이스(recordStore)에만 더한다.
func (s *Store) SaveOverwrite(ctx context.Context, uid string, p *payload) (Record, error) {
	return s.save(ctx, uid, p, true)
}

func (s *Store) save(ctx context.Context, uid string, p *payload, overwrite bool) (Record, error) {
	ref := s.collection(uid).Doc(p.callID)
	err := s.FS.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		staleShards, staleTodos, hadAnalysis := 0, 0, false
		existing, e := tx.Get(ref)
		if e == nil {
			if !overwrite {
				if existing.Data()["digest"] == p.digest {
					return nil
				}
				return ErrConflict
			}
			// 🔴 조각 수가 **줄어들 때** 낡은 조각을 지우지 않으면 Get 이 읽지 않는 문서가
			// 남고, 다음에 조각 수가 다시 늘면 그 낡은 바이트가 중간에 끼어 JSON 이 깨진다.
			staleShards = clampCount(existing.Data()["transcriptShards"], maxShards)
			staleTodos = clampCount(existing.Data()["todoCount"], maxTransactionWrites)
			hadAnalysis = storedHasAnalysis(existing)
		} else if status.Code(e) != codes.NotFound {
			return e
		}
		var dels []*firestore.DocumentRef
		for i := p.shards; i < staleShards; i++ {
			dels = append(dels, ref.Collection("transcript").Doc(fmt.Sprintf("%05d", i)))
		}
		for i := len(p.todos); i < staleTodos; i++ {
			dels = append(dels, ref.Collection("todos").Doc(fmt.Sprintf("%05d", i)))
		}
		if p.analysis == nil && hadAnalysis {
			dels = append(dels, ref.Collection("analysis").Doc("v1"))
		}
		// 삭제도 transaction 쓰기 한도를 먹는다. 한도를 넘으면 Commit 이 500 으로 터진다.
		if 2+p.shards+len(p.todos)+len(dels) > maxTransactionWrites {
			return errTooLarge
		}
		if e = tx.Set(ref, map[string]any{"record": p.meta, "digest": p.digest, "recordedAt": p.recordedAt, "transcriptShards": p.shards, "todoCount": len(p.todos), "hasAnalysis": p.analysis != nil, "syncedAt": firestore.ServerTimestamp}); e != nil {
			return e
		}
		for i := 0; i < p.shards; i++ {
			end := (i + 1) * shardBytes
			if end > len(p.transcript) {
				end = len(p.transcript)
			}
			if e = tx.Set(ref.Collection("transcript").Doc(fmt.Sprintf("%05d", i)), map[string]any{"data": p.transcript[i*shardBytes : end]}); e != nil {
				return e
			}
		}
		if p.analysis != nil {
			if e = tx.Set(ref.Collection("analysis").Doc("v1"), map[string]any{"data": p.analysis}); e != nil {
				return e
			}
		}
		for i, b := range p.todos {
			if e = tx.Set(ref.Collection("todos").Doc(fmt.Sprintf("%05d", i)), map[string]any{"data": b, "completed": false}); e != nil {
				return e
			}
		}
		for _, d := range dels {
			if e = tx.Delete(d); e != nil {
				return e
			}
		}
		return nil
	})
	return p.summary, err
}

// Touch 는 저장된 **요약 레코드만** 제자리에서 고친다.
//
// 파이프라인이 단계마다 상태를 옮길 때 전사문·분석을 통째로 다시 쓰는 것은 낭비이고,
// 아직 전사문이 없는 단계에서는 쓸 것 자체가 없다. record 필드 하나만 갈아 끼운다.
func (s *Store) Touch(ctx context.Context, uid, id string, mut func(*Record)) error {
	ref := s.collection(uid).Doc(id)
	return s.FS.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		doc, e := tx.Get(ref)
		if e != nil {
			return e
		}
		r, e := decodeRecord(doc)
		if e != nil {
			return e
		}
		mut(&r)
		// 저장된 record 는 이미 요약이라 원문·분석이 들어 있지 않다. 그래도 방어적으로 비운다.
		r.Transcript, r.Analysis = nil, nil
		b, e := marshalCompact(r)
		if e != nil {
			return e
		}
		return tx.Update(ref, []firestore.Update{{Path: "record", Value: b}})
	})
}

// clampCount 는 저장된 개수 필드를 안전한 범위로 자른다. 손상된 문서가 담은 값으로
// 루프를 돌면 transaction 한도를 넘겨 Commit 이 터진다.
func clampCount(v any, max int) int {
	n, ok := v.(int64)
	if !ok || n <= 0 {
		return 0
	}
	if int(n) > max {
		return max
	}
	return int(n)
}

// storedHasAnalysis 는 분석 문서 존재 여부를 읽는다.
//
// 🔴 **하위호환**: 이 필드는 서버 파이프라인과 함께 생겼다. 그전에 저장된 문서에는
// 필드 자체가 없고 분석은 항상 있었다 — 없으면 true 로 봐야 구버전 데이터가 계속 읽힌다.
func storedHasAnalysis(d *firestore.DocumentSnapshot) bool {
	v, ok := d.Data()["hasAnalysis"].(bool)
	return !ok || v
}
func decodeRecord(d *firestore.DocumentSnapshot) (Record, error) {
	var r Record
	b, ok := d.Data()["record"].([]byte)
	if !ok {
		return r, errStored
	}
	return r, json.Unmarshal(b, &r)
}
func (s *Store) Get(ctx context.Context, uid, id string) (Record, error) {
	ref := s.collection(uid).Doc(id)
	doc, e := ref.Get(ctx)
	if e != nil {
		return Record{}, e
	}
	r, e := decodeRecord(doc)
	if e != nil {
		return r, e
	}
	// 검사 없는 타입 단언은 문서가 조금만 어긋나도 panic 이고, panic 은 이 요청 하나가
	// 아니라 인스턴스 전체를 끊는다. nil·타입 불일치·말이 안 되는 개수는 500 으로 돌린다.
	shards, okShards := doc.Data()["transcriptShards"].(int64)
	todoCount, okTodos := doc.Data()["todoCount"].(int64)
	if !okShards || !okTodos || shards < 0 || shards > maxShards || todoCount < 0 || todoCount > maxTransactionWrites {
		return r, errStored
	}
	// 🔴 **원문도 분석도 아직 없는 통화가 정상 상태다.** 서버 파이프라인은 전사가 끝나기
	// 전부터 진행 상태를 보여 주는 플레이스홀더 레코드를 저장한다. 예전처럼 analysis/v1
	// 이 없다고 500 을 내면 앱의 상세 화면이 「서버 오류」로 뜬다.
	hasAnalysis := storedHasAnalysis(doc)
	count, todos := int(shards), int(todoCount)
	var refs []*firestore.DocumentRef
	if hasAnalysis {
		refs = append(refs, ref.Collection("analysis").Doc("v1"))
	}
	analysisDocs := len(refs)
	for i := 0; i < count; i++ {
		refs = append(refs, ref.Collection("transcript").Doc(fmt.Sprintf("%05d", i)))
	}
	for i := 0; i < todos; i++ {
		refs = append(refs, ref.Collection("todos").Doc(fmt.Sprintf("%05d", i)))
	}
	if len(refs) == 0 {
		return r, nil
	}
	docs, e := s.FS.GetAll(ctx, refs)
	if e != nil {
		return r, e
	}
	data := func(d *firestore.DocumentSnapshot) ([]byte, error) {
		b, ok := d.Data()["data"].([]byte)
		if !ok {
			return nil, errStored
		}
		return b, nil
	}
	if analysisDocs == 1 {
		// hasAnalysis 가 true 인데 문서가 없는 경우는 하위호환 기본값(필드 없음 → true)이
		// 빗나간 것뿐이다. 500 대신 분석 없음으로 본다.
		if a := docs[0]; a.Exists() {
			b, e := data(a)
			if e != nil {
				return r, e
			}
			r.Analysis = &Analysis{}
			if e = json.Unmarshal(b, r.Analysis); e != nil {
				return r, e
			}
		}
	}
	if count > 0 {
		var transcript []byte
		for _, d := range docs[analysisDocs : analysisDocs+count] {
			b, e := data(d)
			if e != nil {
				return r, e
			}
			transcript = append(transcript, b...)
		}
		r.Transcript = &Transcript{}
		if e = json.Unmarshal(transcript, r.Transcript); e != nil {
			return r, e
		}
	}
	for _, d := range docs[analysisDocs+count:] {
		b, e := data(d)
		if e != nil {
			return r, e
		}
		var t Todo
		if e = json.Unmarshal(b, &t); e != nil {
			return r, e
		}
		if r.Analysis == nil {
			continue // 분석 없이 남은 todo 는 보여 줄 자리가 없다. 조용히 건너뛴다.
		}
		r.Analysis.Todos = append(r.Analysis.Todos, t)
	}
	return r, nil
}

const (
	// defaultPageSize 는 limit 을 안 보냈을 때의 페이지 크기다(예전의 고정값과 같다).
	defaultPageSize = 30
	// maxPageSize 는 클라이언트가 요구할 수 있는 상한이다. 앱이 무한 스크롤 한 화면을
	// 30+30 두 번에 나눠 부르던 것을 한 번에 채우게 하되, 한 요청이 읽는 문서 수는
	// 서버가 쥐고 있어야 한다(recipients·sms 목록과 같은 1~100 관례).
	maxPageSize = 100
	// maxQueryRunes 는 검색어 길이 상한이다. internal/sms/history.go 와 같은 기준이다.
	maxQueryRunes = 100
	// maxSearchScan 은 **검색 한 번이 훑는 문서 수 상한**이다.
	//
	// 🔴 Firestore 는 부분 문자열 검색을 인덱스로 못 한다. 그래서 검색은 최신순으로 문서를
	// 읽어 메모리에서 거르는 수밖에 없는데, 상한이 없으면 「없는 이름」 한 번에 컬렉션
	// 전체를 읽는다. 상한에 걸리면 찾은 만큼만 돌려주고 nextCursor 로 이어 받게 한다.
	// 300 인 이유: 하루 수십 건 규모에서 최근 며칠은 한 번에 덮으면서, 무료 쿼터
	// (읽기 50,000/일) 대비 검색 한 번이 0.6% 를 넘지 않는다.
	maxSearchScan = 300
)

type cursorData struct {
	ID         string
	RecordedAt time.Time
	// Q 는 이 커서를 만든 **검색어**다. 검색어가 바뀌면 커서는 무효다 — 다른 검색어로 만든
	// 위치에서 이어 읽으면 앞부분이 통째로 빠진 목록이 나오는데, 앱에서는 그냥 "결과가
	// 적네" 로 보여 아무도 눈치채지 못한다. omitempty 라 검색 없는 커서는 예전과 같은 바이트다.
	Q string `json:",omitempty"`
}

// List 는 최신 통화일시 순 페이징이고, q 가 있으면 **이름 또는 전화번호 뒷자리**로 거른다.
//
// 🔴 **거르기는 서버 메모리에서 한다.** Firestore 는 부분 문자열(뒷자리) 검색을 인덱스로
// 못 하기 때문이다. 저장 시 검색용 필드를 따로 쓰는 방법도 있지만, 뒷자리 검색은 prefix
// 쿼리로 안 되어 "뒷4자리" 같은 파생 필드를 또 심어야 하고 이미 저장된 통화 전부를
// 백필해야 한다 — 통화 요약은 record 필드에 통째 JSON 으로 들어 있어서 백필이 곧
// 전 문서 재작성이다. 하루 수십 건 규모에서 그 복잡도를 살 이유가 없다.
//
// 대신 두 가지로 비용을 묶는다:
//
//	① 한 요청이 읽는 문서 수를 maxSearchScan 으로 막는다.
//	② 상한에 걸리면 찾은 만큼만 돌려주고 nextCursor 를 남긴다. 그러면 items 가 limit 보다
//	   적어도 nextCursor 가 있으면 "여기까지만 찾았다" 는 뜻이고, 앱은 이어 부르면 된다.
//
// 검색이 아닐 때(q=="")는 예전과 똑같이 limit+1 건만 읽는다.
func (s *Store) List(ctx context.Context, uid, q string, limit int, cursor string) (Page, error) {
	return s.list(ctx, uid, q, limit, cursor, maxSearchScan)
}

// list 는 스캔 상한을 인자로 받는다. 상한에 걸렸을 때의 동작(찾은 만큼 + nextCursor)이야말로
// 꼭 확인해야 하는 부분인데, 상한이 상수로 박혀 있으면 테스트가 문서 300건을 먼저 만들어야 한다.
func (s *Store) list(ctx context.Context, uid, q string, limit int, cursor string, scanLimit int) (Page, error) {
	p := Page{Items: []Record{}}
	q = strings.ToLower(strings.TrimSpace(q))
	// 🔴 Firestore 를 만지기 전에 검증한다. 잘못된 limit 으로 쿼리를 날리면 돈만 쓰고 500 이 난다.
	if limit < 1 || limit > maxPageSize || !utf8.ValidString(q) || utf8.RuneCountInString(q) > maxQueryRunes || len(cursor) > 2048 {
		return p, ErrCursor
	}
	var last cursorData
	resumed := cursor != ""
	if resumed {
		b, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil || json.Unmarshal(b, &last) != nil || !validID(last.ID) || last.RecordedAt.IsZero() || last.Q != q {
			return p, ErrCursor
		}
	}
	// 검색이 아니면 한 건 더 읽어 다음 페이지 존재 여부만 확인한다. 빈 페이지를 돌려주지 않기 위해서다.
	budget := limit + 1
	if q != "" {
		budget = scanLimit
	}
	base := s.collection(uid).OrderBy("recordedAt", firestore.Desc).OrderBy(firestore.DocumentID, firestore.Desc)
	pageEnd := last // 지금까지 **담은** 마지막 항목의 위치. 다음 페이지는 여기서 이어져야 한다.
	scanned := 0
	for scanned < budget {
		chunk := budget - scanned
		if chunk > limit+1 {
			chunk = limit + 1
		}
		query := base.Limit(chunk)
		if resumed || scanned > 0 {
			query = query.StartAfter(last.RecordedAt, last.ID)
		}
		docs, e := query.Documents(ctx).GetAll()
		if e != nil {
			return Page{Items: []Record{}}, e
		}
		for _, d := range docs {
			scanned++
			// 커서를 **먼저** 전진시킨다. 손상된 문서 하나 때문에 목록 전체가 막히지 않게
			// 하려면 그 문서를 건너뛰되 다음 페이지는 그 뒤에서 이어져야 한다.
			// 저장 내용이 무엇이든 로그로 새어 나가지 않도록 아무것도 기록하지 않는다.
			recordedAt, ok := d.Data()["recordedAt"].(time.Time)
			if !ok {
				// recordedAt 으로 정렬한 쿼리 결과라 실제로는 오지 않는다. 커서를 만들 수
				// 없으므로 전진시키지 않고 건너뛴다(budget 이 있어 무한 루프는 안 된다).
				continue
			}
			last = cursorData{d.Ref.ID, recordedAt, q}
			r, e := decodeRecord(d)
			if e != nil {
				continue
			}
			if !recipients.MatchesQuery(q, r.Contact.Name, r.Contact.Phone) {
				continue
			}
			if len(p.Items) == limit {
				// 자리가 없는데 또 걸렸다 → 다음 페이지가 확실히 있다.
				p.NextCursor = encodeCursor(pageEnd)
				return p, nil
			}
			p.Items = append(p.Items, r)
			pageEnd = last
		}
		if len(docs) < chunk {
			return p, nil // 더 읽을 문서가 없다. 마지막 페이지다.
		}
	}
	// 읽기 예산을 다 썼는데 컬렉션은 아직 남아 있다. 찾은 만큼만 주고 "여기까지 훑었다" 는
	// 위치를 넘긴다. 검색이 아닐 때는 이 자리가 곧 "limit 건을 채웠고 뒤에 더 있다" 는 뜻이다.
	p.NextCursor = encodeCursor(last)
	return p, nil
}

func encodeCursor(c cursorData) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}
