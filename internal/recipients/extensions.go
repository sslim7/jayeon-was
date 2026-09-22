package recipients

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/sslim7/nature-was/internal/httpx"
	"github.com/sslim7/nature-was/internal/messaging"
	"github.com/xuri/excelize/v2"
)

type History struct {
	Source        string                 `json:"source" firestore:"source"`
	ID            string                 `json:"id" firestore:"id"`
	CampaignID    string                 `json:"campaignId,omitempty" firestore:"campaignId"`
	CampaignTitle string                 `json:"campaignTitle" firestore:"campaignTitle"`
	RecipientID   string                 `json:"recipientId" firestore:"recipientId"`
	Name          string                 `json:"name" firestore:"name"`
	Phone         string                 `json:"phone" firestore:"phone"`
	Message       string                 `json:"message" firestore:"message"`
	Status        string                 `json:"status" firestore:"status"`
	Transport     string                 `json:"transport,omitempty" firestore:"transport"`
	SentAt        *time.Time             `json:"sentAt" firestore:"sentAt"`
	FailedAt      *time.Time             `json:"failedAt" firestore:"failedAt"`
	ErrorCode     string                 `json:"errorCode" firestore:"errorCode"`
	ErrorMessage  string                 `json:"errorMessage" firestore:"errorMessage"`
	CreatedAt     time.Time              `json:"createdAt" firestore:"createdAt"`
	UpdatedAt     time.Time              `json:"updatedAt" firestore:"updatedAt"`
	Attachments   []messaging.Attachment `json:"attachments" firestore:"attachments"`
}
type ImportRow struct {
	CustomFields []CustomField `json:"customFields" firestore:"customFields"`
	Row          int           `json:"row" firestore:"row"`
	Name         string        `json:"name" firestore:"name"`
	Phone        string        `json:"phone" firestore:"phone"`
	GroupID      string        `json:"groupId" firestore:"groupId"`
	Status       string        `json:"status" firestore:"status"`
	Reason       string        `json:"reason" firestore:"reason"`
}
type Import struct {
	ID            string      `json:"id" firestore:"id"`
	AddedCount    int         `json:"addedCount" firestore:"addedCount"`
	ExcludedCount int         `json:"excludedCount" firestore:"excludedCount"`
	Items         []ImportRow `json:"items" firestore:"items"`
	CreatedAt     time.Time   `json:"createdAt" firestore:"createdAt"`
	Confirmed     bool        `json:"confirmed" firestore:"confirmed"`
	// 아래 셋은 저장 구조를 위한 값이라 응답에 싣지 않는다. Items 는 chunks 하위 컬렉션에서
	// 다시 모아 채운다. 이전 형식(메타 문서 안에 items 가 통째로 있던 미리보기)에서는
	// ChunkCount 가 0 이고 Items 가 그대로 들어 있다.
	ChunkCount      int `json:"-" firestore:"chunkCount"`
	TotalRows       int `json:"-" firestore:"totalRows"`
	ConfirmedChunks int `json:"-" firestore:"confirmedChunks"`
}

// 🔴 미리보기 메타 문서는 items 를 담지 않는다. 행은 chunks 하위 컬렉션에 나눠 넣는다.
// 예전처럼 한 문서에 전부 넣으면 Firestore 문서 한도(1MiB)에 걸려 저장 자체가 거절됐고,
// 그걸 막으려고 행 수를 200 으로 잠가 두는 악순환이 있었다.
type importMeta struct {
	ID              string    `firestore:"id"`
	AddedCount      int       `firestore:"addedCount"`
	ExcludedCount   int       `firestore:"excludedCount"`
	CreatedAt       time.Time `firestore:"createdAt"`
	Confirmed       bool      `firestore:"confirmed"`
	ChunkCount      int       `firestore:"chunkCount"`
	TotalRows       int       `firestore:"totalRows"`
	ConfirmedChunks int       `firestore:"confirmedChunks"`
}

type importChunk struct {
	Items []ImportRow `firestore:"items"`
}

const (
	// 🔴 확정은 청크 하나를 트랜잭션 하나로 처리한다. ADD 행마다 쓰기가 2건이고
	// (수신자 문서 + 번호 잠금 문서) 여기에 청크 문서와 메타 문서 갱신이 붙는다.
	// Firestore 트랜잭션의 쓰기 상한이 500건이라 200행이면 200*2+2=402 로 여유가 남는다.
	// ⚠️ 이 값을 올리면 상한을 넘어 확정 트랜잭션이 통째로 실패한다 — 일부만 저장되는 게
	// 아니라 그 청크가 전부 안 된다.
	maxRowsPerChunk = 200
	// 청크 하나가 Firestore 문서 하나다. 문서 한도 1MiB 에 여유를 둔 값이라,
	// 열이 넓은 파일은 200행이 되기 전에 여기서 먼저 끊긴다.
	maxChunkBytes = 500 << 10
	// ⚠️ 이건 기능 한도가 아니라 서버 메모리 안전선이다. parseXLSX 는 모든 행을 메모리에
	// 올리므로 무한정 받을 수 없다. 실질 상한은 이미 업로드 2MiB 가 잡고 있고 이 값은
	// 그보다 훨씬 위라 정상 파일은 닿지 않는다. 행이 많다고 막는 게 아니라 나눠서 처리한다.
	maxParseRows = 10000
	// 번호 잠금을 한 번에 몇 개씩 읽을지. gRPC 메시지 한도에 여유를 두는 값이다.
	phoneLookupBatch = 300
	// 이름·전화번호·그룹 외의 열 수. validate() 의 「추가 항목은 최대 20개」와 같은 값이라
	// 여기서 먼저 막지 않으면 모든 행이 EXCLUDED 로 떨어져 이유가 행마다 흩어진다.
	maxExtraColumns = 20
)

func chunkRef(ref *firestore.DocumentRef, n int) *firestore.DocumentRef {
	return ref.Collection("chunks").Doc(fmt.Sprintf("chunk-%04d", n))
}

func (v *Import) count() {
	v.AddedCount = 0
	v.ExcludedCount = 0
	for _, r := range v.Items {
		if r.Status == "ADD" {
			v.AddedCount++
		} else {
			v.ExcludedCount++
		}
	}
}
func parseXLSX(data []byte) (out []ImportRow, err error) {
	// 손상된 외부 파일이 파서에서 panic을 일으켜도 요청 단위 검증 오류로 제한한다.
	defer func() {
		if recover() != nil {
			out = nil
			err = messaging.ValidationError{Message: "엑셀 파일을 읽다가 실패했어요. 엑셀에서 「.xlsx」로 다시 저장한 뒤 올려 주세요."}
		}
	}()
	f, e := excelize.OpenReader(bytes.NewReader(data), excelize.Options{UnzipSizeLimit: 20 << 20, UnzipXMLSizeLimit: 10 << 20})
	if e != nil {
		return nil, messaging.ValidationError{Message: "엑셀 파일을 열 수 없어요. 「.xlsx」 파일이 맞는지, 파일이 손상되지 않았는지 확인해 주세요."}
	}
	defer f.Close()
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, messaging.ValidationError{Message: "엑셀에 시트가 없어요. 첫 시트에 자료를 넣어 주세요."}
	}
	rows, e := f.Rows(sheets[0])
	if e != nil {
		return nil, messaging.ValidationError{Message: "첫 시트를 읽을 수 없어요. 파일이 손상되지 않았는지 확인해 주세요."}
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, messaging.ValidationError{Message: "첫 행에 열 제목이 없어요. 첫 행에 「이름」과 「전화번호」를 넣어 주세요."}
	}
	header, e := rows.Columns()
	if e != nil {
		return nil, messaging.ValidationError{Message: "첫 행의 열 제목을 읽을 수 없어요. 파일이 손상되지 않았는지 확인해 주세요."}
	}
	cols := map[string]int{}
	extra := []int{}
	headers := map[string]bool{}
	for i, h := range header {
		h = strings.TrimSpace(h)
		// ⚠️ 몇 번째 열인지/어떤 제목인지 말해 주지 않으면 사용자는 21개 열 중 어디를 고쳐야
		// 할지 알 수 없다. 「확인해 주세요」로 뭉뚱그리지 않는다.
		if h == "" {
			return nil, messaging.ValidationError{Message: fmt.Sprintf("%d번째 열의 제목이 비어 있어요. 첫 행에 제목을 넣거나 그 열을 지워 주세요.", i+1)}
		}
		if headers[h] {
			return nil, messaging.ValidationError{Message: fmt.Sprintf("열 제목 「%s」이 두 번 나옵니다. 하나만 남겨 주세요.", h)}
		}
		headers[h] = true
		header[i] = h
		key := ""
		switch h {
		case "이름", "name":
			key = "name"
		case "전화번호", "연락처", "phone":
			key = "phone"
		case "그룹", "groupId":
			key = "groupId"
		}
		if key != "" {
			if prev, exists := cols[key]; exists {
				return nil, messaging.ValidationError{Message: fmt.Sprintf("「%s」와 「%s」는 같은 뜻이라 함께 쓸 수 없어요. 하나만 남겨 주세요.", header[prev], h)}
			}
			cols[key] = i
		} else {
			extra = append(extra, i)
		}
	}
	if len(extra) > maxExtraColumns {
		return nil, messaging.ValidationError{Message: fmt.Sprintf("이름·전화번호·그룹 외의 열은 %d개까지예요. 이 파일은 %d개입니다. 열을 줄여서 올려 주세요.", maxExtraColumns, len(extra))}
	}
	_, hasName := cols["name"]
	_, hasPhone := cols["phone"]
	switch {
	case !hasName && !hasPhone:
		return nil, messaging.ValidationError{Message: "첫 행에 「이름」과 「전화번호」(또는 「연락처」) 열이 필요해요."}
	case !hasName:
		return nil, messaging.ValidationError{Message: "첫 행에 「이름」 열이 없어요. 이름 열을 넣어 주세요."}
	case !hasPhone:
		return nil, messaging.ValidationError{Message: "첫 행에 「전화번호」(또는 「연락처」) 열이 없어요."}
	}
	seen := map[string]bool{}
	out = []ImportRow{}
	rowNo := 1
	for rows.Next() {
		rowNo++
		if rowNo > maxParseRows+1 {
			// ⚠️ 여기서 바로 돌아가면 "한도를 넘었다" 밖에 말하지 못한다. 남은 행을 처리 없이
			// 세기만 해서 실제 행 수를 알려 준다. 파일이 이미 2MiB 로 제한돼 세는 비용은 유한하다.
			total := rowNo - 1
			for rows.Next() {
				total++
			}
			return nil, messaging.ValidationError{Message: fmt.Sprintf("한 번에 %d행까지 읽을 수 있어요. 이 파일은 %d행입니다. 파일을 나누어 올려 주세요.", maxParseRows, total)}
		}
		cells, e := rows.Columns()
		if e != nil {
			return nil, messaging.ValidationError{Message: fmt.Sprintf("%d행을 읽을 수 없어요. 파일이 손상되지 않았는지 확인해 주세요.", rowNo)}
		}
		get := func(k string) string {
			i, ok := cols[k]
			if !ok || i >= len(cells) {
				return ""
			}
			return strings.TrimSpace(cells[i])
		}
		if len(cells) > len(header) {
			return nil, messaging.ValidationError{Message: fmt.Sprintf("%d행에 열 제목보다 많은 칸(%d칸)이 있어요. 첫 행에 제목을 추가하거나 오른쪽 빈 칸을 지워 주세요.", rowNo, len(cells))}
		}
		fields := []CustomField{}
		for _, i := range extra {
			v := ""
			if i < len(cells) {
				v = cells[i]
			}
			fields = append(fields, CustomField{Name: header[i], Value: v})
		}
		in := Input{Name: get("name"), Phone: get("phone"), GroupID: get("groupId"), CustomFields: fields}
		blank := true
		for _, v := range cells {
			if strings.TrimSpace(v) != "" {
				blank = false
				break
			}
		}
		if blank {
			continue
		}
		r := ImportRow{CustomFields: fields, Row: rowNo, Name: in.Name, Phone: in.Phone, GroupID: in.GroupID, Status: "ADD"}
		in, e = validate(in)
		if e != nil {
			r.Status = "EXCLUDED"
			r.Reason = e.Error()
		} else {
			r.Name = in.Name
			r.Phone = in.Phone
			r.GroupID = in.GroupID
			if seen[in.Phone] {
				r.Status = "EXCLUDED"
				r.Reason = "파일 내 중복 전화번호"
			}
			seen[in.Phone] = true
		}
		out = append(out, r)
	}
	if e = rows.Error(); e != nil {
		return nil, messaging.ValidationError{Message: "엑셀을 끝까지 읽지 못했어요. 파일이 손상되지 않았는지 확인해 주세요."}
	}
	if len(out) == 0 {
		return nil, messaging.ValidationError{Message: "자료 행이 없어요. 두 번째 행부터 이름과 전화번호를 넣어 주세요."}
	}
	return out, nil
}

// splitChunks 는 행을 저장 단위로 끊는다. 행 수와 크기 두 기준을 모두 지키며,
// 둘 중 먼저 닿는 쪽에서 끊는다(열이 넓은 파일은 200행이 되기 전에 크기로 먼저 끊긴다).
func splitChunks(rows []ImportRow) ([][]ImportRow, error) {
	out := [][]ImportRow{}
	cur := []ImportRow{}
	size := 2 // JSON 배열의 대괄호
	for i := range rows {
		b, e := json.Marshal(rows[i])
		if e != nil {
			return nil, e
		}
		// ⚠️ 한 행이 혼자서도 청크 한도를 넘으면 더 나눌 방법이 없다. 그 행을 지목해 거절한다.
		// 🔴 앞 행 유무와 무관하게 행 자체 크기로 판단해야 한다. "지금 청크에 안 들어가면
		// 새 청크로" 로만 처리하면 큰 행이 빈 새 청크에 들어가 한도를 넘은 채 저장된다.
		// 3 은 JSON 배열의 대괄호와 쉼표 몫이다.
		if len(b)+3 > maxChunkBytes {
			return nil, messaging.ValidationError{Message: fmt.Sprintf("%d행의 내용이 너무 큽니다. 그 행의 칸을 줄여 주세요.", rows[i].Row)}
		}
		if len(cur) == maxRowsPerChunk || size+len(b)+1 > maxChunkBytes {
			out = append(out, cur)
			cur = []ImportRow{}
			size = 2
		}
		cur = append(cur, rows[i])
		size += len(b) + 1
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out, nil
}

// markRegistered 는 이미 등록된 번호인 행을 제외 처리한다.
// ⚠️ 행마다 Get 을 돌리면 행 수만큼 왕복이 생겨 큰 파일에서 미리보기가 사실상 멈춘다.
// GetAll 은 한 번의 요청으로 여러 문서를 읽으므로 왕복이 행 수와 무관해진다.
func (s *Store) markRegistered(ctx context.Context, uid string, rows []ImportRow) error {
	refs := []*firestore.DocumentRef{}
	owner := []int{} // refs[i] 가 어느 행의 것인지
	for i := range rows {
		if rows[i].Status != "ADD" {
			continue
		}
		for _, key := range phoneKeys(rows[i].Phone) {
			refs = append(refs, phoneRef(s.FS, uid, key))
			owner = append(owner, i)
		}
	}
	for start := 0; start < len(refs); start += phoneLookupBatch {
		end := min(start+phoneLookupBatch, len(refs))
		docs, e := s.FS.GetAll(ctx, refs[start:end])
		if e != nil {
			return e
		}
		for i, d := range docs {
			if !d.Exists() {
				continue
			}
			r := &rows[owner[start+i]]
			if r.Status != "ADD" {
				continue
			}
			r.Status = "EXCLUDED"
			r.Reason = "이미 등록된 전화번호"
		}
	}
	return nil
}
func (s *Store) Preview(ctx context.Context, uid string, in messaging.Upload) (Import, error) {
	data, e := base64.StdEncoding.DecodeString(in.DataBase64)
	if e != nil {
		return Import{}, messaging.ValidationError{Message: "파일을 읽을 수 없어요. 파일을 다시 선택해 주세요."}
	}
	if len(data) > 2<<20 {
		return Import{}, messaging.ValidationError{Message: "파일은 2MiB까지 올릴 수 있어요. 파일을 나누어 올려 주세요."}
	}
	if !strings.HasSuffix(strings.ToLower(in.Name), ".xlsx") {
		return Import{}, messaging.ValidationError{Message: "「.xlsx」 파일만 올릴 수 있어요. 엑셀에서 「.xlsx」로 저장한 뒤 올려 주세요."}
	}
	rows, e := parseXLSX(data)
	if e != nil {
		return Import{}, e
	}
	if e = s.markRegistered(ctx, uid, rows); e != nil {
		return Import{}, e
	}
	chunks, e := splitChunks(rows)
	if e != nil {
		return Import{}, e
	}
	ref := messaging.Collection(s.FS, uid, "recipientImports").NewDoc()
	out := Import{ID: ref.ID, Items: rows, CreatedAt: time.Now().UTC(), ChunkCount: len(chunks), TotalRows: len(rows)}
	out.count()
	// 🔴 청크를 먼저 쓰고 메타를 마지막에 쓴다. 중간에 끊기면 메타가 없어 확정이 시작되지 않는다.
	// 반대 순서면 행이 빠진 미리보기를 확정할 수 있고, 그건 조용히 일부만 저장되는 사고다.
	for i, c := range chunks {
		if _, e = chunkRef(ref, i).Create(ctx, importChunk{Items: c}); e != nil {
			return Import{}, e
		}
	}
	_, e = ref.Create(ctx, importMeta{ID: ref.ID, AddedCount: out.AddedCount, ExcludedCount: out.ExcludedCount, CreatedAt: out.CreatedAt, ChunkCount: out.ChunkCount, TotalRows: out.TotalRows})
	if e != nil {
		return Import{}, e
	}
	return out, nil
}

// loadImport 는 메타와 청크를 모아 응답용 Import 를 만든다.
// 이전 형식(메타 문서 안에 items) 은 청크가 없으므로 읽은 그대로 쓴다.
func (s *Store) loadImport(ctx context.Context, ref *firestore.DocumentRef, meta Import) (Import, error) {
	meta.ID = ref.ID
	if meta.ChunkCount == 0 {
		if meta.Items == nil {
			meta.Items = []ImportRow{}
		}
		return meta, nil
	}
	items := []ImportRow{}
	for n := range meta.ChunkCount {
		doc, e := chunkRef(ref, n).Get(ctx)
		if e != nil {
			return Import{}, e
		}
		var c importChunk
		if e = doc.DataTo(&c); e != nil {
			return Import{}, e
		}
		items = append(items, c.Items...)
	}
	meta.Items = items
	return meta, nil
}

// confirmChunk 는 청크 하나를 트랜잭션 하나로 확정한다.
// 🔴 중간에 끊겨도 안전한 이유: 수신자·번호 잠금 쓰기와 메타의 confirmedChunks 증가가 같은
// 트랜잭션에 들어 있다. 청크는 전부 되거나 전부 안 된다. 같은 요청을 다시 부르면
// confirmedChunks 부터 이어 가므로 이미 만든 수신자가 두 번 만들어지지 않는다.
func (s *Store) confirmChunk(ctx context.Context, uid string, ref *firestore.DocumentRef, n int, legacy bool) error {
	return s.FS.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		doc, e := tx.Get(ref)
		if e != nil {
			return e
		}
		var meta Import
		if e = doc.DataTo(&meta); e != nil {
			return e
		}
		// 다른 요청이 먼저 이 청크를 끝냈다. 다시 쓰면 같은 수신자가 두 번 생긴다.
		if meta.Confirmed || meta.ConfirmedChunks > n {
			return nil
		}
		items := meta.Items
		if !legacy {
			cdoc, e := tx.Get(chunkRef(ref, n))
			if e != nil {
				return e
			}
			var c importChunk
			if e = cdoc.DataTo(&c); e != nil {
				return e
			}
			items = c.Items
		}
		// 저장된 미리보기가 이전 버전에서 만들어졌어도 확정 시 최신 입력 규칙을 다시 적용한다.
		// ⚠️ 청크를 넘나드는 파일 내 중복은 여기 seen 으로 못 잡지만, 앞 청크가 남긴 번호 잠금을
		// 아래에서 읽어 「이미 등록된 전화번호」로 제외한다. 중복 생성은 어느 쪽으로도 안 된다.
		seen := map[string]bool{}
		for i := range items {
			r := &items[i]
			if r.Status != "ADD" {
				continue
			}
			in, validationErr := validate(Input{Name: r.Name, Phone: r.Phone, GroupID: r.GroupID, CustomFields: r.CustomFields})
			if validationErr != nil {
				r.Status = "EXCLUDED"
				r.Reason = "확정 시 검증 실패: " + validationErr.Error()
				continue
			}
			if seen[in.Phone] {
				r.Status = "EXCLUDED"
				r.Reason = "확정 시 파일 내 중복 전화번호"
				continue
			}
			seen[in.Phone] = true
			r.Name = in.Name
			r.Phone = in.Phone
			r.GroupID = in.GroupID
			r.CustomFields = in.CustomFields
			if r.CustomFields == nil {
				r.CustomFields = []CustomField{}
			}
		}
		// 모든 번호 잠금을 먼저 읽고 나서 쓰기 시작한다. 다른 import/수동등록과 경합해도 중복되지 않는다.
		// ⚠️ 한 건씩 읽으면 청크마다 왕복이 수백 번이라 확정이 몇십 초씩 걸리고 요청이 먼저
		// 끊긴다. 한꺼번에 읽는다 — 없는 문서도 읽은 것으로 기록되므로 경합 감지는 그대로다.
		locks := []*firestore.DocumentRef{}
		owner := []int{} // locks[i] 가 어느 행의 것인지
		for i := range items {
			if items[i].Status != "ADD" {
				continue
			}
			for _, key := range phoneKeys(items[i].Phone) {
				locks = append(locks, phoneRef(s.FS, uid, key))
				owner = append(owner, i)
			}
		}
		for start := 0; start < len(locks); start += phoneLookupBatch {
			end := min(start+phoneLookupBatch, len(locks))
			snaps, e := tx.GetAll(locks[start:end])
			if e != nil {
				return e
			}
			for i, d := range snaps {
				if !d.Exists() {
					continue
				}
				r := &items[owner[start+i]]
				if r.Status != "ADD" {
					continue
				}
				r.Status = "EXCLUDED"
				r.Reason = "확정 시 이미 등록된 전화번호"
			}
		}
		now := time.Now().UTC()
		added, excluded := 0, 0
		for _, r := range items {
			if r.Status != "ADD" {
				excluded++
				continue
			}
			added++
			dest := collection(s.FS, uid).NewDoc()
			item := Recipient{CustomFields: r.CustomFields, ID: dest.ID, Name: r.Name, Phone: r.Phone, GroupID: r.GroupID, CreatedAt: now, UpdatedAt: now}
			if e = tx.Create(dest, item); e != nil {
				return e
			}
			if e = tx.Set(phoneRef(s.FS, uid, r.Phone), map[string]any{"recipientId": dest.ID}); e != nil {
				return e
			}
		}
		// 첫 청크에서 미리보기 집계를 확정 집계로 갈아 끼우고, 이후 청크는 더한다.
		if n == 0 {
			meta.AddedCount, meta.ExcludedCount = 0, 0
		}
		meta.AddedCount += added
		meta.ExcludedCount += excluded
		meta.ConfirmedChunks = n + 1
		total := meta.ChunkCount
		if legacy {
			total = 1
		}
		meta.Confirmed = meta.ConfirmedChunks >= total
		if legacy {
			meta.Items = items
			return tx.Set(ref, meta)
		}
		if e = tx.Set(chunkRef(ref, n), importChunk{Items: items}); e != nil {
			return e
		}
		return tx.Set(ref, importMeta{ID: ref.ID, AddedCount: meta.AddedCount, ExcludedCount: meta.ExcludedCount, CreatedAt: meta.CreatedAt, Confirmed: meta.Confirmed, ChunkCount: meta.ChunkCount, TotalRows: meta.TotalRows, ConfirmedChunks: meta.ConfirmedChunks})
	})
}
func (s *Store) Confirm(ctx context.Context, uid, id string) (Import, error) {
	if !ValidateID(id) {
		return Import{}, messaging.ErrInvalid
	}
	ref := messaging.Collection(s.FS, uid, "recipientImports").Doc(id)
	doc, e := ref.Get(ctx)
	if e != nil {
		return Import{}, e
	}
	var meta Import
	if e = doc.DataTo(&meta); e != nil {
		return Import{}, e
	}
	if meta.Confirmed {
		return s.loadImport(ctx, ref, meta)
	}
	// ⚠️ 이전 형식: items 가 메타 문서 안에 통째로 들어 있다. 배포 직전에 만든 미리보기가
	// 24시간 동안 남아 있으므로 그것도 확정되게 해야 한다. 못 읽으면 사용자는 방금 만든
	// 미리보기를 잃는다. 청크가 없으면 items 를 청크 하나로 취급한다(옛 한도가 200행이라
	// 트랜잭션 쓰기 상한 안에 들어온다).
	legacy := meta.ChunkCount == 0
	total := meta.ChunkCount
	if legacy {
		total = 1
		if len(meta.Items) == 0 {
			return Import{}, messaging.ValidationError{Message: "미리보기 자료가 없어요. 파일을 다시 올려 주세요."}
		}
	}
	if time.Since(meta.CreatedAt) > 24*time.Hour {
		return Import{}, messaging.ValidationError{Message: "미리보기는 만든 지 24시간 안에만 확정할 수 있어요. 파일을 다시 올려 주세요."}
	}
	for n := meta.ConfirmedChunks; n < total; n++ {
		if e = s.confirmChunk(ctx, uid, ref, n, legacy); e != nil {
			return Import{}, e
		}
	}
	if doc, e = ref.Get(ctx); e != nil {
		return Import{}, e
	}
	meta = Import{}
	if e = doc.DataTo(&meta); e != nil {
		return Import{}, e
	}
	return s.loadImport(ctx, ref, meta)
}
func (s *Store) History(ctx context.Context, uid, id string) ([]History, error) {
	if !ValidateID(id) {
		return nil, messaging.ErrInvalid
	}
	docs, e := DocumentRef(s.FS, uid, id).Collection("history").Documents(ctx).GetAll()
	if e != nil {
		return nil, e
	}
	items := []History{}
	seen := map[string]bool{}
	for _, d := range docs {
		var h History
		if e = d.DataTo(&h); e != nil {
			return nil, e
		}
		if h.Attachments == nil {
			h.Attachments = []messaging.Attachment{}
		}
		if h.Source == "" {
			h.Source = "ANDROID"
		}
		items = append(items, h)
		seen[h.ID] = true
	}
	// 확장 이전 캠페인의 최종 결과도 스냅샷으로 읽는다. 원본 수신자 수정/삭제와 무관하다.
	campaigns, e := messaging.Collection(s.FS, uid, "smsCampaigns").Documents(ctx).GetAll()
	if e != nil {
		return nil, e
	}
	for _, d := range campaigns {
		var legacy struct {
			Campaign struct {
				ID    string `firestore:"id"`
				Title string `firestore:"title"`
			} `firestore:"campaign"`
			Recipients []struct {
				History
				AttemptID string `firestore:"attemptId"`
			} `firestore:"recipients"`
		}
		if e = d.DataTo(&legacy); e != nil {
			return nil, e
		}
		for _, r := range legacy.Recipients {
			if r.RecipientID != id || r.Status != "SENT" && r.Status != "FAILED" {
				continue
			}
			h := r.History
			h.ID = d.Ref.ID + "_" + h.ID + "_" + r.AttemptID
			if seen[h.ID] {
				continue
			}
			h.Source = "ANDROID"
			h.CampaignTitle = legacy.Campaign.Title
			h.CampaignID = d.Ref.ID
			if h.Attachments == nil {
				h.Attachments = []messaging.Attachment{}
			}
			items = append(items, h)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := HistoryTime(items[i]), HistoryTime(items[j])
		if a.Equal(b) {
			return items[i].ID > items[j].ID
		}
		return a.After(b)
	})
	return items, nil
}
func registerExtensions(mux *http.ServeMux, h *handler, guard func(http.Handler) http.Handler) {
	registerExternalSends(mux, h, guard)
	mux.Handle("POST /recipients/imports/preview", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := prepare(w, r)
		if uid == "" {
			return
		}
		in, ok := messaging.DecodeUpload(w, r, 2<<20)
		if !ok {
			return
		}
		out, e := h.store.Preview(r.Context(), uid, in)
		if e != nil {
			messaging.Fail(w, e)
			return
		}
		httpx.WriteJSON(w, 200, out)
	})))
	mux.Handle("POST /recipients/imports/{id}/confirm", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := prepare(w, r)
		if uid == "" {
			return
		}
		out, e := h.store.Confirm(r.Context(), uid, r.PathValue("id"))
		if e != nil {
			messaging.Fail(w, e)
			return
		}
		httpx.WriteJSON(w, 200, out)
	})))
	mux.Handle("GET /recipients/{id}/history", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := prepare(w, r)
		if uid == "" {
			return
		}
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || n > 100 {
				messaging.Fail(w, messaging.ErrInvalid)
				return
			}
			limit = n
		}
		items, e := h.store.History(r.Context(), uid, r.PathValue("id"))
		if e != nil {
			messaging.Fail(w, e)
			return
		}
		c := r.URL.Query().Get("cursor")
		start := 0
		if c != "" {
			found := false
			for i, v := range items {
				if v.ID == c {
					start = i + 1
					found = true
					break
				}
			}
			if !found {
				messaging.Fail(w, messaging.ErrInvalid)
				return
			}
		}
		end := start + limit
		var next *string
		if end < len(items) {
			v := items[end-1].ID
			next = &v
		} else {
			end = len(items)
		}
		httpx.WriteJSON(w, 200, struct {
			Items      []History `json:"items"`
			NextCursor *string   `json:"nextCursor"`
		}{items[start:end], next})
	})))
}

// 기존 성공 시도도 집계하여 새 필드가 없던 수신자를 보강한다.
type sentStats struct {
	Latest time.Time
	Count  int
}

func (s *Store) legacyStats(ctx context.Context, uid string) (map[string]sentStats, error) {
	out := map[string]sentStats{}
	docs, e := messaging.Collection(s.FS, uid, "smsCampaigns").Documents(ctx).GetAll()
	if e != nil {
		return nil, e
	}
	for _, d := range docs {
		var v struct {
			Recipients []struct {
				RecipientID string     `firestore:"recipientId"`
				Status      string     `firestore:"status"`
				SentAt      *time.Time `firestore:"sentAt"`
				Attempts    []struct {
					ID         string     `firestore:"id"`
					Status     string     `firestore:"status"`
					FinishedAt *time.Time `firestore:"finishedAt"`
				} `firestore:"attempts"`
			} `firestore:"recipients"`
		}
		if e = d.DataTo(&v); e != nil {
			return nil, e
		}
		for _, r := range v.Recipients {
			st := out[r.RecipientID]
			count := 0
			seen := map[string]bool{}
			for _, a := range r.Attempts {
				if a.Status == "SENT" && !seen[a.ID] {
					seen[a.ID] = true
					count++
					if a.FinishedAt != nil && a.FinishedAt.After(st.Latest) {
						st.Latest = *a.FinishedAt
					}
				}
			}
			if r.Status == "SENT" && count == 0 {
				count = 1
			}
			st.Count += count
			if r.Status == "SENT" && r.SentAt != nil && r.SentAt.After(st.Latest) {
				st.Latest = *r.SentAt
			}
			out[r.RecipientID] = st
		}
	}
	external, e := ExternalHistory(ctx, s.FS, uid)
	if e != nil {
		return nil, e
	}
	for _, h := range external {
		st := out[h.RecipientID]
		st.Count++
		if h.SentAt != nil && h.SentAt.After(st.Latest) {
			st.Latest = *h.SentAt
		}
		out[h.RecipientID] = st
	}
	return out, nil
}

// SuccessfulCount는 이전 버전 문서의 성공 시도를 포함한 집계다.
func (s *Store) SuccessfulCount(ctx context.Context, uid, id string) (int, error) {
	v, e := s.legacyStats(ctx, uid)
	if e != nil {
		return 0, e
	}
	return v[id].Count, nil
}

func HistoryTime(h History) time.Time {
	if h.SentAt != nil {
		return *h.SentAt
	}
	if h.FailedAt != nil {
		return *h.FailedAt
	}
	return h.UpdatedAt
}
