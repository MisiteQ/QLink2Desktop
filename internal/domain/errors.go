package domain

import (
	"errors"
	"fmt"
)

// 领域哨兵错误。上层用 errors.Is 判断，避免依赖错误文案做分支判断。
var (
	// ErrValidation：输入不合法（缺字段、格式错误、越界）。
	ErrValidation = errors.New("参数校验失败")
	// ErrNotFound：指定的链接不存在。
	ErrNotFound = errors.New("链接不存在")
	// ErrNameConflict：AppName 已被其它链接占用。
	ErrNameConflict = errors.New("应用标识冲突")
	// ErrForbidden：已通过身份校验但无权执行该操作（例如清理非本项目管理的应用）。
	ErrForbidden = errors.New("操作被拒绝")
)

// NotFound 返回一个带上资源名的 ErrNotFound，用于「XX 不存在」这类提示。
//
// 用 errors.Is(err, ErrNotFound) 依然能识别，因此不会破坏上层的分支判断，
// 只是让最终展示给用户的文案更具体。
func NotFound(resource string) error {
	return fmt.Errorf("%w: %s不存在", ErrNotFound, resource)
}
