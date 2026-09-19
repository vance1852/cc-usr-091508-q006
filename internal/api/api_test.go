package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"samplechain/internal/seed"
	"samplechain/internal/store"
)

func newTestServer(t *testing.T) (http.Handler, *store.DB, *seed.Data) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	data, err := seed.Load(db)
	if err != nil {
		t.Fatal(err)
	}
	return NewRouter(db), db, data
}

func doReq(t *testing.T, h http.Handler, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func regBody(labNo, bottle, seal, box string) map[string]any {
	return map[string]any{
		"batch_code": "FIRE-2026-009", "box_code": box, "lab_no": labNo,
		"container_code": bottle, "seal_no": seal, "point_code": "TANK-01",
		"method_code": "LOX-PURITY",
		"sampled_at":  time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}
}

func TestAuthAndRoles(t *testing.T) {
	h, _, _ := newTestServer(t)

	if code, _ := doReq(t, h, "POST", "/v1/samples/register", "", regBody("L", "BTL-001", "S", "B")); code != http.StatusUnauthorized {
		t.Fatalf("no token: expected 401, got %d", code)
	}
	// 审批人不能登记。
	if code, body := doReq(t, h, "POST", "/v1/samples/register", "tok-approver1",
		regBody("L1", "BTL-001", "S1", "B1")); code != http.StatusForbidden {
		t.Fatalf("approver register: expected 403, got %d %v", code, body)
	}
	// 外部实验室不能读内部下钻。
	if code, _ := doReq(t, h, "GET", "/v1/batches/FIRE-2026-009/trace", "tok-extlab", nil); code != http.StatusForbidden {
		t.Fatalf("external trace: expected 403, got %d", code)
	}
	// 采样员不能看审批视图。
	if code, _ := doReq(t, h, "GET", "/v1/batches/FIRE-2026-009/approval", "tok-zhang", nil); code != http.StatusForbidden {
		t.Fatalf("sampler approval: expected 403, got %d", code)
	}
	// 试车审批人不能逐站下钻，只能读审批结论。
	if code, _ := doReq(t, h, "GET", "/v1/batches/FIRE-2026-009/trace", "tok-approver1", nil); code != http.StatusForbidden {
		t.Fatalf("approver trace: expected 403, got %d", code)
	}
}

func TestEndToEndFlow(t *testing.T) {
	h, db, data := newTestServer(t)
	_ = data

	// 1) 甲班登记。
	code, body := doReq(t, h, "POST", "/v1/samples/register", "tok-zhang",
		regBody("LOX-LAB-100", "BTL-001", "SEAL-100", "BOX-100"))
	if code != http.StatusCreated {
		t.Fatalf("register: %d %v", code, body)
	}

	// 2) 重复登记（乙班用同实验室编号）→ 409 + 冲突明细 + 留痕偏差。
	code, body = doReq(t, h, "POST", "/v1/samples/register", "tok-li",
		regBody("LOX-LAB-100", "BTL-002", "SEAL-101", "BOX-101"))
	if code != http.StatusConflict {
		t.Fatalf("conflict register: expected 409, got %d", code)
	}
	conflicts, _ := body["conflicts"].([]any)
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %v", body)
	}

	// 3) 温度正常。
	code, body = doReq(t, h, "POST", "/v1/samples/LOX-LAB-100/temperatures", "tok-zhang",
		map[string]any{"readings": []map[string]any{
			{"taken_at": "2026-09-19T08:00:00Z", "celsius": -185.0},
			{"taken_at": "2026-09-19T08:20:00Z", "celsius": -184.2},
		}})
	if code != http.StatusCreated {
		t.Fatalf("temps: %d %v", code, body)
	}

	// 4) 交接给化验员。
	code, body = doReq(t, h, "POST", "/v1/samples/LOX-LAB-100/release", "tok-zhang",
		map[string]any{"to_login": "analyst1", "station": "化验室门口"})
	if code != http.StatusCreated {
		t.Fatalf("release: %d %v", code, body)
	}
	// 非接收人确认 → 403。
	if code, _ := doReq(t, h, "POST", "/v1/samples/LOX-LAB-100/receive", "tok-li",
		map[string]any{"seal_intact": true}); code != http.StatusForbidden {
		t.Fatalf("wrong receiver: expected 403, got %d", code)
	}
	code, body = doReq(t, h, "POST", "/v1/samples/LOX-LAB-100/receive", "tok-analyst1",
		map[string]any{"seal_intact": true})
	if code != http.StatusCreated {
		t.Fatalf("receive: %d %v", code, body)
	}

	// 5) 化验员提交结果，自己复核被拒。
	code, body = doReq(t, h, "POST", "/v1/results", "tok-analyst1", map[string]any{
		"lab_no": "LOX-LAB-100", "method_code": "LOX-PURITY", "instrument": "GC-9",
		"readings": []map[string]any{{"name": "purity", "value": 99.51, "unit": "%"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("submit: %d %v", code, body)
	}
	rid := int64(body["id"].(float64))
	if code, _ := doReq(t, h, "POST", path("/v1/results/%d/review", rid), "tok-analyst1",
		map[string]any{"approve": true}); code != http.StatusForbidden {
		t.Fatalf("self review: expected 403, got %d", code)
	}

	// 6) 复核人完成复核。
	code, body = doReq(t, h, "POST", path("/v1/results/%d/review", rid), "tok-reviewer1",
		map[string]any{"approve": true, "conclusion": "纯度合格"})
	if code != http.StatusOK {
		t.Fatalf("review: %d %v", code, body)
	}
	if body["status"] != "reviewed" {
		t.Fatalf("expected reviewed, got %v", body["status"])
	}

	// 7) 审批人读到的放行依据可放行，且指向 v1。
	code, body = doReq(t, h, "GET", "/v1/batches/FIRE-2026-009/approval", "tok-approver1", nil)
	if code != http.StatusOK {
		t.Fatalf("approval: %d %v", code, body)
	}
	items := body["items"].([]any)
	var found bool
	for _, it := range items {
		m := it.(map[string]any)
		if m["lab_no"] == "LOX-LAB-100" {
			found = true
			if m["releasable"] != true || m["result_version"].(float64) != 1 {
				t.Fatalf("approval item wrong: %v", m)
			}
		}
	}
	if !found {
		t.Fatalf("approval item missing")
	}

	// 8) 外部实验室只见脱敏批号与合格结论，不见内部编号。
	code, body = doReq(t, h, "GET", "/v1/batches/FIRE-2026-009/masked", "tok-reviewer1", nil)
	if code != http.StatusOK {
		t.Fatalf("masked: %d %v", code, body)
	}
	masked := body["masked_batch"].(string)
	code, body = doReq(t, h, "GET", "/v1/external/batches/"+masked, "tok-extlab", nil)
	if code != http.StatusOK {
		t.Fatalf("external: %d %v", code, body)
	}
	samples := body["samples"].([]any)
	if len(samples) != 1 {
		t.Fatalf("expected 1 external sample, got %d", len(samples))
	}
	sv := samples[0].(map[string]any)
	if sv["status"] != "released" {
		t.Fatalf("external status expected released, got %v", sv["status"])
	}
	if _, leaked := sv["lab_no"]; leaked {
		t.Fatalf("external view must not leak internal lab number")
	}

	// 9) 评审下钻：链路完整。
	code, body = doReq(t, h, "GET", "/v1/batches/FIRE-2026-009/trace", "tok-reviewer1", nil)
	if code != http.StatusOK {
		t.Fatalf("trace: %d %v", code, body)
	}
	tr := body["samples"].([]any)
	st := tr[0].(map[string]any)
	if st["chain_intact"] != true {
		t.Fatalf("chain should be intact, trace=%v", st)
	}
	if len(st["custody_chain"].([]any)) != 3 {
		t.Fatalf("expected 3 custody events (collect/release/receive)")
	}

	// 10) 容器归还后可再用。
	if code, _ = doReq(t, h, "POST", "/v1/samples/LOX-LAB-100/return", "tok-analyst1",
		map[string]any{"note": "检测完毕"}); code != http.StatusCreated {
		t.Fatalf("return: expected 201, got %d", code)
	}
	code, body = doReq(t, h, "POST", "/v1/samples/register", "tok-zhang",
		regBody("LOX-LAB-200", "BTL-001", "SEAL-200", "BOX-200"))
	if code != http.StatusCreated {
		t.Fatalf("bottle reuse register: expected 201, got %d %v", code, body)
	}

	// 直接核对库里仍然只有一个当前保管人。
	s, err := db.GetSampleByLabNo("LOX-LAB-200")
	if err != nil {
		t.Fatal(err)
	}
	if s.CurrentCustodian != "zhang" {
		t.Fatalf("new sample custodian should be zhang, got %q", s.CurrentCustodian)
	}
}

func path(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}
