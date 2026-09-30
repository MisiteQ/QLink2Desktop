package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// maxBodyBytes 是单个请求体的上限。
//
// 8MB 足够容纳一张高清图标（前端会先本地压缩），
// 又能挡住「往内存里灌一个 GB」这类误操作或恶意请求。
const maxBodyBytes = 8 << 20

// errorBody 是所有失败响应的统一形状。
//
// 前端只需要读 `error` 一个字段就能展示，不必为每种接口写一套解析逻辑。
type errorBody struct {
	Error string `json:"error"`
	// Kind 让前端可以做「按类型分支」而不必去匹配中文文案。
	Kind string `json:"kind,omitempty"`
}

// writeJSON 输出 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 接口响应一律不缓存：这些数据都是实时的运行态。
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// 此时响应头已发出，无法再改状态码，只能记录。
		slog.Debug("写出 JSON 响应失败", "error", err)
	}
}

// ok 输出 200 + 数据。
func ok(w http.ResponseWriter, v any) { writeJSON(w, http.StatusOK, v) }

// acceptedResponse 是「已受理，稍后完成」的统一响应。
//
// 用于一切走后台队列的动作：请求路径只保证"收到了并且会去做"，
// 不保证"已经做完"。前端据此把提示从「完成」改成「已提交，正在处理」，
// 并靠 SSE 推送的状态变化看到最终结果。
type acceptedResponse struct {
	Accepted bool   `json:"accepted"`
	Pending  int    `json:"pending"`
	Message  string `json:"message,omitempty"`
}

// accepted 输出 202 + 受理回执。
func accepted(w http.ResponseWriter, msg string, pending int) {
	writeJSON(w, http.StatusAccepted, acceptedResponse{
		Accepted: true,
		Pending:  pending,
		Message:  msg,
	})
}

// acceptedWith 输出 202 + 自带负载的受理回执。
//
// 用于「受理后立刻能给出一个可轮询的句柄」的场景（例如网络扫描返回任务快照）。
// 直接用 acceptedResponse 会丢掉这个句柄，前端就只能靠猜。
func acceptedWith(w http.ResponseWriter, v any) {
	writeJSON(w, http.StatusAccepted, v)
}

// writeError 输出结构化的错误响应。
func writeError(w http.ResponseWriter, err error) {
	status, kind := statusForError(err)
	if status >= http.StatusInternalServerError {
		slog.Error("接口处理失败", "status", status, "error", err)
	}
	writeJSON(w, status, errorBody{Error: err.Error(), Kind: kind})
}

// statusForError 把领域错误映射为 HTTP 状态码与机器可读类型。
//
// 集中映射的好处：处理器里只需要 `return err`，
// 不必在每个分支里手写 WriteHeader —— 那种写法最容易漏掉一个分支导致返回 200 + 错误体。
func statusForError(err error) (int, string) {
	switch {
	case err == nil:
		return http.StatusOK, ""
	case errors.Is(err, domain.ErrValidation):
		return http.StatusBadRequest, "validation"
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, domain.ErrNameConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(err, domain.ErrForbidden):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, errUnauthorized):
		return http.StatusUnauthorized, "unauthorized"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

// decodeJSON 解析请求体；失败时已写出错误响应并返回 false。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		writeError(w, fmt.Errorf("%w: 请求体为空", domain.ErrValidation))
		return false
	}
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(body)
	// 拒绝未知字段：拼错的字段名会立刻暴露，而不是被静默忽略。
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, fmt.Errorf("%w: 请求体解析失败: %v", domain.ErrValidation, err))
		return false
	}
	// 允许末尾有空白，但不能再有第二个 JSON 值。
	if err := dec.Decode(&struct{}{}); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, fmt.Errorf("%w: 请求体包含多余内容", domain.ErrValidation))
		return false
	}
	return true
}

// pathValue 读取路径参数并去掉首尾空白。
func pathValue(r *http.Request, name string) string {
	return strings.TrimSpace(r.PathValue(name))
}

// queryBool 解析形如 ?force=1 的布尔查询参数。
func queryBool(r *http.Request, name string) bool {
	v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// clientIP 尽力还原真实来源地址，优先取反代写入的转发头。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if rip := r.Header.Get("X-Real-Ip"); rip != "" {
		return strings.TrimSpace(rip)
	}
	host := r.RemoteAddr
	// RemoteAddr 是 host:port，只保留 host。
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	return host
}
