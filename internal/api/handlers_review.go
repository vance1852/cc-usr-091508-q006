package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"samplechain/internal/store"
)

// ---------- 偏差 ----------

func (s *Server) openDeviation(w http.ResponseWriter, r *http.Request) {
	sampleID := chi.URLParam(r, "id")
	var req struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
		Kind   string `json:"kind"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	actor := personFrom(r.Context())
	id, err := s.st.OpenManualDeviation(store.ManualDeviationInput{
		SampleID: sampleID, ActorID: actor.ID, Title: req.Title,
		Detail: req.Detail, Kind: req.Kind,
		OpID: idempotencyKey(r, newOpID("dev")),
	})
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"deviation_id": id, "status": "open"})
}

func (s *Server) dispose(w http.ResponseWriter, r *http.Request) {
	devID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "偏差 ID 非法")
		return
	}
	var req struct {
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	actor := personFrom(r.Context())
	res, err := s.st.Dispose(store.DispositionInput{
		DeviationID: devID, ActorID: actor.ID, Action: req.Action, Note: req.Note,
		OpID: idempotencyKey(r, newOpID("disp")),
	})
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- 检测结果 / 复核 / 采用 ----------

func (s *Server) submitResult(w http.ResponseWriter, r *http.Request) {
	sampleID := chi.URLParam(r, "id")
	var req struct {
		MethodID          string  `json:"method_id"`
		RawReading        string  `json:"raw_reading"`
		RawUnit           string  `json:"raw_unit"`
		Purity            float64 `json:"purity"`
		SupersedesVersion int     `json:"supersedes_version"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	actor := personFrom(r.Context())
	res, err := s.st.SubmitResult(store.ResultInput{
		SampleID: sampleID, MethodID: req.MethodID, RawReading: req.RawReading,
		RawUnit: req.RawUnit, Purity: req.Purity,
		SupersedesVersion: req.SupersedesVersion, AnalystID: actor.ID,
		OpID: idempotencyKey(r, newOpID("result")),
	})
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) reviewResult(w http.ResponseWriter, r *http.Request) {
	resultID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "结果 ID 非法")
		return
	}
	var req struct {
		Approve *bool  `json:"approve"`
		Note    string `json:"note"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	if req.Approve == nil {
		writeError(w, http.StatusUnprocessableEntity, "validation", "必须明确 approve")
		return
	}
	actor := personFrom(r.Context())
	res, err := s.st.ReviewResult(store.ReviewInput{
		ResultID: resultID, ReviewerID: actor.ID, Approve: *req.Approve,
		Note: req.Note, OpID: idempotencyKey(r, newOpID("review")),
	})
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) adopt(w http.ResponseWriter, r *http.Request) {
	sampleID := chi.URLParam(r, "id")
	var req struct {
		Version int    `json:"version"`
		Note    string `json:"note"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	if req.Version <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "validation", "必须指定采用的 version")
		return
	}
	actor := personFrom(r.Context())
	res, err := s.st.AdoptResult(store.AdoptInput{
		SampleID: sampleID, Version: req.Version, ActorID: actor.ID, Note: req.Note,
		OpID: idempotencyKey(r, newOpID("adopt")),
	})
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) assignExternal(w http.ResponseWriter, r *http.Request) {
	sampleID := chi.URLParam(r, "id")
	var req struct {
		MethodID string `json:"method_id"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	// 外部委托以样品的脱敏批号为键；先取样品再委托。
	st, err := s.st.SampleStatus(sampleID)
	if err != nil {
		writeBizError(w, err)
		return
	}
	actor := personFrom(r.Context())
	if err := s.st.AssignExternal(st.PublicCode, req.MethodID, actor.ID,
		idempotencyKey(r, newOpID("extassign"))); err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"public_code": st.PublicCode, "method_id": req.MethodID,
	})
}

// ---------- 读取 ----------

func (s *Server) getSample(w http.ResponseWriter, r *http.Request) {
	sampleID := chi.URLParam(r, "id")
	st, err := s.st.SampleStatus(sampleID)
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) batchChain(w http.ResponseWriter, r *http.Request) {
	batchID := chi.URLParam(r, "id")
	view, err := s.st.BatchChain(batchID)
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) batchConclusions(w http.ResponseWriter, r *http.Request) {
	batchID := chi.URLParam(r, "id")
	view, err := s.st.BatchConclusions(batchID)
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch_id": batchID, "conclusions": view})
}

// ---------- 外部实验室 ----------

func (s *Server) listExternalOrders(w http.ResponseWriter, _ *http.Request) {
	orders, err := s.st.ListExternalOrders()
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": orders})
}

func (s *Server) submitExternalResult(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PublicCode string  `json:"public_code"`
		MethodName string  `json:"method"`
		RawReading string  `json:"raw_reading"`
		RawUnit    string  `json:"raw_unit"`
		Purity     float64 `json:"purity"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	methodID, err := s.st.MethodIDByName(req.MethodName)
	if err != nil {
		writeBizError(w, err)
		return
	}
	actor := personFrom(r.Context())
	res, err := s.st.SubmitExternalResult(store.ExternalResultInput{
		PublicCode: req.PublicCode, MethodID: methodID, RawReading: req.RawReading,
		RawUnit: req.RawUnit, Purity: req.Purity, ActorID: actor.ID,
		OpID: idempotencyKey(r, newOpID("extresult")),
	})
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}
