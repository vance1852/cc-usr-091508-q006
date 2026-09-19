package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

//  11. 两名保管人（同一当前保管人的两次请求）同时发起交接，只能成功一笔；
//     同一接收人并发确认也只能产生一次责任转移。
func TestConcurrentTransfersSingleWinner(t *testing.T) {
	f := newFixture(t)
	db := f.db
	if _, _, err := db.RegisterSample(f.users["zhang"],
		regInput("LAB-Z", "BTL-001", "SEAL-Z", baseTime())); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.Release(f.users["zhang"], ReleaseInput{
				LabNo: "LAB-Z", ToLogin: "li", Station: "GATE",
			}, false)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	var ok, conflict int
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrConflict):
			conflict++
		default:
			t.Fatalf("unexpected release error: %v", err)
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("expected exactly 1 success and %d conflicts, got ok=%d conflict=%d", n-1, ok, conflict)
	}

	// 多个接收人并发确认，只有一笔成功，保管人仍唯一。
	rcResults := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.Receive(f.users["li"], ReceiveInput{
				LabNo: "LAB-Z", SealIntact: boolPtr(true),
			}, false)
			rcResults <- err
		}()
	}
	wg.Wait()
	close(rcResults)
	ok, conflict = 0, 0
	for err := range rcResults {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrConflict), errors.Is(err, ErrForbidden):
			conflict++
		default:
			t.Fatalf("unexpected receive error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("expected exactly 1 successful receive, got %d (conflicts %d)", ok, conflict)
	}
	s, _ := db.GetSampleByLabNo("LAB-Z")
	if s.CurrentCustodian != "li" {
		t.Fatalf("sole custodian must be li, got %q", s.CurrentCustodian)
	}
	if s.CustodianSeq != 1 {
		t.Fatalf("custodian seq must be 1, got %d", s.CustodianSeq)
	}
}

//  12. 处置 accept_justified 后偏差关闭、样品恢复可用；retest 不自动恢复，
//     必须等复测结果完成复核才能放行。
func TestDispositionAcceptJustifiedRestoresUsability(t *testing.T) {
	f := newFixture(t)
	db := f.db
	s, _, err := db.RegisterSample(f.users["zhang"],
		regInput("LAB-J", "BTL-001", "SEAL-J", baseTime()))
	if err != nil {
		t.Fatal(err)
	}
	// 制造一条温度越界偏差。
	if _, err := db.AddTemperatures(f.users["zhang"], "LAB-J", []TemperatureInput{
		{TakenAt: baseTime().Format(time.RFC3339), Celsius: -180.0},
	}); err != nil {
		t.Fatal(err)
	}
	devs, _ := db.Deviations(s.ID)
	if len(devs) != 1 || !devs[0].Open {
		t.Fatalf("expected 1 open deviation, got %+v", devs)
	}
	if _, err := db.DisposeDeviation(f.users["reviewer1"], DispositionInput{
		DeviationID: devs[0].ID, Decision: "accept_justified",
		Justification: "短时开罐取样导致，与保温曲线比对可接受",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetSample(s.ID)
	if !got.Usable || got.OpenDeviationCount != 0 {
		t.Fatalf("sample should be usable after justified acceptance, usable=%v open=%d",
			got.Usable, got.OpenDeviationCount)
	}
	// 处置不可改写：再次处置必须失败。
	if _, err := db.DisposeDeviation(f.users["reviewer1"], DispositionInput{
		DeviationID: devs[0].ID, Decision: "reject_sample", Justification: "改判",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second disposition must conflict, got %v", err)
	}
}
