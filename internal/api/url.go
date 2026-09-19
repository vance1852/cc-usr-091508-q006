package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// urlID 解析路径中的整数 ID。
func urlID(w http.ResponseWriter, r *http.Request, param string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, param), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, param+" 必须是整数")
		return 0, false
	}
	return id, true
}
