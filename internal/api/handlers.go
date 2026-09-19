package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"samplechain/internal/store"
)

func (s *Server) registerSample(w http.ResponseWriter, r *http.Request) {
	u, _ := userFromContext(r.Context())
	var in store.RegisterSampleInput
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	sample, conflicts, err := s.DB.RegisterSample(u, in)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeJSON(w, http.StatusConflict, errBody{Error: err.Error(), Conflicts: conflicts})
			return
		}
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sample)
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	s.doTransfer(w, r, false)
}

func (s *Server) recoverRelease(w http.ResponseWriter, r *http.Request) {
	s.doTransfer(w, r, true)
}

func (s *Server) doTransfer(w http.ResponseWriter, r *http.Request, offline bool) {
	u, _ := userFromContext(r.Context())
	var in store.ReleaseInput
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	in.LabNo = chi.URLParam(r, "labNo")
	if offline && in.IdemKey != "" {
		if id, err := s.DB.SampleIDByLabNo(in.LabNo); err == nil {
			if ev, err := s.DB.EventByIdemKey(id, in.IdemKey); err == nil {
				writeJSON(w, http.StatusOK, map[string]any{"replayed": true, "event": ev})
				return
			}
		}
	}
	ev, err := s.DB.Release(u, in, offline)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

func (s *Server) receive(w http.ResponseWriter, r *http.Request) {
	s.doReceive(w, r, false)
}

func (s *Server) recoverReceive(w http.ResponseWriter, r *http.Request) {
	s.doReceive(w, r, true)
}

func (s *Server) doReceive(w http.ResponseWriter, r *http.Request, offline bool) {
	u, _ := userFromContext(r.Context())
	var in store.ReceiveInput
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	in.LabNo = chi.URLParam(r, "labNo")
	if offline && in.IdemKey != "" {
		if id, err := s.DB.SampleIDByLabNo(in.LabNo); err == nil {
			if ev, err := s.DB.EventByIdemKey(id, in.IdemKey); err == nil {
				writeJSON(w, http.StatusOK, map[string]any{"replayed": true, "event": ev})
				return
			}
		}
	}
	ev, err := s.DB.Receive(u, in, offline)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

func (s *Server) returnContainer(w http.ResponseWriter, r *http.Request) {
	u, _ := userFromContext(r.Context())
	var body struct {
		Note string `json:"note"`
	}
	_ = decode(r, &body)
	ev, err := s.DB.ReturnContainer(u, chi.URLParam(r, "labNo"), body.Note)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

func (s *Server) closeShipment(w http.ResponseWriter, r *http.Request) {
	u, _ := userFromContext(r.Context())
	if err := s.DB.CloseShipment(u, chi.URLParam(r, "boxCode")); err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "closed", "box_code": chi.URLParam(r, "boxCode")})
}

func (s *Server) addTemperatures(w http.ResponseWriter, r *http.Request) {
	u, _ := userFromContext(r.Context())
	var body struct {
		Readings []store.TemperatureInput `json:"readings"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	out, err := s.DB.AddTemperatures(u, chi.URLParam(r, "labNo"), body.Readings)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"recorded": out})
}

func (s *Server) submitResult(w http.ResponseWriter, r *http.Request) {
	u, _ := userFromContext(r.Context())
	var in store.SubmitResultInput
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	rv, err := s.DB.SubmitResult(u, in)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rv)
}

func (s *Server) reviewResult(w http.ResponseWriter, r *http.Request) {
	u, _ := userFromContext(r.Context())
	var body struct {
		Approve    bool   `json:"approve"`
		Conclusion string `json:"conclusion"`
		Note       string `json:"note"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	id, ok := urlID(w, r, "id")
	if !ok {
		return
	}
	rv, err := s.DB.ReviewResult(u, id, body.Approve, body.Conclusion, body.Note)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rv)
}

func (s *Server) disposeDeviation(w http.ResponseWriter, r *http.Request) {
	u, _ := userFromContext(r.Context())
	var in store.DispositionInput
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	id, ok := urlID(w, r, "id")
	if !ok {
		return
	}
	in.DeviationID = id
	d, err := s.DB.DisposeDeviation(u, in)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) listDeviations(w http.ResponseWriter, r *http.Request) {
	devs, err := s.DB.Deviations(0)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deviations": devs})
}

func (s *Server) traceSample(w http.ResponseWriter, r *http.Request) {
	sample, err := s.DB.GetSampleByLabNo(chi.URLParam(r, "labNo"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	tr, err := s.DB.TraceSample(sample.ID)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tr)
}

func (s *Server) traceBatch(w http.ResponseWriter, r *http.Request) {
	tr, err := s.DB.TraceBatch(chi.URLParam(r, "code"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tr)
}

func (s *Server) approvalView(w http.ResponseWriter, r *http.Request) {
	items, err := s.DB.ApprovalView(chi.URLParam(r, "code"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch": chi.URLParam(r, "code"), "items": items})
}

func (s *Server) maskedBatch(w http.ResponseWriter, r *http.Request) {
	masked, ok, err := s.DB.MaskedBatchOf(chi.URLParam(r, "code"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "批次不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"masked_batch": masked})
}

func (s *Server) externalView(w http.ResponseWriter, r *http.Request) {
	views, err := s.DB.ExternalViewByMaskedBatch(chi.URLParam(r, "masked"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"masked_batch": chi.URLParam(r, "masked"), "samples": views})
}
