package calls

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
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
	List(context.Context, string, string) (Page, error)
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
	ref := s.collection(uid).Doc(p.callID)
	err := s.FS.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		existing, e := tx.Get(ref)
		if e == nil {
			if existing.Data()["digest"] == p.digest {
				return nil
			}
			return ErrConflict
		}
		if status.Code(e) != codes.NotFound {
			return e
		}
		if e = tx.Set(ref, map[string]any{"record": p.meta, "digest": p.digest, "recordedAt": p.recordedAt, "transcriptShards": p.shards, "todoCount": len(p.todos), "syncedAt": firestore.ServerTimestamp}); e != nil {
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
		if e = tx.Set(ref.Collection("analysis").Doc("v1"), map[string]any{"data": p.analysis}); e != nil {
			return e
		}
		for i, b := range p.todos {
			if e = tx.Set(ref.Collection("todos").Doc(fmt.Sprintf("%05d", i)), map[string]any{"data": b, "completed": false}); e != nil {
				return e
			}
		}
		return nil
	})
	return p.summary, err
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
	refs := []*firestore.DocumentRef{ref.Collection("analysis").Doc("v1")}
	// 검사 없는 타입 단언은 문서가 조금만 어긋나도 panic 이고, panic 은 이 요청 하나가
	// 아니라 인스턴스 전체를 끊는다. nil·타입 불일치·말이 안 되는 개수는 500 으로 돌린다.
	shards, okShards := doc.Data()["transcriptShards"].(int64)
	todoCount, okTodos := doc.Data()["todoCount"].(int64)
	if !okShards || !okTodos || shards < 0 || shards > maxShards || todoCount < 0 || todoCount > maxTransactionWrites {
		return r, errStored
	}
	count, todos := int(shards), int(todoCount)
	for i := 0; i < count; i++ {
		refs = append(refs, ref.Collection("transcript").Doc(fmt.Sprintf("%05d", i)))
	}
	for i := 0; i < todos; i++ {
		refs = append(refs, ref.Collection("todos").Doc(fmt.Sprintf("%05d", i)))
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
	b, e := data(docs[0])
	if e != nil {
		return r, e
	}
	r.Analysis = &Analysis{}
	if e = json.Unmarshal(b, r.Analysis); e != nil {
		return r, e
	}
	var transcript []byte
	for _, d := range docs[1 : 1+count] {
		b, e = data(d)
		if e != nil {
			return r, e
		}
		transcript = append(transcript, b...)
	}
	r.Transcript = &Transcript{}
	if e = json.Unmarshal(transcript, r.Transcript); e != nil {
		return r, e
	}
	for _, d := range docs[1+count:] {
		b, e = data(d)
		if e != nil {
			return r, e
		}
		var t Todo
		if e = json.Unmarshal(b, &t); e != nil {
			return r, e
		}
		r.Analysis.Todos = append(r.Analysis.Todos, t)
	}
	return r, nil
}

// pageSize 는 서버가 정하는 페이지 크기다. 클라이언트가 보내는 limit 은 쓰지 않는다.
const pageSize = 30

type cursorData struct {
	ID         string
	RecordedAt time.Time
}

// 최신 통화일시 순 페이징만 한다. 이름 필터는 앱이 받아 온 목록에서 직접 걸러낸다 —
// 서버가 전체를 훑어 부분검색을 하면 화면에 한 건도 안 나오는 페이지에서도 Firestore
// 읽기 비용이 그대로 발생한다.
func (s *Store) List(ctx context.Context, uid, cursor string) (Page, error) {
	p := Page{Items: []Record{}}
	// 한 건 더 읽어 다음 페이지 존재 여부만 확인한다. 빈 페이지를 돌려주지 않기 위해서다.
	q := s.collection(uid).OrderBy("recordedAt", firestore.Desc).OrderBy(firestore.DocumentID, firestore.Desc).Limit(pageSize + 1)
	if cursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(cursor)
		var c cursorData
		if e != nil || json.Unmarshal(b, &c) != nil || !validID(c.ID) || c.RecordedAt.IsZero() {
			return p, ErrCursor
		}
		q = q.StartAfter(c.RecordedAt, c.ID)
	}
	iter := q.Documents(ctx)
	defer iter.Stop()
	var last cursorData
	scanned := 0
	for {
		d, e := iter.Next()
		if e == iterator.Done {
			return p, nil
		}
		if e != nil {
			return p, e
		}
		if scanned == pageSize {
			b, _ := json.Marshal(last)
			p.NextCursor = base64.RawURLEncoding.EncodeToString(b)
			return p, nil
		}
		scanned++
		// 커서를 **먼저** 전진시킨다. 손상된 문서 하나 때문에 목록 전체가 막히지 않게
		// 하려면 그 문서를 건너뛰되 다음 페이지는 그 뒤에서 이어져야 한다.
		// 저장 내용이 무엇이든 로그로 새어 나가지 않도록 아무것도 기록하지 않는다.
		recordedAt, ok := d.Data()["recordedAt"].(time.Time)
		if !ok {
			// recordedAt 으로 정렬한 쿼리 결과라 실제로는 오지 않는다. 커서를 만들 수
			// 없으므로 전진시키지 않고 건너뛴다.
			continue
		}
		last = cursorData{d.Ref.ID, recordedAt}
		r, e := decodeRecord(d)
		if e != nil {
			continue
		}
		p.Items = append(p.Items, r)
	}
}
