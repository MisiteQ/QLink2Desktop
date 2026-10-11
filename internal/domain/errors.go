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
	// ErrPortInUse：本应用内已有规则占用了这个监听端口。
	//
	// 它和 ErrNameConflict 都是"资源被抢"，但对用户的行动指引完全不同：
	// 标识冲突要改名字，端口冲突要换端口或先停掉占用它的东西。
	// 分开成两个哨兵，前端才能按类型给出不同的提示，而不是去匹配中文文案。
	//
	// 文本刻意写成"监听端口冲突"而不是"端口已被占用"：它会被 %w 包在消息
	// 前面，包装体本身已经带了端口号（"…: 18080 已被规则「X」占用"），
	// 前缀再提一次端口就变成"端口已被占用: 端口 18080 已被规则…"——
	// 读起来像系统出错。这条是在界面上真看到才发现的。
	ErrPortInUse = errors.New("监听端口冲突")
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
