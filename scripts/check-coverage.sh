#!/usr/bin/env bash
# check-coverage.sh — compare Go coverprofile (or a numeric total) against a threshold.
# Exit 0 iff total >= threshold (equality passes). Fail clearly on bad input.
set -euo pipefail

usage() {
  cat <<'USAGE' >&2
Usage:
  scripts/check-coverage.sh --profile <coverprofile> --threshold <pct>
  scripts/check-coverage.sh --total <pct> --threshold <pct>

Options:
  --profile PATH     Cover profile from go test -coverprofile=...
  --total PCT        Numeric total percentage (comparator-only proofs)
  --threshold PCT    Minimum accepted total (e.g. 80)
  -h, --help         Show this help
USAGE
}

profile=""
total=""
threshold=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --profile)
      [[ $# -ge 2 ]] || { echo "error: --profile requires a path" >&2; exit 2; }
      profile=$2
      shift 2
      ;;
    --total)
      [[ $# -ge 2 ]] || { echo "error: --total requires a value" >&2; exit 2; }
      total=$2
      shift 2
      ;;
    --threshold)
      [[ $# -ge 2 ]] || { echo "error: --threshold requires a value" >&2; exit 2; }
      threshold=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "error: unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
done

if [[ -z "$threshold" ]]; then
  echo "error: --threshold is required" >&2
  usage
  exit 2
fi

if [[ -n "$profile" && -n "$total" ]]; then
  echo "error: pass only one of --profile or --total" >&2
  exit 2
fi

if [[ -z "$profile" && -z "$total" ]]; then
  echo "error: one of --profile or --total is required" >&2
  usage
  exit 2
fi

is_number() {
  # Accept optional leading +/-, digits, optional fractional part.
  [[ "$1" =~ ^[+-]?[0-9]+([.][0-9]+)?$ ]]
}

if ! is_number "$threshold"; then
  echo "error: --threshold must be numeric, got: ${threshold}" >&2
  exit 2
fi

if [[ -n "$profile" ]]; then
  if [[ ! -f "$profile" ]]; then
    echo "error: cover profile not found: ${profile}" >&2
    exit 2
  fi
  if [[ ! -s "$profile" ]]; then
    echo "error: cover profile is empty: ${profile}" >&2
    exit 2
  fi
  cover_out=$(go tool cover -func="$profile") || {
    echo "error: go tool cover failed for profile: ${profile}" >&2
    exit 2
  }
  # Expect a line like: total: (statements) 80.1%
  total_line=$(printf '%s\n' "$cover_out" | awk '/^total:/ {print; found=1} END {exit !found}') || {
    echo "error: no total: line in go tool cover output for profile: ${profile}" >&2
    exit 2
  }
  total=$(printf '%s\n' "$total_line" | awk '{print $NF}' | tr -d '%')
  if [[ -z "$total" ]] || ! is_number "$total"; then
    echo "error: could not parse numeric total from: ${total_line}" >&2
    exit 2
  fi
else
  if ! is_number "$total"; then
    echo "error: --total must be numeric, got: ${total}" >&2
    exit 2
  fi
fi

printf 'coverage total: %s%%\n' "$total"
printf 'threshold: %s%%\n' "$threshold"

awk -v total="$total" -v threshold="$threshold" 'BEGIN {
  if ((total + 0) >= (threshold + 0)) {
    exit 0
  }
  printf "error: coverage %.4g%% is below threshold %.4g%%\n", total + 0, threshold + 0 > "/dev/stderr"
  exit 1
}'
