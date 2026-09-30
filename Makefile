# QLink2Desktop Makefile
#
# 常用目标：
#   make            等价于 make fpk（默认产出飞牛安装包）
#   make fpk        构建当前平台的 .fpk（x86 / arm 可用 ARCH= 指定）
#   make release    发版：版本号 patch +1，双架构一起出包
#   make build      编译本机可执行文件，便于本地调试
#   make test       运行全部单元测试
#   make vet        go vet
#   make fmt        gofmt 并报告需要重排的文件
#   make icons      从根目录源图重新生成全部图标资源
#   make e2e        网关等效端到端验证（无需浏览器）
#   make browser-e2e 真浏览器启动期验证（需 Chromium 系浏览器 + node）
#   make clean      清理构建产物
#
# 说明：真正完成 fpk 打包的是 scripts/build-fpk.sh，
# 它负责交叉编译、图标生成、manifest 的 platform 注入以及调用官方 fnpack。
# 版本号存在仓库根目录的 VERSION 文件里，只有 make release 会改写它。

APP_NAME  := qlink2desktop
GO       ?= $(shell command -v go 2>/dev/null || echo go)
PYTHON   ?= $(shell command -v python3 2>/dev/null || command -v python 2>/dev/null || echo python3)

# Windows 上可执行文件带 .exe 后缀（Go 的 -o 不会自动补）。
ifeq ($(OS),Windows_NT)
EXE := .exe
else
EXE :=
endif

# 版本号唯一来源：仓库根目录的 VERSION 文件（scripts/release.sh 负责递增）。
VERSION ?= $(shell tr -d '[:space:]' < VERSION 2>/dev/null || echo 1.0.0)

ARCH ?= x86

LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all fpk fpk-all release build run test vet fmt icons clean check jscheck e2e browser-e2e verify

all: fpk

# ---- 打包 -------------------------------------------------------------------

fpk:
	./scripts/build-fpk.sh $(ARCH)

# 双架构一起出包（不打版本号，便于同一版本反复验证）。
fpk-all:
	./scripts/build-fpk.sh x86
	./scripts/build-fpk.sh arm

# 正式发版：先把版本号 +1，再一次性构建双架构，产物名带版本号。
release:
	./scripts/release.sh

# ---- 本地开发 ---------------------------------------------------------------

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(APP_NAME)$(EXE) ./cmd/server

run: build
	./$(APP_NAME)$(EXE) -port 5900

# ---- 质量门禁 ---------------------------------------------------------------

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	@out="$$(gofmt -l . )"; \
	if [ -n "$$out" ]; then echo "以下文件需要 gofmt:"; echo "$$out"; exit 1; fi; \
	echo "gofmt: 全部通过"

# 一次性跑完提交前应过的全部检查。
check: fmt vet test jscheck

# ---- 端到端 -------------------------------------------------------------

# 用两个 HTTP 代理复刻飞牛统一网关的两种转发方式（剥前缀 / 带前缀），
# 逐条验证「网关之后」页面能否正常打开：入口尾斜杠自愈、文档、每个静态
# 资源、递归 import 图、首屏单请求、鉴权边界、深链接回退。
# 这些正是真机白屏踩过的坑，本机直连 localhost 是测不出来的。
e2e: build
	$(PYTHON) scripts/e2e-gateway-check.py --exe ./$(APP_NAME)$(EXE)

# 真浏览器启动期验证：起后端 + 网关模拟器，用真实 Chromium 打开入口，
# 等看门狗窗口过去再读回页面实际显示的内容。
# 需要本机有 Chromium 系浏览器（Edge 即可）+ node + playwright-core。
# 验证的是"浏览器拿到页面之后的行为"——缓存、预扫描、模块图失败，
# 这些只看服务端响应是看不出来的。
browser-e2e: build
	$(PYTHON) scripts/browser-e2e.py --exe ./$(APP_NAME)$(EXE) --expect ok

# 交付包复验：解开产物逐个核对版本号、架构、包结构，
# 以及守卫 / 看门狗 / import 图自检是否真的进了包。
verify:
	$(PYTHON) scripts/verify-fpk.py

jscheck:
	$(PYTHON) scripts/check-js-imports.py

# ---- 资源 -------------------------------------------------------------------

icons:
	$(GO) run ./cmd/iconforge -src QLink2Desktop.png -app fnos-app

# ---- 清理 -------------------------------------------------------------------

clean:
	rm -rf .build
	rm -f $(APP_NAME)$(EXE)
	rm -f $(APP_NAME)-*.fpk
