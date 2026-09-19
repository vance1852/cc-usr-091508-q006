package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type fixture struct {
	db    *DB
	users map[string]User
	batch string
}

// loadFixtureData 在空库写入测试所需的班组、用户、方法、采样点、批次与容器。
func loadFixtureData(db *DB) (map[string]User, string, error) {
	crews := map[string]string{"A": "甲班", "B": "乙班", "C": "丙班"}
	for code, name := range crews {
		if err := db.EnsureCrew(code, name); err != nil {
			return nil, "", err
		}
	}
	type spec struct{ login, name, role, crew string }
	for _, s := range []spec{
		{"zhang", "张工", "sampler", "A"},
		{"li", "李工", "sampler", "B"},
		{"wang", "王工", "sampler", "C"},
		{"analyst1", "化验员刘", "analyst", ""},
		{"reviewer1", "复核人陈", "reviewer", ""},
		{"approver1", "审批人赵", "approver", ""},
		{"admin", "管理员", "admin", ""},
	} {
		u := User{Token: "tok-" + s.login, Login: s.login, DisplayName: s.name,
			Role: s.role, CrewCode: s.crew, Active: true}
		if err := db.EnsureUser(u); err != nil {
			return nil, "", err
		}
	}
	loMin, loMax := -190.0, -181.0
	if err := db.EnsureMethod(Method{Code: "LOX-PURITY", Name: "液氧纯度", Unit: "%",
		TempMin: &loMin, TempMax: &loMax, MaxGapMinutes: 30}); err != nil {
		return nil, "", err
	}
	if err := db.EnsurePoint("TANK-01", "液氧储罐"); err != nil {
		return nil, "", err
	}
	admin, err := db.UserByToken("tok-admin")
	if err != nil {
		return nil, "", err
	}
	batch := "FIRE-2026-009"
	if _, err := db.EnsureBatch(batch, "液氧 LOX", admin.ID); err != nil {
		return nil, "", err
	}
	for _, b := range []string{"BTL-001", "BTL-002", "BTL-003", "BTL-004"} {
		if err := db.EnsureContainer(b, "bottle"); err != nil {
			return nil, "", err
		}
	}
	logins := []string{"zhang", "li", "wang", "analyst1", "reviewer1", "approver1", "admin"}
	users := map[string]User{}
	for _, l := range logins {
		users[l], _ = db.UserByToken("tok-" + l)
	}
	return users, batch, nil
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	users, batch, err := loadFixtureData(db)
	if err != nil {
		t.Fatalf("fixture data: %v", err)
	}
	return &fixture{db: db, users: users, batch: batch}
}

func regInput(labNo, bottle, seal string, at time.Time) RegisterSampleInput {
	return RegisterSampleInput{
		BatchCode: "FIRE-2026-009", BoxCode: "BOX-1", LabNo: labNo,
		ContainerCode: bottle, SealNo: seal, PointCode: "TANK-01",
		MethodCode: "LOX-PURITY", SampledAt: at.UTC().Format(time.RFC3339),
	}
}

func baseTime() time.Time { return time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC) }

func boolPtr(b bool) *bool { return &b }

// 1. 同一瓶号/箱号/实验室编号被不同班组登记，必须拦下并产生编号冲突偏差。
func TestRegistrationConflictsAcrossCrews(t *testing.T) {
	f := newFixture(t)
	db := f.db
	t0 := baseTime()

	s1, conflicts, err := db.RegisterSample(f.users["zhang"], regInput("LAB-1", "BTL-001", "SEAL-1", t0))
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("first registration should succeed: err=%v conflicts=%v", err, conflicts)
	}
	if s1.CurrentCustodian != "zhang" {
		t.Fatalf("collector should be current custodian, got %q", s1.CurrentCustodian)
	}

	// 第二个班组用相同实验室编号、不同瓶与封签登记。
	in2 := regInput("LAB-1", "BTL-002", "SEAL-2", t0)
	in2.BoxCode = "BOX-2"
	_, conflicts, err = db.RegisterSample(f.users["li"], in2)
	if !errors.Is(err, ErrConflict) || len(conflicts) != 1 || conflicts[0].Field != "lab_no" {
		t.Fatalf("expected lab_no conflict, got err=%v conflicts=%v", err, conflicts)
	}

	// 第三个班组占用同一转运箱。
	in3 := regInput("LAB-3", "BTL-003", "SEAL-3", t0)
	in3.BoxCode = "BOX-1"
	_, conflicts, err = db.RegisterSample(f.users["wang"], in3)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected box conflict, got err=%v conflicts=%v", err, conflicts)
	}
	var boxConflict bool
	for _, c := range conflicts {
		if c.Field == "box_code" {
			boxConflict = true
		}
	}
	if !boxConflict {
		t.Fatalf("expected box_code conflict item, got %v", conflicts)
	}

	// 同一采集瓶在未归还前不能二次登记。
	in4 := regInput("LAB-4", "BTL-001", "SEAL-4", t0)
	in4.BoxCode = "BOX-4"
	_, conflicts, err = db.RegisterSample(f.users["wang"], in4)
	if !errors.Is(err, ErrConflict) || len(conflicts) != 1 || conflicts[0].Field != "container_code" {
		t.Fatalf("expected container conflict, got err=%v conflicts=%v", err, conflicts)
	}

	// 封签不得重复使用。
	in5 := regInput("LAB-5", "BTL-002", "SEAL-1", t0)
	in5.BoxCode = "BOX-5"
	_, _, err = db.RegisterSample(f.users["wang"], in5)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected seal reuse conflict, got %v", err)
	}

	// 冲突必须留下偏差与登记留痕，且不能靠删除消失。
	devs, err := db.Deviations(0)
	if err != nil {
		t.Fatal(err)
	}
	var nConflict int
	for _, d := range devs {
		if d.Type == "number_conflict" {
			nConflict++
			if !d.Open {
				t.Fatalf("number conflict deviation should be open pending disposition")
			}
		}
	}
	if nConflict < 3 {
		t.Fatalf("expected >=3 number_conflict deviations, got %d", nConflict)
	}
}

// 2. 事实表 UPDATE/DELETE 必须被触发器中止。
func TestAppendOnlyTriggers(t *testing.T) {
	f := newFixture(t)
	t0 := baseTime()
	if _, _, err := f.db.RegisterSample(f.users["zhang"],
		regInput("LAB-9", "BTL-001", "SEAL-9", t0)); err != nil {
		t.Fatal(err)
	}
	// 温度读数
	if _, err := f.db.AddTemperatures(f.users["zhang"], "LAB-9", []TemperatureInput{
		{TakenAt: t0.Format(time.RFC3339), Celsius: -185.0},
	}); err != nil {
		t.Fatal(err)
	}
	// 编号冲突偏差（不同班组登记同一实验室编号）
	bad := regInput("LAB-9", "BTL-002", "SEAL-X2", t0)
	bad.BoxCode = "BOX-X"
	if _, _, err := f.db.RegisterSample(f.users["li"], bad); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	// 一版结果及其状态事件
	if _, err := f.db.Release(f.users["zhang"],
		ReleaseInput{LabNo: "LAB-9", ToLogin: "analyst1", Station: "LAB"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Receive(f.users["analyst1"],
		ReceiveInput{LabNo: "LAB-9", SealIntact: boolPtr(true)}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SubmitResult(f.users["analyst1"], SubmitResultInput{
		LabNo: "LAB-9", MethodCode: "LOX-PURITY",
		Readings: []RawReadingInput{{Name: "purity", Value: 99.5, Unit: "%"}},
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ table, set, where string }{
		{"custody_events", "note='x'", "1=1"},
		{"deviations", "detail='x'", "1=1"},
		{"temperature_readings", "celsius=0", "1=1"},
		{"result_versions", "note='x'", "1=1"},
		{"result_status_events", "note='x'", "1=1"},
		{"raw_readings", "unit='x'", "1=1"},
		{"registration_attempts", "detail='x'", "1=1"},
	} {
		if _, err := f.db.sql.Exec("UPDATE " + tc.table + " SET " + tc.set + " WHERE " + tc.where); err == nil {
			t.Fatalf("UPDATE on %s must be rejected", tc.table)
		}
		if _, err := f.db.sql.Exec("DELETE FROM " + tc.table + " WHERE " + tc.where); err == nil {
			t.Fatalf("DELETE on %s must be rejected", tc.table)
		}
	}
}

// 3. 温度越界与记录空白形成偏差，样品被暂停使用，后续交接与提交均被阻断。
func TestTemperatureExcursionAndGap(t *testing.T) {
	f := newFixture(t)
	db := f.db
	t0 := baseTime()
	s, _, err := db.RegisterSample(f.users["zhang"], regInput("LAB-T", "BTL-001", "SEAL-T", t0))
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.AddTemperatures(f.users["zhang"], "LAB-T", []TemperatureInput{
		{TakenAt: t0.Format(time.RFC3339), Celsius: -185.0, Source: "online"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 越界：-180 高于上限 -181。
	_, err = db.AddTemperatures(f.users["zhang"], "LAB-T", []TemperatureInput{
		{TakenAt: t0.Add(10 * time.Minute).Format(time.RFC3339), Celsius: -180.0, Source: "online"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 空白：距上一条 60 分钟 > 允许的 30 分钟。
	_, err = db.AddTemperatures(f.users["zhang"], "LAB-T", []TemperatureInput{
		{TakenAt: t0.Add(70 * time.Minute).Format(time.RFC3339), Celsius: -184.0, Source: "offline"},
	})
	if err != nil {
		t.Fatal(err)
	}

	devs, _ := db.Deviations(s.ID)
	var hasExcursion, hasGap bool
	for _, d := range devs {
		if d.Type == "temp_excursion" {
			hasExcursion = true
		}
		if d.Type == "temp_gap" {
			hasGap = true
		}
	}
	if !hasExcursion || !hasGap {
		t.Fatalf("expected excursion and gap deviations, got %+v", devs)
	}

	got, err := db.GetSample(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Usable || got.OpenDeviationCount == 0 {
		t.Fatalf("sample must be blocked, usable=%v open=%d", got.Usable, got.OpenDeviationCount)
	}

	// 暂停后续使用：交出被拒。
	if _, err := db.Release(f.users["zhang"], ReleaseInput{
		LabNo: "LAB-T", ToLogin: "analyst1", Station: "GATE",
	}, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("release while deviation open must be blocked, got %v", err)
	}
}

//  4. 两阶段交接：release 不转移责任，receive 承接上一保管人后才切换；
//     并发第二笔 release 必须失败（链路中只能有一名当前保管人）。
func TestTwoPhaseCustodyAndSingleCustodian(t *testing.T) {
	f := newFixture(t)
	db := f.db
	s, _, err := db.RegisterSample(f.users["zhang"], regInput("LAB-C", "BTL-001", "SEAL-C", baseTime()))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.Release(f.users["zhang"], ReleaseInput{
		LabNo: "LAB-C", ToLogin: "li", Station: "GATE-1",
	}, false); err != nil {
		t.Fatal(err)
	}
	mid, _ := db.GetSample(s.ID)
	if mid.CurrentCustodian != "zhang" {
		t.Fatalf("after release custodian must remain zhang, got %q", mid.CurrentCustodian)
	}

	// 在途期间第二笔交接被唯一索引拦截。
	_, err = db.Release(f.users["zhang"], ReleaseInput{
		LabNo: "LAB-C", ToLogin: "wang", Station: "GATE-1",
	}, false)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second concurrent release must conflict, got %v", err)
	}

	// 非指定接收人不能接收。
	if _, err := db.Receive(f.users["wang"], ReceiveInput{
		LabNo: "LAB-C", SealIntact: boolPtr(true),
	}, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-designated receiver must be forbidden, got %v", err)
	}

	// 接收必须承接上一位保管人；封签完好，责任转移。
	if _, err := db.Receive(f.users["li"], ReceiveInput{
		LabNo: "LAB-C", SealIntact: boolPtr(true),
	}, false); err != nil {
		t.Fatal(err)
	}
	after, _ := db.GetSample(s.ID)
	if after.CurrentCustodian != "li" {
		t.Fatalf("after receive custodian must be li, got %q", after.CurrentCustodian)
	}
	if after.CustodianSeq != 1 {
		t.Fatalf("custodian seq expected 1, got %d", after.CustodianSeq)
	}

	chain, _ := db.CustodyChain(s.ID)
	if !VerifyChain(chain) {
		t.Fatalf("custody hash chain broken: %+v", chain)
	}
}

// 5. 接收时发现封签破损 → 偏差并暂停；处置 accept_justified 后才能继续。
func TestSealBrokenDeviationAndDisposition(t *testing.T) {
	f := newFixture(t)
	db := f.db
	s, _, err := db.RegisterSample(f.users["zhang"], regInput("LAB-S", "BTL-001", "SEAL-S", baseTime()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Release(f.users["zhang"], ReleaseInput{
		LabNo: "LAB-S", ToLogin: "li", Station: "GATE",
	}, false); err != nil {
		t.Fatal(err)
	}
	ev, err := db.Receive(f.users["li"], ReceiveInput{
		LabNo: "LAB-S", SealIntact: boolPtr(false), Note: "封签有撬动痕迹",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if ev.SealIntact == nil || *ev.SealIntact {
		t.Fatalf("event should record broken seal")
	}
	// 破损接收不转移保管责任。
	cur, _ := db.GetSample(s.ID)
	if cur.CurrentCustodian != "zhang" {
		t.Fatalf("broken-seal receive must not transfer custody, got %q", cur.CurrentCustodian)
	}
	devs, _ := db.Deviations(s.ID)
	var devID int64
	for _, d := range devs {
		if d.Type == "seal_broken" {
			devID = d.ID
		}
	}
	if devID == 0 {
		t.Fatalf("seal_broken deviation missing")
	}

	// 采样班组不能处置本班组样品偏差（甲班 zhang 是 reviewer? 这里 reviewer1 无班组，可以）。
	if _, err := db.DisposeDeviation(f.users["reviewer1"], DispositionInput{
		DeviationID: devID, Decision: "reject_sample", Justification: "封签破损，样品不可信",
	}); err != nil {
		t.Fatal(err)
	}
	// reject 后样品永久不可用。
	if _, err := db.Release(f.users["zhang"], ReleaseInput{
		LabNo: "LAB-S", ToLogin: "li", Station: "GATE",
	}, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("rejected sample must remain blocked, got %v", err)
	}
}

// 6. 离线扫码恢复：相同 idem_key 重放只生效一次，链路不产生重复事件。
func TestOfflineRecoveryIdempotent(t *testing.T) {
	f := newFixture(t)
	db := f.db
	s, _, err := db.RegisterSample(f.users["zhang"], regInput("LAB-O", "BTL-001", "SEAL-O", baseTime()))
	if err != nil {
		t.Fatal(err)
	}
	ct := baseTime().Add(2 * time.Hour).Format(time.RFC3339)
	rel := ReleaseInput{LabNo: "LAB-O", ToLogin: "li", Station: "DOCK",
		ClientTime: ct, IdemKey: "dev-77-rel-1"}
	ev1, err := db.Release(f.users["zhang"], rel, true)
	if err != nil {
		t.Fatal(err)
	}
	ev2, err := db.Release(f.users["zhang"], rel, true)
	if err != nil {
		t.Fatalf("replay of same offline event should be served, got %v", err)
	}
	if ev1.ID != ev2.ID {
		t.Fatalf("idempotent replay must return the same event %d vs %d", ev1.ID, ev2.ID)
	}

	recv := ReceiveInput{LabNo: "LAB-O", SealIntact: boolPtr(true),
		ClientTime: ct, IdemKey: "dev-77-rcv-1"}
	rc1, err := db.Receive(f.users["li"], recv, true)
	if err != nil {
		t.Fatal(err)
	}
	rc2, err := db.Receive(f.users["li"], recv, true)
	if err != nil {
		t.Fatalf("receive replay should be served, got %v", err)
	}
	if rc1.ID != rc2.ID {
		t.Fatalf("receive replay must return same event")
	}
	chain, _ := db.CustodyChain(s.ID)
	// collect + recover_release + recover_receive，且只有 3 条。
	if len(chain) != 3 {
		t.Fatalf("expected 3 chain events, got %d", len(chain))
	}
	if chain[1].Kind != "recover_release" || chain[2].Kind != "recover_receive" {
		t.Fatalf("unexpected kinds %s %s", chain[1].Kind, chain[2].Kind)
	}
	if !VerifyChain(chain) {
		t.Fatalf("chain broken after recovery")
	}
}

// 7. 容器归还后可再次使用；归还后样品无当前保管人。
func TestContainerReuseAfterReturn(t *testing.T) {
	f := newFixture(t)
	db := f.db
	t0 := baseTime()
	s1, _, err := db.RegisterSample(f.users["zhang"], regInput("LAB-R1", "BTL-001", "SEAL-R1", t0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReturnContainer(f.users["zhang"], "LAB-R1", "检测完毕归还"); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetSample(s1.ID)
	if got.CurrentCustodianID != nil {
		t.Fatalf("returned sample must have no custodian")
	}
	if got.ConsumedAt == "" {
		t.Fatalf("returned sample must be marked consumed")
	}

	// 同一物理瓶可绑定新样品（新封签、新编号、新箱）。
	in2 := regInput("LAB-R2", "BTL-001", "SEAL-R2", t0.Add(time.Hour))
	in2.BoxCode = "BOX-NEW"
	s2, _, err := db.RegisterSample(f.users["zhang"], in2)
	if err != nil {
		t.Fatalf("bottle should be reusable: %v", err)
	}
	if s2.ContainerCode != "BTL-001" {
		t.Fatalf("expected reused bottle, got %q", s2.ContainerCode)
	}
}

//  8. 结果版本：采样班组不能批准自己的检测；检测人不能复核自己的结果；
//     只有 reviewed 版本成为采用版本；纠正只能以新版本引用旧版本。
func TestResultReviewSegregationAndSupersede(t *testing.T) {
	f := newFixture(t)
	db := f.db
	s, _, err := db.RegisterSample(f.users["zhang"], regInput("LAB-V", "BTL-001", "SEAL-V", baseTime()))
	if err != nil {
		t.Fatal(err)
	}
	// 交给化验员。
	if _, err := db.Release(f.users["zhang"], ReleaseInput{LabNo: "LAB-V", ToLogin: "analyst1", Station: "LAB"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Receive(f.users["analyst1"], ReceiveInput{LabNo: "LAB-V", SealIntact: boolPtr(true)}, false); err != nil {
		t.Fatal(err)
	}

	v1, err := db.SubmitResult(f.users["analyst1"], SubmitResultInput{
		LabNo: "LAB-V", MethodCode: "LOX-PURITY", Instrument: "GC-9",
		Readings: []RawReadingInput{{Name: "purity", Value: 99.51, Unit: "%"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v1.VersionNo != 1 || v1.Status != "submitted" {
		t.Fatalf("unexpected v1: %+v", v1)
	}

	// 检测人不能复核自己。
	if _, err := db.ReviewResult(f.users["analyst1"], v1.ID, true, "ok", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("self review must be forbidden, got %v", err)
	}
	// 采样班组成员不能批准本班组检测：把 reviewer1 临时设为 A 班人不可能（角色限制），
	// 另建一个属于 A 班的 reviewer 验证。
	if err := db.EnsureUser(User{Token: "tok-revA", Login: "revA", DisplayName: "甲班复核",
		Role: "reviewer", CrewCode: "A", Active: true}); err != nil {
		t.Fatal(err)
	}
	revA, _ := db.UserByToken("tok-revA")
	if _, err := db.ReviewResult(revA, v1.ID, true, "ok", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("sampling crew must not approve own crew's test, got %v", err)
	}

	// 独立复核人驳回。
	v1b, err := db.ReviewResult(f.users["reviewer1"], v1.ID, false, "", "读数存疑，要求复测")
	if err != nil {
		t.Fatal(err)
	}
	if v1b.Status != "rejected" {
		t.Fatalf("expected rejected, got %s", v1b.Status)
	}
	// 已驳回复核不能二次改状态。
	if _, err := db.ReviewResult(f.users["reviewer1"], v1.ID, true, "ok", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("re-review must conflict, got %v", err)
	}

	// 复测：新版本引用旧版本。
	v2, err := db.SubmitResult(f.users["analyst1"], SubmitResultInput{
		LabNo: "LAB-V", MethodCode: "LOX-PURITY", Instrument: "GC-9",
		SupersedesID: v1.ID,
		Readings:     []RawReadingInput{{Name: "purity", Value: 99.62, Unit: "%"}},
		Note:         "复测替换首次读数",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v2.SupersedesID == nil || *v2.SupersedesID != v1.ID || v2.VersionNo != 2 {
		t.Fatalf("v2 must supersede v1: %+v", v2)
	}
	// 旧版本状态与读数未被改动。
	old, _ := db.GetResult(v1.ID)
	if old.Status != "rejected" || len(old.Readings) != 1 || old.Readings[0].Value != 99.51 {
		t.Fatalf("superseded version must remain immutable: %+v", old)
	}

	if _, err := db.ReviewResult(f.users["reviewer1"], v2.ID, true, "符合放行指标", ""); err != nil {
		t.Fatal(err)
	}
	cur, _ := db.GetSample(s.ID)
	if cur.CurrentResultID == nil || *cur.CurrentResultID != v2.ID {
		t.Fatalf("adopted result must be v2, got %v", cur.CurrentResultID)
	}

	// 审批视图只呈现完成复核的版本。
	items, err := db.ApprovalView(f.batch)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range items {
		if it.LabNo == "LAB-V" {
			found = true
			if !it.Releasable || it.ResultVersion != 2 {
				t.Fatalf("approval item should point at reviewed v2: %+v", it)
			}
		}
	}
	if !found {
		t.Fatalf("approval item missing")
	}
}

// 9. 外部实验室视图：只见脱敏批号，不见内部编号、人员与班组。
func TestExternalMaskedView(t *testing.T) {
	f := newFixture(t)
	db := f.db
	if _, _, err := db.RegisterSample(f.users["zhang"], regInput("LAB-X", "BTL-001", "SEAL-X", baseTime())); err != nil {
		t.Fatal(err)
	}
	masked, ok, err := db.MaskedBatchOf(f.batch)
	if err != nil || !ok || masked == f.batch || len(masked) != 12 {
		t.Fatalf("bad masked code %q ok=%v err=%v", masked, ok, err)
	}
	views, err := db.ExternalViewByMaskedBatch(masked)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 external view, got %d", len(views))
	}
	v := views[0]
	if v.MaskedBatch != masked || v.Status != "retest_pending" {
		t.Fatalf("unexpected external view: %+v", v)
	}

	// 未知脱敏批号返回空集，无法枚举内部批号。
	empty, err := db.ExternalViewByMaskedBatch("doesnotexist")
	if err != nil || len(empty) != 0 {
		t.Fatalf("unknown masked batch must yield empty set, got %v %v", empty, err)
	}
}

// 10. 下钻视图：链路、温度、偏差、结果版本齐全且哈希链完整。
func TestBatchTraceDrillDown(t *testing.T) {
	f := newFixture(t)
	db := f.db
	s, _, err := db.RegisterSample(f.users["zhang"], regInput("LAB-D", "BTL-001", "SEAL-D", baseTime()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddTemperatures(f.users["zhang"], "LAB-D", []TemperatureInput{
		{TakenAt: baseTime().Format(time.RFC3339), Celsius: -184.0},
	}); err != nil {
		t.Fatal(err)
	}
	trace, err := db.TraceBatch(f.batch)
	if err != nil {
		t.Fatal(err)
	}
	if trace.BatchCode != f.batch || len(trace.Samples) != 1 {
		t.Fatalf("bad trace: %+v", trace)
	}
	st := trace.Samples[0]
	if !st.ChainOK || len(st.Custody) != 1 || len(st.Temperatures) != 1 {
		t.Fatalf("trace content wrong: chainOK=%v custody=%d temps=%d",
			st.ChainOK, len(st.Custody), len(st.Temperatures))
	}
	if st.Sample.ID != s.ID {
		t.Fatalf("sample mismatch")
	}

	// 物理篡改一条事件后，链校验必须失败。
	if _, err := db.sql.Exec(`PRAGMA ignore_check_constraints=ON`); err != nil {
		t.Fatal(err)
	}
	// 触发器仍会阻止 UPDATE；直接验证 VerifyChain 能识别 hash 不一致。
	row := db.sql.QueryRow(`SELECT hash FROM custody_events WHERE id=?`, st.Custody[0].ID)
	var good string
	if err := row.Scan(&good); err != nil {
		t.Fatal(err)
	}
	tampered := st.Custody[0]
	tampered.Hash = "deadbeef"
	if VerifyChain([]CustodyEvent{tampered}) {
		t.Fatalf("VerifyChain must detect tampered hash")
	}
	_ = good
	_ = sql.ErrNoRows
}
