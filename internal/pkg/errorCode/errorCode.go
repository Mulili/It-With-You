package errorcode

import "errors"

type Error struct {
	Code    int    // 业务错误码
	Message string // 对外展示的消息
	Err     error  // 内部原始 error（可选，用于日志）
}

func (e *Error) Error() string {
	return e.Message
}

const (
	// HTTP 标准状态码
	OK                 = 200
	BadRequest         = 400
	Unauthorized       = 401
	Forbidden          = 403
	NotFound           = 404
	Conflict           = 409
	Internal           = 500
	TooManyRequests    = 429
	ServiceUnavailable = 503
	//	API状态码
	CodeUnkownAPIKey = 10001
)

var (
	//HTTP标准状态码
	ErrorBadRequest      = &Error{Code: BadRequest, Message: "请求参数错误"}
	ErrorUnauthorized    = &Error{Code: Unauthorized, Message: "未授权，请先登录"}
	ErrCodeForbidden     = &Error{Code: Forbidden, Message: "无权限执行该操作"}
	ErrorNotFound        = &Error{Code: NotFound, Message: "资源不存在"}
	ErrorConflict        = &Error{Code: Conflict, Message: "资源冲突"}
	ErrorInternal        = &Error{Code: Internal, Message: "服务器内部错误"}
	ErrorTooManyRequests = &Error{Code: TooManyRequests, Message: "请求过于频繁，请稍后再试"}
	//API状态码
	ErrCodeUnKownAPIKey = &Error{Code: CodeUnkownAPIKey, Message: "API不存在或无法访问"}
)

// 创建纯业务错误（不包裹内部 error）
func New(code int, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

// 包裹内部 error，用于 Service 层记录原始错误
func Wrap(err error, code int, msg string) *Error {
	return &Error{Code: code, Message: msg, Err: err}
}

// 从 error 中提取 *Error，用于 Handler 层判断
func From(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
