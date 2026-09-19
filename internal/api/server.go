// Package api 提供样品链路服务的 HTTP 接口（chi 路由）。
//
// 鉴权：Authorization: Bearer <token>，令牌在登录令牌表中预置。
// 写接口接受 Idempotency-Key 头作为离线幂等键 op_id。
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"samplechain/internal/store"
)

type Server struct {
	st *store.Store
}

func NewServer(st *store.Store) http.Handler {
	s := &Server{st: st}
	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	r.Route("/v1", func(r chi.Router) {
		// 档案管理（化验室管理员）
		r.With(s.requireRole("lab_admin")).Post("/people", s.createPerson)
		r.With(s.requireRole("lab_admin")).Post("/people/{id}/tokens", s.issueToken)
		r.With(s.requireRole("lab_admin")).Post("/batches", s.createBatch)
		r.With(s.requireRole("lab_admin")).Post("/batches/{id}/points", s.createPoint)
		r.With(s.requireRole("lab_admin")).Put("/batches/{id}/temp-limits", s.setTempLimits)
		r.With(s.requireRole("lab_admin")).Post("/methods", s.createMethod)
		r.With(s.requireRole("lab_admin")).Post("/samples/{id}/deviations", s.openDeviation)
		r.With(s.requireRole("lab_admin")).Post("/deviations/{id}/dispositions", s.dispose)
		r.With(s.requireRole("lab_admin")).Post("/samples/{id}/adopt", s.adopt)
		r.With(s.requireRole("lab_admin")).Post("/samples/{id}/external-assignment", s.assignExternal)

		// 容器与采集
		r.With(s.requireRole("sampler", "lab_admin")).Post("/containers", s.registerContainer)
		r.With(s.requireRole("sampler", "courier", "lab_admin")).
			Post("/containers/{id}/registrations", s.registerCode)
		r.With(s.requireRole("sampler")).Post("/samples", s.collect)

		// 保管链（保管人资格在事务内核验，这里排除外部身份）
		r.With(s.requireRole("sampler", "courier", "analyst", "lab_admin")).
			Post("/samples/{id}/release", s.release)
		r.With(s.requireRole("sampler", "courier", "analyst", "lab_admin")).
			Post("/transfers/{id}/receive", s.receive)
		r.With(s.requireRole("sampler", "courier", "analyst", "lab_admin")).
			Post("/transfers/{id}/cancel", s.cancel)
		r.With(s.requireRole("analyst", "lab_admin")).Post("/samples/{id}/consume", s.consume)

		// 检测结果
		r.With(s.requireRole("analyst", "lab_admin")).Post("/samples/{id}/results", s.submitResult)
		r.With(s.requireRole("reviewer")).Post("/results/{id}/review", s.reviewResult)

		// 读取
		r.With(s.requireRole("sampler", "courier", "analyst", "reviewer", "lab_admin")).
			Get("/samples/{id}", s.getSample)
		r.With(s.requireRole("reviewer", "lab_admin")).
			Get("/batches/{id}/chain", s.batchChain)
		// 审批人只能读取完成复核的结论，不能下钻含原始读数/草稿的完整链路。
		r.With(s.requireRole("reviewer", "lab_admin", "approver")).
			Get("/batches/{id}/conclusions", s.batchConclusions)

		// 外部实验室：只能接触脱敏批号
		r.With(s.requireRole("external_lab")).Get("/external/orders", s.listExternalOrders)
		r.With(s.requireRole("external_lab")).Post("/external/results", s.submitExternalResult)
	})

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return r
}

// ---------- 鉴权 ----------

type ctxKey string

const personKey ctxKey = "person"

func (s *Server) requireRole(roles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(roles))
	for _, r := range roles {
		allowed[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := bearerToken(r)
			if tok == "" {
				writeError(w, http.StatusUnauthorized, "unauthorized", "缺少登录令牌")
				return
			}
			p, err := s.st.PersonByToken(tok)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized", "令牌无效")
				return
			}
			if !allowed[p.Role] {
				writeError(w, http.StatusForbidden, "forbidden",
					"角色 %s 无权执行该操作", p.Role)
				return
			}
			ctx := contextWithPerson(r.Context(), p)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return ""
}

// ---------- JSON 辅助 ----------

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, format string, args ...any) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": sprintfF(format, args...),
		},
	})
}

func writeBizError(w http.ResponseWriter, err error) {
	var be *store.BizError
	if errors.As(err, &be) {
		writeError(w, statusFor(be.Code), string(be.Code), "%s", be.Message)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", "%s", err.Error())
}

func statusFor(code store.ErrCode) int {
	switch code {
	case store.ErrNotFound:
		return http.StatusNotFound
	case store.ErrPermission, store.ErrDuty:
		return http.StatusForbidden
	case store.ErrValidation:
		return http.StatusUnprocessableEntity
	case store.ErrConflict, store.ErrCustody, store.ErrState, store.ErrReplay:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func idempotencyKey(r *http.Request, fallback string) string {
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		return k
	}
	return fallback
}
