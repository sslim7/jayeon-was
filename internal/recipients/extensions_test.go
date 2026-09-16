package recipients

import (
	"bytes"
	"context"
	"encoding/base64"
	"github.com/sslim7/nature-was/internal/messaging"
	"github.com/xuri/excelize/v2"
	"sync"
	"testing"
)

func workbook(t *testing.T, rows [][]any) []byte {
	t.Helper()
	f := excelize.NewFile()
	defer f.Close()
	for i, row := range rows {
		cell, _ := excelize.CoordinatesToCellName(1, i+1)
		if e := f.SetSheetRow("Sheet1", cell, &row); e != nil {
			t.Fatal(e)
		}
	}
	var b bytes.Buffer
	if e := f.Write(&b); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestImportPreviewConfirmRace(t *testing.T) {
	s, uid := testStore(t)
	ctx := context.Background()
	_, e := s.Save(ctx, uid, "", Input{Name: "기존", Phone: "01011112222"})
	if e != nil {
		t.Fatal(e)
	}
	data := workbook(t, [][]any{{"이름", "전화번호", "그룹"}, {"기존중복", "010-1111-2222", "A"}, {"신규", "01033334444", "B"}, {"파일중복", "010-3333-4444", "B"}, {"오류", "abc", ""}, {"경합", "01055556666", "A"}})
	p, e := s.Preview(ctx, uid, messaging.Upload{Name: "목록.xlsx", DataBase64: base64.StdEncoding.EncodeToString(data)})
	if e != nil || p.AddedCount != 2 || p.ExcludedCount != 3 {
		t.Fatal(p, e)
	}
	_, e = s.Save(ctx, uid, "", Input{Name: "중간등록", Phone: "01055556666"})
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, e := s.Confirm(ctx, uid, p.ID)
			if e != nil || got.AddedCount != 1 || got.ExcludedCount != 4 || !got.Confirmed {
				t.Error(got, e)
			}
		}()
	}
	wg.Wait()
	page, e := s.List(ctx, uid, "", "", "", 100)
	if e != nil || len(page.Items) != 3 {
		t.Fatal(page, e)
	}
	if _, e = s.Confirm(ctx, uid+"other", p.ID); e == nil {
		t.Fatal("다른 사용자 import 접근")
	}
}
func TestImportInvalidFiles(t *testing.T) {
	for _, data := range [][]byte{[]byte("bad"), workbook(t, [][]any{{"이름"}, {"A"}})} {
		if _, e := parseXLSX(data); e == nil {
			t.Fatal("잘못된 파일 허용")
		}
	}
}
