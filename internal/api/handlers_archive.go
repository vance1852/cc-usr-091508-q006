package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"samplechain/internal/store"
)

// ---------- 人员 / 令牌 ----------

type personReq struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Team string `json:"team"`
	Role string `json:"role"`
}

func (s *Server) createPerson(w http.ResponseWriter, r *http.Request) {
	var req personReq
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	p, err := s.st.CreatePerson(store.Person{ID: req.ID, Name: req.Name, Team: req.Team, Role: req.Role})
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) issueToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Token string `json:"token"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	tok, err := s.st.IssueToken(id, req.Token)
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"person_id": id, "token": tok})
}

// ---------- 批次 / 采样点 / 温度 / 方法 ----------

type batchReq struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Product string `json:"product"`
}

func (s *Server) createBatch(w http.ResponseWriter, r *http.Request) {
	var req batchReq
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	actor := personFrom(r.Context())
	b, err := s.st.CreateBatch(store.Batch{ID: req.ID, Name: req.Name, Product: req.Product}, actor.ID)
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) createPoint(w http.ResponseWriter, r *http.Request) {
	batchID := chi.URLParam(r, "id")
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	id, err := s.st.CreateSamplingPoint(batchID, req.Name)
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "batch_id": batchID, "name": req.Name})
}

func (s *Server) setTempLimits(w http.ResponseWriter, r *http.Request) {
	batchID := chi.URLParam(r, "id")
	var req struct {
		MinCelsius *float64 `json:"min_celsius"`
		MaxCelsius *float64 `json:"max_celsius"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	if req.MinCelsius == nil || req.MaxCelsius == nil {
		writeError(w, http.StatusUnprocessableEntity, "validation", "必须提供 min_celsius 与 max_celsius")
		return
	}
	id, err := s.st.SetTempLimits(batchID, *req.MinCelsius, *req.MaxCelsius)
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "batch_id": batchID,
		"min_celsius": *req.MinCelsius, "max_celsius": *req.MaxCelsius,
	})
}

func (s *Server) createMethod(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	id, err := s.st.CreateMethod(req.ID, req.Name, req.Description)
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "name": req.Name})
}

// ---------- 容器 / 编号登记 / 采集 ----------

func (s *Server) registerContainer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	id, err := s.st.RegisterContainer(req.ID, req.Kind)
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "kind": req.Kind})
}

func (s *Server) registerCode(w http.ResponseWriter, r *http.Request) {
	containerID := chi.URLParam(r, "id")
	var req struct {
		Scope    string `json:"scope"`
		Code     string `json:"code"`
		SampleID string `json:"sample_id"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	actor := personFrom(r.Context())
	res, err := s.st.RegisterContainerCode(store.RegisterCodeInput{
		ContainerID: containerID, Scope: req.Scope, Code: req.Code,
		SampleID: req.SampleID, ActorID: actor.ID,
		OpID: idempotencyKey(r, newOpID("reg")),
	})
	if res.Conflict {
		// 偏差已落库并暂停样品：以 409 告知冲突，响应体携带偏差号。
		writeJSON(w, http.StatusConflict, res)
		return
	}
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

type collectReq struct {
	PublicCode  string `json:"public_code"`
	BatchID     string `json:"batch_id"`
	PointID     string `json:"point_id"`
	ContainerID string `json:"container_id"`
	SealNo      string `json:"seal_no"`
	SealIntact  *bool  `json:"seal_intact"`
	CollectedAt string `json:"collected_at"`
}

func (s *Server) collect(w http.ResponseWriter, r *http.Request) {
	var req collectReq
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	if req.SealIntact == nil {
		writeError(w, http.StatusUnprocessableEntity, "validation", "必须明确 seal_intact")
		return
	}
	actor := personFrom(r.Context())
	res, err := s.st.Collect(store.CollectInput{
		PublicCode: req.PublicCode, BatchID: req.BatchID, PointID: req.PointID,
		CollectorID: actor.ID, ContainerID: req.ContainerID, SealNo: req.SealNo,
		SealIntact: *req.SealIntact, CollectedAt: req.CollectedAt,
		OpID: idempotencyKey(r, newOpID("collect")),
	})
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// newOpID 为在线请求生成服务端幂等键；离线客户端通过 Idempotency-Key 自带。
func newOpID(prefix string) string {
	return prefix + "-" + randToken(12)
}
