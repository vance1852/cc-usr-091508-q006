package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"samplechain/internal/store"
)

// ---------- 保管链 ----------

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	sampleID := chi.URLParam(r, "id")
	var req struct {
		ToPersonID      string `json:"to_person_id"`
		SealIntact      *bool  `json:"seal_intact"`
		Note            string `json:"note"`
		ExpectedVersion int    `json:"expected_version"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	if req.ToPersonID == "" {
		writeError(w, http.StatusUnprocessableEntity, "validation", "缺少 to_person_id")
		return
	}
	if req.SealIntact == nil {
		writeError(w, http.StatusUnprocessableEntity, "validation", "必须明确 seal_intact")
		return
	}
	actor := personFrom(r.Context())
	res, err := s.st.Release(store.ReleaseInput{
		SampleID: sampleID, OpID: idempotencyKey(r, newOpID("release")),
		FromPersonID: actor.ID, ToPersonID: req.ToPersonID,
		SealIntact: *req.SealIntact, Note: req.Note, ExpectedVersion: req.ExpectedVersion,
	})
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) receive(w http.ResponseWriter, r *http.Request) {
	transferID := chi.URLParam(r, "id")
	var req struct {
		ReleaseCode     string   `json:"release_code"`
		SealIntact      *bool    `json:"seal_intact"`
		SealNo          string   `json:"seal_no"`
		TempMin         *float64 `json:"temp_min"`
		TempMax         *float64 `json:"temp_max"`
		ExpectedVersion int      `json:"expected_version"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	if req.ReleaseCode == "" {
		writeError(w, http.StatusUnprocessableEntity, "validation", "必须提供扫码得到的 release_code")
		return
	}
	if req.SealIntact == nil {
		writeError(w, http.StatusUnprocessableEntity, "validation", "必须明确 seal_intact")
		return
	}
	if (req.TempMin == nil) != (req.TempMax == nil) {
		writeError(w, http.StatusUnprocessableEntity, "validation", "温度区间必须同时提供 min/max，或整体省略表示记录空白")
		return
	}
	actor := personFrom(r.Context())
	in := store.ReceiveInput{
		TransferID: transferID, OpID: idempotencyKey(r, newOpID("receive")),
		ReceiverID: actor.ID, ReleaseCode: req.ReleaseCode,
		SealIntact: *req.SealIntact, SealNo: req.SealNo,
		ExpectedVersion: req.ExpectedVersion,
	}
	if req.TempMin != nil && req.TempMax != nil {
		in.HasTemp = true
		in.TempMin, in.TempMax = *req.TempMin, *req.TempMax
	}
	res, err := s.st.Receive(in)
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	transferID := chi.URLParam(r, "id")
	var req struct {
		Note string `json:"note"`
	}
	_ = decode(r, &req)
	actor := personFrom(r.Context())
	res, err := s.st.CancelTransfer(transferID, actor.ID, idempotencyKey(r, newOpID("cancel")))
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) consume(w http.ResponseWriter, r *http.Request) {
	sampleID := chi.URLParam(r, "id")
	var req struct {
		Note string `json:"note"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求体解析失败：%s", err.Error())
		return
	}
	actor := personFrom(r.Context())
	res, err := s.st.Consume(sampleID, actor.ID, idempotencyKey(r, newOpID("consume")), req.Note)
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
