package recipients

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/sslim7/nature-was/internal/messaging"
	"testing"
	"time"
)

func TestCustomColumnsMandatoryAndPreserve(t *testing.T) {
	data := workbook(t, [][]any{{"이름", "전화번호", "그룹", "직함", "이메일"}, {"홍길동", "01012345678", "가족", "  회장  ", "a@example.com"}, {"제외", "01011112222", "", "", ""}})
	rows, e := parseXLSX(data)
	if e != nil || len(rows) != 2 || len(rows[0].CustomFields) != 2 || rows[0].CustomFields[0].Value != "  회장  " || rows[1].Status != "EXCLUDED" {
		t.Fatal(rows, e)
	}
	for _, head := range [][]any{{"이름", "전화번호"}, {"이름", "전화번호", "그룹", "추가", "추가"}, {"이름", "전화번호", "그룹", "", "값"}, {"이름", "name", "전화번호", "그룹"}} {
		if _, e = parseXLSX(workbook(t, [][]any{head, {"A", "01012345678", "B", "C", "D"}})); e == nil {
			t.Fatal("잘못된 헤더허용", head)
		}
	}
	s, uid := testStore(t)
	ctx := context.Background()
	p, e := s.Preview(ctx, uid, messaging.Upload{Name: "추가.xlsx", DataBase64: base64.StdEncoding.EncodeToString(data)})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Confirm(ctx, uid, p.ID); e != nil {
		t.Fatal(e)
	}
	page, e := s.List(ctx, uid, "", "", "", 50)
	if e != nil || len(page.Items) != 1 {
		t.Fatal(page, e)
	}
	r := page.Items[0]
	if len(r.CustomFields) != 2 {
		t.Fatal(r)
	}
	r, e = s.Save(ctx, uid, r.ID, Input{Name: r.Name, Phone: r.Phone, GroupID: "변경"})
	if e != nil || len(r.CustomFields) != 2 {
		t.Fatal(r, e)
	}
	r, e = s.Save(ctx, uid, r.ID, Input{Name: r.Name, Phone: r.Phone, CustomFields: []CustomField{}})
	if e != nil || r.CustomFields == nil || len(r.CustomFields) != 0 {
		t.Fatal(r, e)
	}
}
func TestNameOrderedPaginationTotal(t *testing.T) {
	s, uid := testStore(t)
	ctx := context.Background()
	batch := s.FS.Batch()
	for i := 129; i >= 0; i-- {
		id := fmt.Sprintf("id%03d", i)
		group := "가"
		if i%2 == 0 {
			group = "나"
		}
		batch.Set(DocumentRef(s.FS, uid, id), Recipient{ID: id, Name: fmt.Sprintf("이름%03d", i/2), Phone: fmt.Sprintf("+82100000%04d", i), GroupID: group})
	}
	if _, e := batch.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	first, e := s.List(ctx, uid, "", "", "", 100)
	if e != nil || first.Total != 130 || len(first.Items) != 100 || first.NextCursor == nil {
		t.Fatal(first, e)
	}
	second, e := s.List(ctx, uid, "", "", *first.NextCursor, 100)
	if e != nil || second.Total != 130 || len(second.Items) != 30 || second.NextCursor != nil {
		t.Fatal(second, e)
	}
	all := append(first.Items, second.Items...)
	for i, r := range all {
		if r.ID != fmt.Sprintf("id%03d", i) || r.CustomFields == nil || r.SentCount != 0 {
			t.Fatal(i, r)
		}
	}
	filtered, e := s.List(ctx, uid, "이름", "나", "", 30)
	if e != nil || filtered.Total != 65 || len(filtered.Items) != 30 {
		t.Fatal(filtered, e)
	}
}

func TestLegacyPhoneLocksAnd010Validation(t *testing.T) {
	s, uid := testStore(t)
	ctx := context.Background()
	id := "legacy-phone"
	old := "+821012345678"
	if _, e := DocumentRef(s.FS, uid, id).Set(ctx, Recipient{ID: id, Name: "기존", Phone: old}); e != nil {
		t.Fatal(e)
	}
	phoneRef(s.FS, uid, old).Set(ctx, map[string]any{"recipientId": id})
	if _, e := s.Save(ctx, uid, "", Input{Name: "중복", Phone: "010-1234-5678"}); e != ErrDuplicate {
		t.Fatal(e)
	}
	p, e := s.List(ctx, uid, "010-1234-5678", "", "", 100)
	if e != nil || len(p.Items) != 1 || p.Items[0].Phone != "01012345678" {
		t.Fatal(p, e)
	}
	r, e := s.Save(ctx, uid, id, Input{Name: "수정", Phone: "01012345678"})
	if e != nil || r.Phone != "01012345678" {
		t.Fatal(r, e)
	}
	if e = s.Delete(ctx, uid, id); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Save(ctx, uid, "", Input{Name: "재등록", Phone: "01012345678"}); e != nil {
		t.Fatal(e)
	}
	data := workbook(t, [][]any{{"이름", "전화번호", "그룹"}, {"해외", "+821011112222", "A"}, {"자리수", "0101111222", "A"}, {"다른국번", "01111112222", "A"}, {"정상", "010-1111-2222", "A"}})
	rows, e := parseXLSX(data)
	if e != nil {
		t.Fatal(e)
	}
	for i, row := range rows {
		if i < 3 && row.Status != "EXCLUDED" || i == 3 && (row.Status != "ADD" || row.Phone != "01011112222") {
			t.Fatal(i, row)
		}
	}
}

func TestConfirmRevalidatesLegacyPreview(t *testing.T) {
	s, uid := testStore(t)
	ctx := context.Background()
	legacy := Import{ID: "old-preview", CreatedAt: time.Now().UTC(), Items: []ImportRow{{Name: "구버전국제번호", Phone: "+821011112222", GroupID: "A", Status: "ADD"}, {Name: "다른국번", Phone: "01111112222", GroupID: "A", Status: "ADD"}, {Name: "그룹누락", Phone: "01011112222", Status: "ADD"}, {Name: "필드잘못됨", Phone: "01033334444", GroupID: "A", CustomFields: []CustomField{{Name: "중복"}, {Name: "중복"}}, Status: "ADD"}, {Name: "정상", Phone: "010-5555-6666", GroupID: "A", Status: "ADD"}, {Name: "파일중복", Phone: "01055556666", GroupID: "A", Status: "ADD"}}}
	if _, e := messaging.Collection(s.FS, uid, "recipientImports").Doc(legacy.ID).Set(ctx, legacy); e != nil {
		t.Fatal(e)
	}
	out, e := s.Confirm(ctx, uid, legacy.ID)
	if e != nil || out.AddedCount != 1 || out.ExcludedCount != 5 {
		t.Fatal(out, e)
	}
	page, e := s.List(ctx, uid, "", "", "", 100)
	if e != nil || len(page.Items) != 1 || page.Items[0].Phone != "01055556666" {
		t.Fatal(page, e)
	}
	again, e := s.Confirm(ctx, uid, legacy.ID)
	if e != nil || again.AddedCount != 1 || again.ExcludedCount != 5 {
		t.Fatal(again, e)
	}
}
