package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"samplechain/internal/store"
)

type apiFixture struct {
	st    *store.Store
	srv   http.Handler
	token map[string]string
	ids   map[string]string
}

func setupAPIFixture(t *testing.T) apiFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	mk := func(id, name, team, role string) {
		if _, err := st.CreatePerson(store.Person{ID: id, Name: name, Team: team, Role: role}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.IssueToken(id, "tok-"+id); err != nil {
			t.Fatal(err)
		}
	}
	mk("admin", "管理员", "管理组", "lab_admin")
	mk("sa", "采样甲", "采样一组", "sampler")
	mk("sb", "采样乙", "采样二组", "sampler")
	mk("co", "押运", "转运组", "courier")
	mk("an", "化验甲", "化验一组", "analyst")
	mk("rv", "复核", "质量组", "reviewer")
	mk("rv1", "复核同组", "采样一组", "reviewer")
	mk("ap", "审批", "指挥部", "approver")
	mk("ex", "外部", "外部实验室", "external_lab")

	batch, err := st.CreateBatch(store.Batch{ID: "b1", Name: "试车一号", Product: "LOX"}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	point, err := st.CreateSamplingPoint(batch.ID, "加注口")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetTempLimits(batch.ID, -196, -183); err != nil {
		t.Fatal(err)
	}
	method, err := st.CreateMethod("m-gc", "气相色谱法", "")
	if err != nil {
		t.Fatal(err)
	}
	container, err := st.RegisterContainer("c1", "bottle")
	if err != nil {
		t.Fatal(err)
	}
	return apiFixture{
		st:    st,
		srv:   NewServer(st),
		token: map[string]string{"admin": "tok-admin", "sa": "tok-sa", "sb": "tok-sb", "co": "tok-co", "an": "tok-an", "rv": "tok-rv", "rv1": "tok-rv1", "ap": "tok-ap", "ex": "tok-ex"},
		ids:   map[string]string{"batch": batch.ID, "point": point, "method": method, "container": container},
	}
}

func (f apiFixture) do(t *testing.T, method, path, who string, body any, idem string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if who != "" {
		req.Header.Set("Authorization", "Bearer "+f.token[who])
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, req)
	return rr
}

func decodeBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode failed: %v (body=%s)", err, rr.Body.String())
	}
	return m
}

// 端到端走完整链路，返回样品 ID。
func fullCollect(t *testing.T, f apiFixture, code string) string {
	rr := f.do(t, "POST", "/v1/samples", "sa", map[string]any{
		"public_code": code, "batch_id": f.ids["batch"], "point_id": f.ids["point"],
		"container_id": f.ids["container"], "seal_no": "S-1", "seal_intact": true,
	}, "collect-"+code)
	if rr.Code != http.StatusCreated {
		t.Fatalf("collect status=%d body=%s", rr.Code, rr.Body.String())
	}
	return decodeBody(t, rr)["sample_id"].(string)
}

func TestHTTPAuthMatrix(t *testing.T) {
	f := setupAPIFixture(t)

	// 无令牌 401。
	if rr := f.do(t, "GET", "/v1/samples/x", "", nil, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401, got %d", rr.Code)
	}
	// 采样员不能建批次（403）。
	if rr := f.do(t, "POST", "/v1/batches", "sa", map[string]any{"name": "x", "product": "LOX"}, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("越权应 403, got %d", rr.Code)
	}
	// 审批人只读结论，任何写操作 403。
	if rr := f.do(t, "POST", "/v1/batches", "ap", map[string]any{"name": "x", "product": "LOX"}, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("审批人写操作应 403, got %d", rr.Code)
	}
	// 外部实验室不能访问内部接口。
	if rr := f.do(t, "GET", "/v1/batches/"+f.ids["batch"]+"/chain", "ex", nil, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("外部访问链路应 403, got %d", rr.Code)
	}
}

func TestHTTPEndToEnd(t *testing.T) {
	f := setupAPIFixture(t)
	sampleID := fullCollect(t, f, "PUB-E2E")

	// 交出（押运）。
	rr := f.do(t, "POST", "/v1/samples/"+sampleID+"/release", "sa", map[string]any{
		"to_person_id": "co", "seal_intact": true,
	}, "rel-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("release: %d %s", rr.Code, rr.Body.String())
	}
	rel := decodeBody(t, rr)
	transferID := rel["transfer_id"].(string)
	code := rel["release_code"].(string)

	// 非接收人扫码接收 → 409（保管链冲突）。
	rr = f.do(t, "POST", "/v1/transfers/"+transferID+"/receive", "an", map[string]any{
		"release_code": code, "seal_intact": true,
		"temp_min": -190, "temp_max": -186,
	}, "rcv-wrong-person")
	if rr.Code != http.StatusConflict {
		t.Fatalf("非接收人应 409, got %d %s", rr.Code, rr.Body.String())
	}
	// 押运本人凭确认码接收。
	rr = f.do(t, "POST", "/v1/transfers/"+transferID+"/receive", "co", map[string]any{
		"release_code": code, "seal_intact": true,
		"temp_min": -190, "temp_max": -186,
	}, "rcv-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("receive: %d %s", rr.Code, rr.Body.String())
	}

	// 押运交化验。
	rr = f.do(t, "POST", "/v1/samples/"+sampleID+"/release", "co", map[string]any{
		"to_person_id": "an", "seal_intact": true,
	}, "rel-2")
	rel = decodeBody(t, rr)
	rr = f.do(t, "POST", "/v1/transfers/"+rel["transfer_id"].(string)+"/receive", "an", map[string]any{
		"release_code": rel["release_code"].(string), "seal_intact": true,
		"seal_no": "S-1", "temp_min": -190, "temp_max": -186,
	}, "rcv-2")
	if rr.Code != http.StatusOK {
		t.Fatalf("receive2: %d %s", rr.Code, rr.Body.String())
	}

	// 化验提交 → 同班组复核 403 → 跨班组复核通过。
	rr = f.do(t, "POST", "/v1/samples/"+sampleID+"/results", "an", map[string]any{
		"method_id": f.ids["method"], "raw_reading": "99.5%", "purity": 99.5,
	}, "res-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("submit: %d %s", rr.Code, rr.Body.String())
	}
	resultID := int64(decodeBody(t, rr)["id"].(float64))

	rr = f.do(t, "POST", "/v1/results/"+strconv.FormatInt(resultID, 10)+"/review", "rv1", map[string]any{
		"approve": true,
	}, "rev-sameteam")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("同班组复核应 403, got %d %s", rr.Code, rr.Body.String())
	}
	rr = f.do(t, "POST", "/v1/results/"+strconv.FormatInt(resultID, 10)+"/review", "rv", map[string]any{
		"approve": true,
	}, "rev-ok")
	if rr.Code != http.StatusOK {
		t.Fatalf("跨班组复核: %d %s", rr.Code, rr.Body.String())
	}

	// 管理员采用。
	rr = f.do(t, "POST", "/v1/samples/"+sampleID+"/adopt", "admin", map[string]any{
		"version": 1,
	}, "adopt-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("adopt: %d %s", rr.Code, rr.Body.String())
	}

	// 审批人只读结论；不能下钻完整链路。
	rr = f.do(t, "GET", "/v1/batches/"+f.ids["batch"]+"/conclusions", "ap", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("审批人读结论: %d", rr.Code)
	}
	body := decodeBody(t, rr)
	concs := body["conclusions"].([]any)
	c0 := concs[0].(map[string]any)
	if c0["adopted_version"].(float64) != 1 || c0["purity"].(float64) != 99.5 {
		t.Fatalf("结论投影错误: %+v", c0)
	}
	if rr := f.do(t, "GET", "/v1/batches/"+f.ids["batch"]+"/chain", "ap", nil, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("审批人不应下钻完整链路, got %d", rr.Code)
	}
	// 复核人/管理员可以下钻。
	if rr := f.do(t, "GET", "/v1/batches/"+f.ids["batch"]+"/chain", "rv", nil, ""); rr.Code != http.StatusOK {
		t.Fatalf("复核人下钻应 200, got %d", rr.Code)
	}
}

func TestHTTPIdempotencyReplay(t *testing.T) {
	f := setupAPIFixture(t)
	sampleID := fullCollect(t, f, "PUB-IDEM")

	doRel := func() *httptest.ResponseRecorder {
		return f.do(t, "POST", "/v1/samples/"+sampleID+"/release", "sa", map[string]any{
			"to_person_id": "co", "seal_intact": true,
		}, "idem-release")
	}
	r1 := doRel()
	r2 := doRel()
	if r1.Code != http.StatusCreated || r2.Code != http.StatusCreated {
		t.Fatalf("replay codes: %d %d", r1.Code, r2.Code)
	}
	if decodeBody(t, r1)["transfer_id"] != decodeBody(t, r2)["transfer_id"] {
		t.Fatal("同 Idempotency-Key 重放必须返回同一交接单")
	}
}

func TestHTTPExternalMasking(t *testing.T) {
	f := setupAPIFixture(t)
	sampleID := fullCollect(t, f, "PUB-MASK")

	rr := f.do(t, "POST", "/v1/samples/"+sampleID+"/external-assignment", "admin", map[string]any{
		"method_id": f.ids["method"],
	}, "assign-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("assign: %d %s", rr.Code, rr.Body.String())
	}
	// 外部只见脱敏批号与方法名。
	rr = f.do(t, "GET", "/v1/external/orders", "ex", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list orders: %d", rr.Code)
	}
	body := decodeBody(t, rr)
	orders := body["orders"].([]any)
	if len(orders) != 1 || orders[0].(map[string]any)["public_code"] != "PUB-MASK" {
		t.Fatalf("外部视图错误: %+v", orders)
	}
	for _, o := range orders {
		if o.(map[string]any)["public_code"] == sampleID {
			t.Fatal("外部视图不得暴露内部 ID")
		}
	}
	// 外部按方法名回传结果。
	rr = f.do(t, "POST", "/v1/external/results", "ex", map[string]any{
		"public_code": "PUB-MASK", "method": "气相色谱法",
		"raw_reading": "99.2%", "purity": 99.2,
	}, "ext-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("external result: %d %s", rr.Code, rr.Body.String())
	}
	// 外部不能访问样品真 ID 路径。
	if rr := f.do(t, "GET", "/v1/samples/"+sampleID, "ex", nil, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("外部访问内部样品应 403, got %d", rr.Code)
	}
}

// 温度记录空白（省略温度）→ 样品暂停；审批人结论视图中 hold=true。
func TestHTTPTempGapHold(t *testing.T) {
	f := setupAPIFixture(t)
	sampleID := fullCollect(t, f, "PUB-GAP")
	rr := f.do(t, "POST", "/v1/samples/"+sampleID+"/release", "sa", map[string]any{
		"to_person_id": "co", "seal_intact": true,
	}, "rel-gap")
	rel := decodeBody(t, rr)
	rr = f.do(t, "POST", "/v1/transfers/"+rel["transfer_id"].(string)+"/receive", "co", map[string]any{
		"release_code": rel["release_code"].(string), "seal_intact": true,
	}, "rcv-gap") // 不带温度 → 空白
	if rr.Code != http.StatusOK {
		t.Fatalf("receive: %d %s", rr.Code, rr.Body.String())
	}
	if !decodeBody(t, rr)["hold"].(bool) {
		t.Fatal("温度空白应暂停样品")
	}
	rr = f.do(t, "GET", "/v1/batches/"+f.ids["batch"]+"/conclusions", "ap", nil, "")
	c0 := decodeBody(t, rr)["conclusions"].([]any)[0].(map[string]any)
	if c0["hold"] != true {
		t.Fatalf("审批视图必须暴露暂停状态: %+v", c0)
	}
}
