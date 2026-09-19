package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"samplechain/internal/store"
)

// Server 持有样品链路 HTTP 接口的依赖。
type Server struct {
	DB *store.DB
}

// NewRouter 构建 Chi 路由。
func NewRouter(db *store.DB) http.Handler {
	s := &Server{DB: db}
	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Route("/v1", func(r chi.Router) {
		r.Use(s.auth)

		// 样品登记与交接（外部实验室、审批人不持有保管责任）
		r.With(requireRole("sampler", "admin")).Post("/samples/register", s.registerSample)
		r.With(requireRole("sampler", "analyst", "admin")).
			Post("/samples/{labNo}/release", s.release)
		r.With(requireRole("sampler", "analyst", "admin")).
			Post("/samples/{labNo}/receive", s.receive)
		r.With(requireRole("sampler", "analyst", "admin")).
			Post("/samples/{labNo}/recover/release", s.recoverRelease)
		r.With(requireRole("sampler", "analyst", "admin")).
			Post("/samples/{labNo}/recover/receive", s.recoverReceive)
		r.With(requireRole("sampler", "analyst", "admin")).
			Post("/samples/{labNo}/return", s.returnContainer)
		r.With(requireRole("sampler", "analyst", "admin")).
			Post("/samples/{labNo}/temperatures", s.addTemperatures)
		r.With(requireRole("sampler", "admin")).
			Post("/shipments/{boxCode}/close", s.closeShipment)

		// 检测结果
		r.With(requireRole("analyst", "admin")).Post("/results", s.submitResult)
		r.With(requireRole("reviewer", "admin")).Post("/results/{id}/review", s.reviewResult)

		// 偏差处置
		r.With(requireRole("reviewer", "admin")).
			Post("/deviations/{id}/disposition", s.disposeDeviation)
		r.With(requireRole("reviewer", "admin")).Get("/deviations", s.listDeviations)

		// 查询 / 下钻：评审人员逐站核对样品去向、异常处置与采用的结果版本。
		// 试车审批人不看下钻明细，只能通过 approval 接口读取完成复核的结论。
		r.With(requireRole("reviewer", "admin")).
			Get("/samples/{labNo}/trace", s.traceSample)
		r.With(requireRole("reviewer", "admin")).
			Get("/batches/{code}/trace", s.traceBatch)
		r.With(requireRole("approver", "admin")).
			Get("/batches/{code}/approval", s.approvalView)
		r.With(requireRole("reviewer", "admin")).
			Get("/batches/{code}/masked", s.maskedBatch)

		// 外部实验室：只看脱敏批号
		r.With(requireRole("external")).Get("/external/batches/{masked}", s.externalView)
	})

	return r
}

type ctxKey string

const userCtxKey ctxKey = "user"

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if tok == "" {
			writeError(w, http.StatusUnauthorized, "缺少 Bearer 令牌")
			return
		}
		u, err := s.DB.UserByToken(tok)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "令牌无效或已停用")
			return
		}
		ctx := r.Context()
		ctx = contextWithUser(ctx, u)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return ""
}

func requireRole(roles ...string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, r := range roles {
		allowed[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := userFromContext(r.Context())
			if !ok || !allowed[u.Role] {
				writeError(w, http.StatusForbidden, "当前角色无权执行该操作")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errBody struct {
	Error     string               `json:"error"`
	Conflicts []store.ConflictItem `json:"conflicts,omitempty"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errBody{Error: msg})
}

// mapStoreError 将存储层错误映射为 HTTP 状态码。
func mapStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrValidation):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrForbidden):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "内部错误: "+err.Error())
	}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
