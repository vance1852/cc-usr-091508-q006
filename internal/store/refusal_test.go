package store

import "testing"

// 13. 拒收且封签破损：责任不转移，同时形成封签偏差。
func TestRefusalWithBrokenSeal(t *testing.T) {
	f := newFixture(t)
	db := f.db
	s, _, err := db.RegisterSample(f.users["zhang"],
		regInput("LAB-RB", "BTL-001", "SEAL-RB", baseTime()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Release(f.users["zhang"],
		ReleaseInput{LabNo: "LAB-RB", ToLogin: "li", Station: "GATE"}, false); err != nil {
		t.Fatal(err)
	}
	ev, err := db.Receive(f.users["li"], ReceiveInput{
		LabNo: "LAB-RB", SealIntact: boolPtr(false), RejectTransfer: true,
		Note: "拒收，封签有撬动痕迹",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if ev.SealIntact == nil || *ev.SealIntact {
		t.Fatalf("event must record broken seal")
	}
	cur, _ := db.GetSample(s.ID)
	if cur.CurrentCustodian != "zhang" {
		t.Fatalf("refusal must not move custody, got %q", cur.CurrentCustodian)
	}
	devs, _ := db.Deviations(s.ID)
	var hasBroken bool
	for _, d := range devs {
		if d.Type == "seal_broken" && d.Open {
			hasBroken = true
		}
	}
	if !hasBroken {
		t.Fatalf("refusal with broken seal must open seal_broken deviation: %+v", devs)
	}
	// 偏差未处置，样品被暂停。
	if cur.Usable {
		t.Fatalf("sample must be blocked pending deviation disposition")
	}
}
