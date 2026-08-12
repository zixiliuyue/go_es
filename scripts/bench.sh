#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# bench.sh - go_es 性能基准脚本
#
# 功能:
#   1) 一键跑所有 benchmark, 输出到 bench/ 目录下带时间戳的基线文件
#   2) 支持两个基线文件的 benchstat 对比, 退化 > 阈值时非 0 退出(阻断 CI)
#   3) smoke 模式: 仅跑 1k 级 1 次采样, 用于快速 PR
#   4) full 模式: 跑所有基准 5 次采样, 用于正式基线
#
# 使用:
#   # smoke 模式, 快速 baseline(默认模式, 无参数)
#   bash scripts/bench.sh
#
#   # full 模式, 收集正式基线并写入 bench/<date>_full.txt
#   bash scripts/bench.sh full
#
#   # 对比两个基线文件, 退化 > 10% 非 0 退出
#   bash scripts/bench.sh compare bench/old.txt bench/new.txt [10]
#
#   # 仅跑某个 package, 保存到指定文件
#   bash scripts/bench.sh run-only search bench/my_search.txt
#
# 环境变量:
#   BENCH_REGRESSION_THRESHOLD_PCT: compare 模式阈值, 默认 10
#   BENCH_COUNT: 每个基准采样数, full 默认 5, smoke 默认 1
#   BENCH_TIME: 每个基准最短运行时间, full 默认 1s, smoke 默认 50ms
#   GO_BENCHSTAT: benchstat 可执行文件名(默认 golang.org/x/perf/cmd/benchstat, 会自动 go install)
# ---------------------------------------------------------------------------

set -euo pipefail

# 切换到 repo 根目录
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

BENCH_DIR="$REPO_ROOT/bench"
mkdir -p "$BENCH_DIR"

# ------------------------------------------------------------------
# 参数解析
# ------------------------------------------------------------------
MODE="${1:-smoke}"
shift || true

THRESHOLD_PCT="${BENCH_REGRESSION_THRESHOLD_PCT:-10}"

# ------------------------------------------------------------------
# 工具函数
# ------------------------------------------------------------------

die() { echo "❌ $*" >&2; exit 1; }

require_go() {
  if ! command -v go >/dev/null 2>&1; then
    die "go 不在 PATH, 请先安装 Go 1.25"
  fi
}

ensure_benchstat() {
  local bin="${GO_BENCHSTAT:-benchstat}"
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ℹ️  未找到 benchstat, 尝试: go install golang.org/x/perf/cmd/benchstat@latest"
    GOFLAGS="-mod=mod" go install golang.org/x/perf/cmd/benchstat@latest || \
      die "安装 benchstat 失败, 请手动安装后重试"
    # $GOBIN 或默认 $GOPATH/bin 可能不在 PATH, 显式解析绝对路径
    local gopath_bin
    gopath_bin="$(go env GOPATH)/bin/benchstat"
    if [[ -x "$gopath_bin" ]]; then
      bin="$gopath_bin"
    else
      bin=benchstat
    fi
  fi
  echo "$bin"
}

# run_benches <pkg_pattern, e.g. ./internal/search/|./internal/server/|all> <out_file> <count> <benchtime>
run_benches() {
  local pattern="$1"
  local out_file="$2"
  local count="$3"
  local benchtime="$4"

  require_go

  case "$pattern" in
    all)  target="./internal/search/ ./internal/server/" ;;
    search) target="./internal/search/" ;;
    server) target="./internal/server/" ;;
    *)     die "未知 package 模式: $pattern (支持: all/search/server)" ;;
  esac

  local ts
  ts="$(date '+%Y-%m-%dT%H:%M:%S')"

  echo "=================================================================="
  echo "🧪 go_es benchmark 开始: mode=$MODE pkg=${pattern} count=${count} benchtime=${benchtime}"
  echo "📅 时间: ${ts}"
  echo "📁 输出: ${out_file}"
  echo "=================================================================="

  {
    echo "# go_es benchmark baseline: mode=${MODE} pkg=${pattern}"
    echo "# generated_at: ${ts}"
    echo "# go: $(go version)"
    echo "# host: $(uname -sm) cpu=$(sysctl -n machdep.cpu.brand_string 2>/dev/null || uname -p)"
    echo ""
  } >"$out_file"

  local fail=0
  for pkg in $target; do
    echo "==> 运行: go test -bench=Benchmark -benchmem -count=${count} -benchtime=${benchtime} ${pkg}"
    # go test 把 benchmark 输出写 stdout, zap 日志写 stderr;
    # 只保留 stdout (BenchmarkX 行) 进基线, 日志丢弃, 避免污染 benchstat 解析。
    if ! go test -bench=Benchmark -benchmem "-count=${count}" "-benchtime=${benchtime}" "$pkg" >>"$out_file" 2>/dev/null; then
      echo "❗ 失败: $pkg (可去掉 2>/dev/null 查看日志)" >&2
      fail=1
    fi
  done

  if [[ $fail -ne 0 ]]; then
    die "基准运行失败"
  fi

  echo "✅ 基线已写入: $out_file"
  wc -l "$out_file"
}

# ------------------------------------------------------------------
# smoke 模式
# ------------------------------------------------------------------
mode_smoke() {
  local out="$BENCH_DIR/smoke_$(date '+%Y%m%d_%H%M%S').txt"
  run_benches "all" "$out" "1" "50ms"
  # 软链到 bench/latest_smoke.txt, 方便 diff
  ln -sf "$out" "$BENCH_DIR/latest_smoke.txt"
  echo "🔗 软链: bench/latest_smoke.txt -> $(basename "$out")"
}

# ------------------------------------------------------------------
# full 模式
# ------------------------------------------------------------------
mode_full() {
  local out="$BENCH_DIR/full_$(date '+%Y%m%d_%H%M%S').txt"
  local count="${BENCH_COUNT:-5}"
  local benchtime="${BENCH_TIME:-1s}"
  run_benches "all" "$out" "$count" "$benchtime"
  ln -sf "$out" "$BENCH_DIR/latest_full.txt"
  echo "🔗 软链: bench/latest_full.txt -> $(basename "$out")"
}

# ------------------------------------------------------------------
# run-only 模式: scripts/bench.sh run-only search bench/my.txt
# ------------------------------------------------------------------
mode_run_only() {
  local pkg="${1:?缺少 pkg (all/search/server)}"
  local out_file="${2:?缺少 out_file}"
  mkdir -p "$(dirname "$out_file")"
  # run-only 强制 count=1, benchtime=50ms, 便于快速复现
  local count="${BENCH_COUNT:-1}"
  local benchtime="${BENCH_TIME:-50ms}"
  run_benches "$pkg" "$out_file" "$count" "$benchtime"
}

# ------------------------------------------------------------------
# compare 模式: scripts/bench.sh compare OLD NEW [THRESHOLD_PCT]
# 输出 benchstat 差异, 任意 ns/op 退化 > 阈值时 exit 1
# ------------------------------------------------------------------
mode_compare() {
  local old="${1:?缺少 OLD 基线文件}"
  local new="${2:?缺少 NEW 基线文件}"
  local threshold_pct="${3:-$THRESHOLD_PCT}"

  [[ -f "$old" ]] || die "OLD 基线不存在: $old"
  [[ -f "$new" ]] || die "NEW 基线不存在: $new"

  local bs
  bs="$(ensure_benchstat)"

  echo "=================================================================="
  echo "⚖️  benchstat: $old ↔ $new"
  echo "   退化阈值: > ${threshold_pct}% 视为回归"
  echo "=================================================================="

  local report
  # 先试 benchstat 默认(新版本 benchstat 无 -delta-test flag)
  report="$("$bs" "$old" "$new" 2>&1 || true)"
  # 若 report 包含 flag 相关帮助(说明参数传错/文件被识别失败), 不打印帮助, 再试无帮助路径
  if echo "$report" | grep -qE '(-col |Usage:|consider change significant)'; then
    report="$("$bs" "$old" "$new" 2>/dev/null || true)"
  fi

  echo "$report"

  # ------------------------------------------------------------------
  # 直接从 OLD/NEW 两个原始 go test 输出中提取 ns/op 平均值,
  # 自行计算退化百分比(不依赖 benchstat 输出格式, 对 count=1 也可用)
  # ------------------------------------------------------------------
  # bench_means <file> 输出 "bench_name mean_ns" (同名 benchmark 多采样取均值)
  bench_means() {
    awk '
      /^Benchmark/ && / ns\/op / {
        # 典型: BenchmarkX-10  100  10000 ns/op  35000 B/op  10 allocs/op
        name=$1; ns=$3;
        sum[name] += ns; cnt[name] += 1;
      }
      END {
        for (k in sum) printf "%s %.3f\n", k, sum[k]/cnt[k];
      }
    ' "$1" | sort
  }

  local old_means new_means
  old_means="$(bench_means "$old")"
  new_means="$(bench_means "$new")"
  [[ -n "$old_means" ]] || die "OLD 基线无可解析 Benchmark 行: $old"
  [[ -n "$new_means" ]] || die "NEW 基线无可解析 Benchmark 行: $new"

  echo ""
  echo "--- ns/op 退化检查 (阈值 +${threshold_pct}%) ---"
  local regressions=0
  local regressed_lines=""

  # 遍历 OLD 的每一行, 在 NEW 里找同名 benchmark
  local name old_ns new_ns
  while IFS=' ' read -r name old_ns; do
    [[ -z "$name" ]] && continue
    new_ns="$(awk -v k="$name" '$1==k{print $2}' <<<"$new_means")"
    if [[ -z "$new_ns" ]]; then
      continue
    fi
    # pct = (new - old) / old * 100
    local pct
    pct="$(awk -v o="$old_ns" -v n="$new_ns" 'BEGIN{ if (o<=0) print 0; else printf "%.2f", (n-o)/o*100.0 }')"
    local is_reg
    is_reg="$(awk -v p="$pct" -v t="$threshold_pct" 'BEGIN{print (p > t) ? 1 : 0}')"
    if [[ "$is_reg" == "1" ]]; then
      regressions=$((regressions+1))
      # old/new ns 显示为更易读的整数
      local old_int new_int
      old_int="$(printf "%.0f" "$old_ns")"
      new_int="$(printf "%.0f" "$new_ns")"
      local line="⚠️  退化 +${pct}%: ${name}  ${old_int} → ${new_int} ns/op"
      echo "$line" >&2
      regressed_lines="${regressed_lines}${line}"$'\n'
    fi
  done <<<"$old_means"

  echo ""
  if [[ $regressions -eq 0 ]]; then
    echo "✅ 无性能退化(阈值 +${threshold_pct}%)"
    exit 0
  else
    echo "🛑 发现 ${regressions} 项性能退化(超过 +${threshold_pct}%), 请修复后再合入" >&2
    exit 2
  fi
}

# ------------------------------------------------------------------
# help
# ------------------------------------------------------------------
mode_help() {
  cat <<'EOF'
Usage:
  bash scripts/bench.sh                              smoke 模式 (默认)
  bash scripts/bench.sh smoke                        smoke 模式 (1 次采样, 快)
  bash scripts/bench.sh full                         full 模式  (5 次采样, benchstat 友好)
  bash scripts/bench.sh run-only <pkg> <out>         仅跑指定 pkg (all/search/server)
  bash scripts/bench.sh compare <old> <new> [pct]    对比两个基线, 退化 > pct% 时非 0
  bash scripts/bench.sh help                         查看帮助

环境变量:
  BENCH_COUNT          每个基准采样次数 (smoke=1, full=5)
  BENCH_TIME           每个基准最短运行时间 (smoke=50ms, full=1s)
  BENCH_REGRESSION_THRESHOLD_PCT  compare 模式阈值%, 默认 10
EOF
}

# ------------------------------------------------------------------
# 入口
# ------------------------------------------------------------------
case "$MODE" in
  -h|--help|help)    mode_help ;;
  smoke|"")          mode_smoke ;;
  full)              mode_full ;;
  run-only)          mode_run_only "$@" ;;
  compare)           mode_compare "$@" ;;
  *)                 die "未知模式: $MODE (支持: smoke/full/run-only/compare/help)" ;;
esac
