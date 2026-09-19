package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

type fixture struct {
	admin, samplerA, samplerB, courier, analyst, analystB, reviewer, sameTeamReviewer, approver, external Person
	batch, point, method, container, bottleB                                                              string
}

func setupFixture(t *testing.T, st *Store) fixture {
	t.Helper()
	mk := func(id, name, team, role string) Person {
		p, err := st.CreatePerson(Person{ID: id, Name: name, Team: team, Role: role})
		if err != nil {
			t.Fatalf("create person %s: %v", id, err)
		}
		if _, err := st.IssueToken(id, "tok-"+id); err != nil {
			t.Fatalf("token %s: %v", id, err)
		}
		return p
	}
	f := fixture{
		admin:            mk("admin", "管理员", "管理组", "lab_admin"),
		samplerA:         mk("sa", "采样甲", "采样一组", "sampler"),
		samplerB:         mk("sb", "采样乙", "采样一组", "sampler"),
		courier:          mk("co", "押运", "转运组", "courier"),
		analyst:          mk("an", "化验甲", "化验一组", "analyst"),
		analystB:         mk("an2", "化验乙", "化验二组", "analyst"),
		reviewer:         mk("rv", "复核", "质量组", "reviewer"),
		sameTeamReviewer: mk("rv2", "复核二", "采样一组", "reviewer"),
		approver:         mk("ap", "审批", "指挥部", "approver"),
		external:         mk("ex", "外部", "外部实验室", "external_lab"),
	}
	b, err := st.CreateBatch(Batch{ID: "b1", Name: "试车一号", Product: "LOX"}, f.admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.batch = b.ID
	if f.point, err = st.CreateSamplingPoint(f.batch, "加注口"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.SetTempLimits(f.batch, -196.0, -183.0); err != nil {
		t.Fatal(err)
	}
	if f.method, err = st.CreateMethod("m1", "气相色谱法", ""); err != nil {
		t.Fatal(err)
	}
	if f.container, err = st.RegisterContainer("c1", "bottle"); err != nil {
		t.Fatal(err)
	}
	if f.bottleB, err = st.RegisterContainer("c2", "bottle"); err != nil {
		t.Fatal(err)
	}
	return f
}

func collectOK(t *testing.T, st *Store, f fixture, code, container string, seal bool) CollectResult {
	t.Helper()
	res, err := st.Collect(CollectInput{
		PublicCode: code, BatchID: f.batch, PointID: f.point,
		CollectorID: f.samplerA.ID, ContainerID: container,
		SealNo: "SEAL-1", SealIntact: seal, OpID: "op-collect-" + code,
	})
	if err != nil {
		t.Fatalf("collect %s: %v", code, err)
	}
	return res
}

// 全链路 happy path：采集→交出→接收→提交→跨班组复核→采用。
func TestHappyPath(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)
	c := collectOK(t, st, f, "PUB-001", f.container, true)

	rel, err := st.Release(ReleaseInput{
		SampleID: c.SampleID, OpID: "op-rel-1", FromPersonID: f.samplerA.ID,
		ToPersonID: f.courier.ID, SealIntact: true, ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	recv, err := st.Receive(ReceiveInput{
		TransferID: rel.TransferID, OpID: "op-rcv-1", ReceiverID: f.courier.ID,
		ReleaseCode: rel.ReleaseCode, SealIntact: true, SealNo: "SEAL-1",
		HasTemp: true, TempMin: -190, TempMax: -186, ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if recv.HolderID != f.courier.ID || recv.Version != 2 || recv.Hold {
		t.Fatalf("receive result wrong: %+v", recv)
	}

	// 押运员交给化验员。
	rel2, err := st.Release(ReleaseInput{
		SampleID: c.SampleID, OpID: "op-rel-2", FromPersonID: f.courier.ID,
		ToPersonID: f.analyst.ID, SealIntact: true,
	})
	if err != nil {
		t.Fatalf("release2: %v", err)
	}
	if _, err := st.Receive(ReceiveInput{
		TransferID: rel2.TransferID, OpID: "op-rcv-2", ReceiverID: f.analyst.ID,
		ReleaseCode: rel2.ReleaseCode, SealIntact: true, SealNo: "SEAL-1",
		HasTemp: true, TempMin: -192, TempMax: -188,
	}); err != nil {
		t.Fatalf("receive2: %v", err)
	}

	r1, err := st.SubmitResult(ResultInput{
		SampleID: c.SampleID, MethodID: f.method, RawReading: "99.5% O2",
		Purity: 99.5, AnalystID: f.analyst.ID, OpID: "op-res-1",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if r1.Version != 1 {
		t.Fatalf("want version 1, got %d", r1.Version)
	}
	if _, err := st.ReviewResult(ReviewInput{
		ResultID: r1.ID, ReviewerID: f.reviewer.ID, Approve: true, OpID: "op-rev-1",
	}); err != nil {
		t.Fatalf("review: %v", err)
	}
	if _, err := st.AdoptResult(AdoptInput{
		SampleID: c.SampleID, Version: 1, ActorID: f.admin.ID, OpID: "op-adopt-1",
	}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	view, err := st.SampleStatus(c.SampleID)
	if err != nil {
		t.Fatal(err)
	}
	if view.AdoptedResult != 1 || view.CurrentHolder.ID != f.analyst.ID || view.Hold {
		t.Fatalf("unexpected final view: %+v", view)
	}
}

// 接收时封签破损：生成偏差、样品暂停，但保管人正常切换。
func TestSealBrokenDeviation(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)
	c := collectOK(t, st, f, "PUB-002", f.container, true)
	rel, err := st.Release(ReleaseInput{
		SampleID: c.SampleID, OpID: "op-rel", FromPersonID: f.samplerA.ID,
		ToPersonID: f.courier.ID, SealIntact: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	recv, err := st.Receive(ReceiveInput{
		TransferID: rel.TransferID, OpID: "op-rcv", ReceiverID: f.courier.ID,
		ReleaseCode: rel.ReleaseCode, SealIntact: false, HasTemp: true, TempMin: -190, TempMax: -186,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !recv.Hold || len(recv.DeviationIDs) != 1 {
		t.Fatalf("expect hold with 1 deviation, got %+v", recv)
	}
	if recv.HolderID != f.courier.ID {
		t.Fatal("保管人仍应切换给接收人")
	}
	// 暂停期间禁止交接、消耗、采用。
	if _, err := st.Release(ReleaseInput{
		SampleID: c.SampleID, OpID: "op-rel-x", FromPersonID: f.courier.ID,
		ToPersonID: f.analyst.ID, SealIntact: false,
	}); err == nil {
		t.Fatal("暂停样品不应允许交出")
	}
	if _, err := st.Consume(c.SampleID, f.courier.ID, "op-consume-x", ""); err == nil {
		t.Fatal("暂停样品不应允许消耗")
	}
	r1, _ := st.SubmitResult(ResultInput{
		SampleID: c.SampleID, MethodID: f.method, RawReading: "x",
		Purity: 1, AnalystID: f.analyst.ID, OpID: "op-res-x",
	})
	st.ReviewResult(ReviewInput{ResultID: r1.ID, ReviewerID: f.reviewer.ID, Approve: true, OpID: "op-rev-x"})
	if _, err := st.AdoptResult(AdoptInput{
		SampleID: c.SampleID, Version: 1, ActorID: f.admin.ID, OpID: "op-adopt-x",
	}); err == nil {
		t.Fatal("暂停样品不应允许采用结论")
	}
	// 处置关闭后解除暂停。
	if _, err := st.Dispose(DispositionInput{
		DeviationID: recv.DeviationIDs[0], ActorID: f.admin.ID,
		Action: "corrective_action", Note: "核查封签", OpID: "op-disp-1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Dispose(DispositionInput{
		DeviationID: recv.DeviationIDs[0], ActorID: f.admin.ID,
		Action: "close", Note: "更换封签", OpID: "op-disp-2",
	}); err != nil {
		t.Fatalf("close: %v", err)
	}
	view, _ := st.SampleStatus(c.SampleID)
	if view.Hold {
		t.Fatal("偏差结案后 hold 应解除")
	}
}

// 温度越界与温度空白。
func TestTemperatureDeviations(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)

	// 越界
	c1 := collectOK(t, st, f, "PUB-T1", f.container, true)
	rel, _ := st.Release(ReleaseInput{
		SampleID: c1.SampleID, OpID: "rel-t1", FromPersonID: f.samplerA.ID,
		ToPersonID: f.courier.ID, SealIntact: true,
	})
	recv, err := st.Receive(ReceiveInput{
		TransferID: rel.TransferID, OpID: "rcv-t1", ReceiverID: f.courier.ID,
		ReleaseCode: rel.ReleaseCode, SealIntact: true,
		HasTemp: true, TempMin: -170, TempMax: -160, // 完全高于上限 -183
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recv.DeviationIDs) != 1 {
		t.Fatalf("expect 1 temp_excursion, got %d", len(recv.DeviationIDs))
	}

	// 空白
	c2 := collectOK(t, st, f, "PUB-T2", f.bottleB, true)
	rel2, _ := st.Release(ReleaseInput{
		SampleID: c2.SampleID, OpID: "rel-t2", FromPersonID: f.samplerA.ID,
		ToPersonID: f.courier.ID, SealIntact: true,
	})
	recv2, err := st.Receive(ReceiveInput{
		TransferID: rel2.TransferID, OpID: "rcv-t2", ReceiverID: f.courier.ID,
		ReleaseCode: rel2.ReleaseCode, SealIntact: true, HasTemp: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recv2.DeviationIDs) != 1 {
		t.Fatalf("expect 1 temp_gap, got %d", len(recv2.DeviationIDs))
	}
}

// 编号冲突形成偏差且登记不覆盖；同 op 重放返回同一偏差。
func TestCodeConflict(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)
	c := collectOK(t, st, f, "PUB-C1", f.container, true)

	in := RegisterCodeInput{
		ContainerID: f.container, Scope: "collection", Code: "LOX-B-001",
		SampleID: c.SampleID, ActorID: f.samplerA.ID, OpID: "op-reg-1",
	}
	if _, err := st.RegisterContainerCode(in); err != nil {
		t.Fatal(err)
	}
	// 另一个班组用同编号登记另一个容器：登记被拒但形成偏差并暂停样品。
	in2 := RegisterCodeInput{
		ContainerID: f.bottleB, Scope: "collection", Code: "LOX-B-001",
		SampleID: c.SampleID, ActorID: f.samplerB.ID, OpID: "op-reg-2",
	}
	res2, err := st.RegisterContainerCode(in2)
	if err != nil || !res2.Conflict || res2.DeviationID == 0 {
		t.Fatalf("重复编号登记必须返回冲突偏差，got res=%+v err=%v", res2, err)
	}
	// 重放第一次登记：同 op 同载荷，返回原登记而非报错。
	rep, err := st.RegisterContainerCode(in)
	if err != nil {
		t.Fatalf("replay should succeed: %v", err)
	}
	if rep.Conflict || rep.RegistrationID == "" {
		t.Fatalf("replay result wrong: %+v", rep)
	}
	// 重放冲突登记：返回同一偏差号。
	rep2, err := st.RegisterContainerCode(in2)
	if err != nil || !rep2.Conflict || rep2.DeviationID != res2.DeviationID {
		t.Fatalf("replay conflict should report same deviation, got %+v err=%v", rep2, err)
	}
	view, _ := st.SampleStatus(c.SampleID)
	if !view.Hold || len(view.Deviations) != 1 || view.Deviations[0].Kind != "code_conflict" {
		t.Fatalf("样品应因编号冲突暂停: %+v", view.Deviations)
	}
}

// 采样班组不能批准自己的检测；跨班组复核通过。
func TestReviewDutySeparation(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)
	c := collectOK(t, st, f, "PUB-D1", f.container, true)
	r1, err := st.SubmitResult(ResultInput{
		SampleID: c.SampleID, MethodID: f.method, RawReading: "x",
		Purity: 99, AnalystID: f.analyst.ID, OpID: "res-d1",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.ReviewResult(ReviewInput{
		ResultID: r1.ID, ReviewerID: f.sameTeamReviewer.ID, Approve: true, OpID: "rev-d1-a",
	})
	var be *BizError
	if !errors.As(err, &be) || be.Code != ErrDuty {
		t.Fatalf("同班组复核应被 duty_separation 拒绝，got %v", err)
	}
	if _, err := st.ReviewResult(ReviewInput{
		ResultID: r1.ID, ReviewerID: f.reviewer.ID, Approve: true, OpID: "rev-d1-b",
	}); err != nil {
		t.Fatalf("跨班组复核应通过: %v", err)
	}
}

// 复测版本引用旧版本；旧版本不被修改；采用 v2。
func TestRetestVersionChain(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)
	c := collectOK(t, st, f, "PUB-R1", f.container, true)

	r1, err := st.SubmitResult(ResultInput{
		SampleID: c.SampleID, MethodID: f.method, RawReading: "99.0",
		Purity: 99.0, AnalystID: f.analyst.ID, OpID: "res-r1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReviewResult(ReviewInput{
		ResultID: r1.ID, ReviewerID: f.reviewer.ID, Approve: true, OpID: "rev-r1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdoptResult(AdoptInput{
		SampleID: c.SampleID, Version: 1, ActorID: f.admin.ID, OpID: "adopt-r1",
	}); err != nil {
		t.Fatal(err)
	}
	// 复测纠正：v2 引用 v1。
	r2, err := st.SubmitResult(ResultInput{
		SampleID: c.SampleID, MethodID: f.method, RawReading: "99.7",
		Purity: 99.7, SupersedesVersion: 1, AnalystID: f.analystB.ID, OpID: "res-r2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r2.IsRetest || r2.Version != 2 {
		t.Fatalf("retest flags wrong: %+v", r2)
	}
	// 不能引用不存在/更新的版本。
	_, err = st.SubmitResult(ResultInput{
		SampleID: c.SampleID, MethodID: f.method, RawReading: "x",
		Purity: 1, SupersedesVersion: 9, AnalystID: f.analyst.ID, OpID: "res-r3-bad",
	})
	if err == nil {
		t.Fatal("引用不存在版本应失败")
	}
	if _, err := st.ReviewResult(ReviewInput{
		ResultID: r2.ID, ReviewerID: f.reviewer.ID, Approve: true, OpID: "rev-r2",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdoptResult(AdoptInput{
		SampleID: c.SampleID, Version: 2, ActorID: f.admin.ID, OpID: "adopt-r2",
	}); err != nil {
		t.Fatal(err)
	}
	view, _ := st.SampleStatus(c.SampleID)
	if view.AdoptedResult != 2 || len(view.Results) != 2 {
		t.Fatalf("版本链异常: %+v", view)
	}
	if view.Results[0].Status != "reviewed" || view.Results[0].RawReading != "99.0" {
		t.Fatal("旧版本必须原样保留")
	}
}

// 并发/重复交接：重复 release 被唯一索引拒绝；重复 receive 只有一次成功。
func TestSingleCurrentCustodian(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)
	c := collectOK(t, st, f, "PUB-X1", f.container, true)

	rel, err := st.Release(ReleaseInput{
		SampleID: c.SampleID, OpID: "rel-x1", FromPersonID: f.samplerA.ID,
		ToPersonID: f.courier.ID, SealIntact: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 在途期间第二次交出必须失败（同一容器/样品的部分唯一索引）。
	if _, err := st.Release(ReleaseInput{
		SampleID: c.SampleID, OpID: "rel-x2", FromPersonID: f.samplerA.ID,
		ToPersonID: f.analyst.ID, SealIntact: true,
	}); err == nil {
		t.Fatal("在途交接未完成时不能再次交出")
	}
	// 两个接收请求（不同 op）：只有一条成功，保管版本只 +1。
	if _, err := st.Receive(ReceiveInput{
		TransferID: rel.TransferID, OpID: "rcv-x1", ReceiverID: f.courier.ID,
		ReleaseCode: rel.ReleaseCode, SealIntact: true, HasTemp: true, TempMin: -190, TempMax: -186,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Receive(ReceiveInput{
		TransferID: rel.TransferID, OpID: "rcv-x2", ReceiverID: f.courier.ID,
		ReleaseCode: rel.ReleaseCode, SealIntact: true, HasTemp: true, TempMin: -190, TempMax: -186,
	}); err == nil {
		t.Fatal("重复接收必须失败")
	}
	view, _ := st.SampleStatus(c.SampleID)
	if view.Version != 2 || view.CurrentHolder.ID != f.courier.ID {
		t.Fatalf("保管人/版本错误: holder=%s version=%d", view.CurrentHolder.ID, view.Version)
	}
}

// 接收必须承接上一位保管人的确认：确认码不符/非指定接收人均失败。
func TestReceiveRequiresReleaseConfirmation(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)
	c := collectOK(t, st, f, "PUB-X2", f.container, true)
	rel, _ := st.Release(ReleaseInput{
		SampleID: c.SampleID, OpID: "rel-x2", FromPersonID: f.samplerA.ID,
		ToPersonID: f.courier.ID, SealIntact: true,
	})
	if _, err := st.Receive(ReceiveInput{
		TransferID: rel.TransferID, OpID: "rcv-bad-code", ReceiverID: f.courier.ID,
		ReleaseCode: "wrong-code", SealIntact: true,
	}); err == nil {
		t.Fatal("确认码不符必须拒绝")
	}
	if _, err := st.Receive(ReceiveInput{
		TransferID: rel.TransferID, OpID: "rcv-bad-person", ReceiverID: f.analyst.ID,
		ReleaseCode: rel.ReleaseCode, SealIntact: true,
	}); err == nil {
		t.Fatal("非指定接收人必须拒绝")
	}
}

// 容器再次使用：在用时拒绝；消耗后可复用。
func TestContainerReuse(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)
	c := collectOK(t, st, f, "PUB-U1", f.container, true)
	if _, err := st.Collect(CollectInput{
		PublicCode: "PUB-U2", BatchID: f.batch, PointID: f.point,
		CollectorID: f.samplerA.ID, ContainerID: f.container,
		SealNo: "SEAL-2", SealIntact: true, OpID: "op-collect-u2",
	}); err == nil {
		t.Fatal("容器在用时不能再次使用")
	}
	// 交给化验员并消耗后，容器才释放。
	rel, err := st.Release(ReleaseInput{
		SampleID: c.SampleID, OpID: "rel-u1", FromPersonID: f.samplerA.ID,
		ToPersonID: f.analyst.ID, SealIntact: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Receive(ReceiveInput{
		TransferID: rel.TransferID, OpID: "rcv-u1", ReceiverID: f.analyst.ID,
		ReleaseCode: rel.ReleaseCode, SealIntact: true, HasTemp: true, TempMin: -190, TempMax: -186,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Consume(c.SampleID, f.analyst.ID, "op-consume-u1", "化验消耗"); err != nil {
		t.Fatal(err)
	}
	c2 := collectOK(t, st, f, "PUB-U2", f.container, true)
	if c2.SampleID == "" || c2.SampleID == c.SampleID {
		t.Fatal("消耗后容器应能盛装新样品")
	}
}

// 取消交接后可重新交出；保管版本不变。
func TestCancelAndRerelease(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)
	c := collectOK(t, st, f, "PUB-CA", f.container, true)
	rel, _ := st.Release(ReleaseInput{
		SampleID: c.SampleID, OpID: "rel-ca1", FromPersonID: f.samplerA.ID,
		ToPersonID: f.courier.ID, SealIntact: true,
	})
	if _, err := st.CancelTransfer(rel.TransferID, f.samplerA.ID, "cancel-ca1"); err != nil {
		t.Fatal(err)
	}
	view, _ := st.SampleStatus(c.SampleID)
	if view.Version != 1 || view.CurrentHolder.ID != f.samplerA.ID {
		t.Fatal("取消不应改变保管人与版本")
	}
	rel2, err := st.Release(ReleaseInput{
		SampleID: c.SampleID, OpID: "rel-ca2", FromPersonID: f.samplerA.ID,
		ToPersonID: f.analyst.ID, SealIntact: true,
	})
	if err != nil {
		t.Fatalf("取消后应能重新交出: %v", err)
	}
	if _, err := st.Receive(ReceiveInput{
		TransferID: rel2.TransferID, OpID: "rcv-ca2", ReceiverID: f.analyst.ID,
		ReleaseCode: rel2.ReleaseCode, SealIntact: true, HasTemp: true, TempMin: -190, TempMax: -186,
	}); err != nil {
		t.Fatal(err)
	}
}

// 拒收偏差：样品作废、容器释放；不能再提交结果。
func TestRejectDisposition(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)
	c := collectOK(t, st, f, "PUB-RJ", f.container, true)
	rel, _ := st.Release(ReleaseInput{
		SampleID: c.SampleID, OpID: "rel-rj", FromPersonID: f.samplerA.ID,
		ToPersonID: f.courier.ID, SealIntact: false,
	})
	recv, err := st.Receive(ReceiveInput{
		TransferID: rel.TransferID, OpID: "rcv-rj", ReceiverID: f.courier.ID,
		ReleaseCode: rel.ReleaseCode, SealIntact: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Dispose(DispositionInput{
		DeviationID: recv.DeviationIDs[0], ActorID: f.admin.ID,
		Action: "reject", Note: "封签破损拒收", OpID: "disp-rj",
	}); err != nil {
		t.Fatal(err)
	}
	view, _ := st.SampleStatus(c.SampleID)
	if view.Status != "void" {
		t.Fatalf("样品应作废, got %s", view.Status)
	}
	// 容器已释放，可再次使用。
	collectOK(t, st, f, "PUB-RJ2", f.container, true)
}

// 离线重放：同 op 重放不产生重复事件；过期 expected_version 被拒。
func TestOfflineReplay(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)
	c := collectOK(t, st, f, "PUB-O1", f.container, true)
	rel, err := st.Release(ReleaseInput{
		SampleID: c.SampleID, OpID: "rel-o1", FromPersonID: f.samplerA.ID,
		ToPersonID: f.courier.ID, SealIntact: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	in := ReceiveInput{
		TransferID: rel.TransferID, OpID: "rcv-o1", ReceiverID: f.courier.ID,
		ReleaseCode: rel.ReleaseCode, SealIntact: true, HasTemp: true, TempMin: -190, TempMax: -186,
	}
	first, err := st.Receive(in)
	if err != nil {
		t.Fatal(err)
	}
	// 同 op 重放：结果一致。
	again, err := st.Receive(in)
	if err != nil {
		t.Fatalf("replay should return stored result: %v", err)
	}
	if again.TransferID != first.TransferID || again.Version != first.Version {
		t.Fatalf("replay mismatch: %+v vs %+v", first, again)
	}
	var n int
	st.DB().QueryRow(`SELECT COUNT(*) FROM custody_events WHERE sample_id=? AND kind='receive'`,
		c.SampleID).Scan(&n)
	if n != 1 {
		t.Fatalf("重放不得产生重复接收事件, got %d", n)
	}
	// 携带过期版本的新交接被拒。
	_, err = st.Release(ReleaseInput{
		SampleID: c.SampleID, OpID: "rel-stale", FromPersonID: f.courier.ID,
		ToPersonID: f.analyst.ID, SealIntact: true, ExpectedVersion: 1,
	})
	if err == nil {
		t.Fatal("过期 expected_version 必须被拒绝")
	}
}

// append-only 触发器：事件表与编号登记表禁止 UPDATE/DELETE。
func TestAppendOnlyTriggers(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)
	c := collectOK(t, st, f, "PUB-AO", f.container, true)
	if _, err := st.DB().Exec(`UPDATE custody_events SET note='x' WHERE sample_id=?`, c.SampleID); err == nil {
		t.Fatal("custody_events 必须禁止 UPDATE")
	}
	if _, err := st.DB().Exec(`DELETE FROM custody_events WHERE sample_id=?`, c.SampleID); err == nil {
		t.Fatal("custody_events 必须禁止 DELETE")
	}
	if _, err := st.RegisterContainerCode(RegisterCodeInput{
		ContainerID: f.container, Scope: "lab", Code: "LAB-1",
		SampleID: c.SampleID, ActorID: f.analyst.ID, OpID: "reg-ao",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE container_registrations SET code='y' WHERE code='LAB-1'`); err == nil {
		t.Fatal("container_registrations 必须禁止 UPDATE")
	}
}

// 下钻视图：批次→采样点→样品→事件/偏差/版本完整。
func TestBatchChainDrilldown(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)
	c := collectOK(t, st, f, "PUB-CH", f.container, true)
	rel, _ := st.Release(ReleaseInput{
		SampleID: c.SampleID, OpID: "rel-ch", FromPersonID: f.samplerA.ID,
		ToPersonID: f.courier.ID, SealIntact: true,
	})
	st.Receive(ReceiveInput{
		TransferID: rel.TransferID, OpID: "rcv-ch", ReceiverID: f.courier.ID,
		ReleaseCode: rel.ReleaseCode, SealIntact: true, HasTemp: true, TempMin: -190, TempMax: -186,
	})
	view, err := st.BatchChain(f.batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Stations) != 1 || len(view.Stations[0].Samples) != 1 {
		t.Fatalf("下钻结构异常: %+v", view.Stations)
	}
	sm := view.Stations[0].Samples[0]
	if len(sm.Custody) != 3 { // collect/release/receive
		t.Fatalf("保管事件数错误: %d", len(sm.Custody))
	}
	if sm.CurrentHolder.ID != f.courier.ID {
		t.Fatal("当前保管人错误")
	}
}

// 审批结论视图只投影采用版本；暂停/作废样品显式可见。
func TestConclusionsProjection(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)
	c := collectOK(t, st, f, "PUB-CN", f.container, true)
	// 未复核前：conclusions 中 adopted_version=0。
	rows, err := st.BatchConclusions(f.batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AdoptedVersion != 0 {
		t.Fatalf("未完成复核不应有采用版本: %+v", rows)
	}
	r1, _ := st.SubmitResult(ResultInput{
		SampleID: c.SampleID, MethodID: f.method, RawReading: "99.5",
		Purity: 99.5, AnalystID: f.analyst.ID, OpID: "res-cn",
	})
	st.ReviewResult(ReviewInput{ResultID: r1.ID, ReviewerID: f.reviewer.ID, Approve: true, OpID: "rev-cn"})
	st.AdoptResult(AdoptInput{SampleID: c.SampleID, Version: 1, ActorID: f.admin.ID, OpID: "adopt-cn"})
	rows, _ = st.BatchConclusions(f.batch)
	if rows[0].AdoptedVersion != 1 || rows[0].Purity != 99.5 || rows[0].Reviewer == "" {
		t.Fatalf("结论投影错误: %+v", rows[0])
	}
}

// 外部实验室只见脱敏批号；回传结果仍需内部复核才能采用。
func TestExternalFlow(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)
	c := collectOK(t, st, f, "PUB-EX", f.container, true)
	if err := st.AssignExternal(c.PublicCode, f.method, f.admin.ID, "assign-ex"); err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListExternalOrders()
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 || orders[0].PublicCode != "PUB-EX" || orders[0].MethodName != "气相色谱法" {
		t.Fatalf("外部视图只能出现脱敏批号与方法: %+v", orders)
	}
	for _, o := range orders {
		if o.PublicCode == c.SampleID {
			t.Fatal("外部视图不得暴露内部样品 ID")
		}
	}
	r, err := st.SubmitExternalResult(ExternalResultInput{
		PublicCode: "PUB-EX", MethodID: f.method, RawReading: "99.2%",
		Purity: 99.2, ActorID: f.external.ID, OpID: "ext-res-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsExternal || r.Status != "submitted" {
		t.Fatalf("外部结果标志错误: %+v", r)
	}
	// 未复核不能采用。
	if _, err := st.AdoptResult(AdoptInput{
		SampleID: c.SampleID, Version: r.Version, ActorID: f.admin.ID, OpID: "adopt-ext-bad",
	}); err == nil {
		t.Fatal("外部结果未复核前不能采用")
	}
}

// 两个保管人并发交出同一样品：只能产生一条在途交接，始终只有一名当前保管人。
func TestConcurrentReleaseSingleHolder(t *testing.T) {
	st := openTestStore(t)
	f := setupFixture(t, st)
	c := collectOK(t, st, f, "PUB-CC", f.container, true)

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			to := f.courier.ID
			if i%2 == 0 {
				to = f.analyst.ID
			}
			_, err := st.Release(ReleaseInput{
				SampleID: c.SampleID, OpID: "rel-cc-" + string(rune('a'+i)),
				FromPersonID: f.samplerA.ID, ToPersonID: to, SealIntact: true,
			})
			errs <- err
		}(i)
	}
	ok, conflict := 0, 0
	for i := 0; i < n; i++ {
		err := <-errs
		switch {
		case err == nil:
			ok++
		default:
			var be *BizError
			if errors.As(err, &be) && be.Code == ErrConflict {
				conflict++
			} else {
				t.Fatalf("unexpected err: %v", err)
			}
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("并发交出应恰好成功 1 次，其余冲突：ok=%d conflict=%d", ok, conflict)
	}
	var openN int
	st.DB().QueryRow(`SELECT COUNT(*) FROM transfers WHERE sample_id=? AND status='released'`,
		c.SampleID).Scan(&openN)
	if openN != 1 {
		t.Fatalf("在途交接必须只有 1 条, got %d", openN)
	}
	view, _ := st.SampleStatus(c.SampleID)
	if view.Version != 1 || view.CurrentHolder.ID != f.samplerA.ID {
		t.Fatal("在途期间当前保管人必须仍是交出人且版本不变")
	}
}

var _ = sql.ErrNoRows
